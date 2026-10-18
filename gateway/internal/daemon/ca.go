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

// Stany rotacji CA (meta "ca_rotation").
const (
	rotNone     = ""
	rotStaged   = "staged"   // nowe CA istnieje; pakiety zawierają oba CA; serwer na starym
	rotSwitched = "switched" // serwer na nowym CA; pakiety nadal zawierają oba
)

func (d *Daemon) rotationState(ctx context.Context) string {
	v, _ := d.st.Meta(ctx, "ca_rotation")
	return v
}

func readPEM(path string) ([]byte, bool) {
	b, err := os.ReadFile(path)
	return b, err == nil
}

// bundleCAs zwraca listę CA do pakietów połączenia, zależnie od etapu rotacji.
func (d *Daemon) bundleCAs() ([]string, error) {
	cur, ok := readPEM(d.cfg.CACert())
	if !ok {
		return nil, errors.New("brak certyfikatu CA")
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
	// Klienci, których ostatnie logowanie nie zna CA, na które serwer przechodzi (albo już przeszedł).
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
			fail(w, 409, fmt.Errorf("rotacja już trwa (etap %s)", state))
			return
		}
		err = d.rotateStart(ctx)
	case "switch":
		if state != rotStaged {
			fail(w, 409, errors.New("przełączenie wymaga etapu start"))
			return
		}
		err = d.rotateSwitch(ctx)
	case "finish":
		if state != rotSwitched {
			fail(w, 409, errors.New("zakończenie wymaga etapu switch"))
			return
		}
		err = d.rotateFinish(ctx)
	default:
		fail(w, 400, errors.New("etap rotacji: start, switch albo finish"))
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
	d.log.Warn("rotacja CA: nowe CA utworzone; nowe pakiety połączenia zawierają oba CA")
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
	d.log.Warn("rotacja CA: serwer tunelu przechodzi na nowe CA; klienci tylko ze starym CA zostaną odrzuceni")
	return d.installServerCert(certPEM, keyPEM)
}

func mustRead(p string) []byte {
	b, _ := os.ReadFile(p)
	return b
}

func (d *Daemon) rotateFinish(ctx context.Context) error {
	_ = os.Remove(d.cfg.CAOldCert())
	d.log.Info("rotacja CA zakończona; stare CA usunięte z pakietów")
	return d.st.SetMeta(ctx, "ca_rotation", rotNone)
}

// installServerCert zapisuje certyfikat portu tunelu i wyzwala restart frps (jednostka systemd .path).
func (d *Daemon) installServerCert(certPEM, keyPEM []byte) error {
	if err := pki.WriteFileAtomic(d.cfg.TunnelKey(), keyPEM, 0o640); err != nil {
		return err
	}
	if err := pki.WriteFileAtomic(d.cfg.TunnelCert(), certPEM, 0o644); err != nil {
		return err
	}
	return os.WriteFile(d.cfg.TunnelReloadTrigger(), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
}

// renewLoop odnawia certyfikat portu tunelu 30 dni przed wygaśnięciem.
func (d *Daemon) renewLoop(ctx context.Context) {
	check := func() {
		cur, err := os.ReadFile(d.cfg.TunnelCert())
		if err != nil {
			d.log.Error("brak certyfikatu portu tunelu", "err", err)
			return
		}
		need, _ := pki.NeedsRenewal(cur, time.Now())
		ca, err := pki.LoadCA(d.cfg.CACert(), d.cfg.CAKey())
		if err != nil {
			d.log.Error("nie można wczytać CA", "err", err)
			return
		}
		if !need && pki.IssuedBy(cur, ca.Cert) {
			return
		}
		certPEM, keyPEM, err := ca.IssueServer(d.cfg.GatewayName)
		if err != nil {
			d.log.Error("odnowienie certyfikatu portu tunelu nie powiodło się", "err", err)
			return
		}
		if err := d.installServerCert(certPEM, keyPEM); err != nil {
			d.log.Error("zapis certyfikatu portu tunelu nie powiódł się", "err", err)
			return
		}
		d.log.Warn("certyfikat portu tunelu odnowiony; frps zostanie zrestartowany")
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
