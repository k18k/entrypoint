package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/k18k/entrypoint/gateway/internal/api"
	"github.com/k18k/entrypoint/gateway/internal/pki"
)

// CA rotation states (meta "ca_rotation").
const (
	rotNone     = ""
	rotStaged   = "staged"   // the new CA exists; bundles contain both CAs; server on the old one
	rotSwitched = "switched" // server on the new CA; bundles still contain both
)

func (d *Daemon) rotationState(ctx context.Context) string {
	v, _ := d.st.Meta(ctx, "ca_rotation")
	return v
}

func readPEM(path string) ([]byte, bool) {
	b, err := os.ReadFile(path)
	return b, err == nil
}

// bundleCAs returns the CA list for connection bundles, depending on the rotation step.
func (d *Daemon) bundleCAs() ([]string, error) {
	cur, ok := readPEM(d.cfg.CACert())
	if !ok {
		return nil, errors.New("no CA certificate")
	}
	out := []string{string(cur)}
	switch d.rotationState(context.Background()) {
	case rotStaged:
		if next, ok := readPEM(d.cfg.CANextCert()); ok {
			out = append(out, string(next))
		}
	case rotSwitched:
		if old, ok := readPEM(d.cfg.CAOldCert()); ok {
			out = append(out, string(old))
		}
	}
	return out, nil
}

func certInfo(p []byte) api.CertInfo {
	c, err := pki.ParseCert(p)
	if err != nil {
		return api.CertInfo{}
	}
	return api.CertInfo{Fingerprint: pki.Fingerprint(c), Subject: c.Subject.CommonName, NotAfter: c.NotAfter, Issuer: c.Issuer.CommonName}
}

func (d *Daemon) hCA(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	info := api.CAInfo{State: d.rotationState(ctx)}
	cur, _ := readPEM(d.cfg.CACert())
	info.Current = certInfo(cur)
	if p, ok := readPEM(d.cfg.CANextCert()); ok {
		ci := certInfo(p)
		info.Next = &ci
	}
	if p, ok := readPEM(d.cfg.CAOldCert()); ok {
		ci := certInfo(p)
		info.Old = &ci
	}
	srv, _ := readPEM(d.cfg.TunnelCert())
	info.Server = certInfo(srv)
	cas, _ := d.bundleCAs()
	for _, c := range cas {
		info.BundleCAs = append(info.BundleCAs, certInfo([]byte(c)).Fingerprint)
	}
	// Clients whose last login does not know the CA the server is moving (or has moved) to.
	target := ""
	switch info.State {
	case rotStaged:
		if info.Next != nil {
			target = info.Next.Fingerprint
		}
	case rotSwitched:
		target = info.Current.Fingerprint
	}
	info.StaleClients = []api.TokenInfo{}
	if target != "" {
		toks, _ := d.tokenInfos(ctx)
		for _, t := range toks {
			if t.Revoked || t.LastLogin == nil {
				continue
			}
			has := false
			for _, fp := range t.LastLogin.CAFps {
				if fp == target {
					has = true
				}
			}
			if !has {
				info.StaleClients = append(info.StaleClients, t)
			}
		}
	}
	reply(w, 200, info)
}

func (d *Daemon) hCARotate(w http.ResponseWriter, r *http.Request) {
	var req api.CARotate
	if !decode(w, r, &req) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	ctx := r.Context()
	state := d.rotationState(ctx)
	var err error
	switch req.Step {
	case "start":
		if state != rotNone {
			fail(w, 409, fmt.Errorf("a rotation is already in progress (step %s)", state))
			return
		}
		err = d.rotateStart(ctx)
	case "switch":
		if state != rotStaged {
			fail(w, 409, errors.New("switch requires the start step"))
			return
		}
		err = d.rotateSwitch(ctx)
	case "finish":
		if state != rotSwitched {
			fail(w, 409, errors.New("finish requires the switch step"))
			return
		}
		err = d.rotateFinish(ctx)
	default:
		fail(w, 400, errors.New("rotation step: start, switch or finish"))
		return
	}
	if err != nil {
		fail(w, 500, err)
		return
	}
	d.hCA(w, r)
}

func (d *Daemon) rotateStart(ctx context.Context) error {
	ca, keyPEM, err := pki.NewCA("entrypoint CA " + d.cfg.GatewayName + " " + time.Now().UTC().Format("2006-01"))
	if err != nil {
		return err
	}
	if err := pki.WriteFileAtomic(d.cfg.CANextKey(), keyPEM, 0o600); err != nil {
		return err
	}
	if err := pki.WriteFileAtomic(d.cfg.CANextCert(), ca.CertPEM, 0o644); err != nil {
		return err
	}
	d.log.Warn("CA rotation: new CA created; new connection bundles contain both CAs")
	return d.st.SetMeta(ctx, "ca_rotation", rotStaged)
}

func (d *Daemon) rotateSwitch(ctx context.Context) error {
	next, err := pki.LoadCA(d.cfg.CANextCert(), d.cfg.CANextKey())
	if err != nil {
		return err
	}
	certPEM, keyPEM, err := next.IssueServer(d.cfg.GatewayName)
	if err != nil {
		return err
	}
	cur, err := os.ReadFile(d.cfg.CACert())
	if err != nil {
		return err
	}
	curKey, err := os.ReadFile(d.cfg.CAKey())
	if err != nil {
		return err
	}
	if err := pki.WriteFileAtomic(d.cfg.CAOldCert(), cur, 0o644); err != nil {
		return err
	}
	if err := pki.WriteFileAtomic(d.cfg.CAKey(), mustRead(d.cfg.CANextKey()), 0o600); err != nil {
		return err
	}
	if err := pki.WriteFileAtomic(d.cfg.CACert(), next.CertPEM, 0o644); err != nil {
		_ = pki.WriteFileAtomic(d.cfg.CAKey(), curKey, 0o600)
		return err
	}
	_ = os.Remove(d.cfg.CANextCert())
	_ = os.Remove(d.cfg.CANextKey())
	if err := d.st.SetMeta(ctx, "ca_rotation", rotSwitched); err != nil {
		return err
	}
	d.log.Warn("CA rotation: the tunnel server moves to the new CA; clients with only the old CA will be rejected")
	return d.installServerCert(certPEM, keyPEM)
}

func mustRead(p string) []byte {
	b, _ := os.ReadFile(p)
	return b
}

func (d *Daemon) rotateFinish(ctx context.Context) error {
	_ = os.Remove(d.cfg.CAOldCert())
	d.log.Info("CA rotation finished; old CA removed from bundles")
	return d.st.SetMeta(ctx, "ca_rotation", rotNone)
}

// installServerCert writes the tunnel port certificate and triggers an frps restart (systemd .path unit).
func (d *Daemon) installServerCert(certPEM, keyPEM []byte) error {
	if err := pki.WriteFileAtomic(d.cfg.TunnelKey(), keyPEM, 0o640); err != nil {
		return err
	}
	if err := pki.WriteFileAtomic(d.cfg.TunnelCert(), certPEM, 0o644); err != nil {
		return err
	}
	return os.WriteFile(d.cfg.TunnelReloadTrigger(), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
}

// renewLoop renews the tunnel port certificate 30 days before it expires.
func (d *Daemon) renewLoop(ctx context.Context) {
	check := func() {
		cur, err := os.ReadFile(d.cfg.TunnelCert())
		if err != nil {
			d.log.Error("no tunnel port certificate", "err", err)
			return
		}
		need, _ := pki.NeedsRenewal(cur, time.Now())
		ca, err := pki.LoadCA(d.cfg.CACert(), d.cfg.CAKey())
		if err != nil {
			d.log.Error("cannot load the CA", "err", err)
			return
		}
		if !need && pki.IssuedBy(cur, ca.Cert) {
			return
		}
		certPEM, keyPEM, err := ca.IssueServer(d.cfg.GatewayName)
		if err != nil {
			d.log.Error("renewing the tunnel port certificate failed", "err", err)
			return
		}
		if err := d.installServerCert(certPEM, keyPEM); err != nil {
			d.log.Error("writing the tunnel port certificate failed", "err", err)
			return
		}
		d.log.Warn("tunnel port certificate renewed; frps will be restarted")
	}
	check()
	t := time.NewTicker(12 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}
