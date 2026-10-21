// Package daemon is entrypointd: state, applying revisions, the frps plugin, the API for the CLI.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/k18k/entrypoint/gateway/internal/authz"
	"github.com/k18k/entrypoint/gateway/internal/caddyctl"
	"github.com/k18k/entrypoint/gateway/internal/config"
	"github.com/k18k/entrypoint/gateway/internal/frpsapi"
	"github.com/k18k/entrypoint/gateway/internal/pki"
	"github.com/k18k/entrypoint/gateway/internal/render"
	"github.com/k18k/entrypoint/gateway/internal/spec"
	"github.com/k18k/entrypoint/gateway/internal/store"
)

type Daemon struct {
	cfg   config.Config
	log   *slog.Logger
	st    *store.Store
	authz *authz.Server
	caddy *caddyctl.Client
	frps  *frpsapi.Client
	opts  render.CaddyOptions

	mu sync.Mutex // one change or apply at a time
}

func New(cfg config.Config, log *slog.Logger) (*Daemon, error) {
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, err
	}
	d := &Daemon{
		cfg:   cfg,
		log:   log,
		st:    st,
		caddy: caddyctl.New(cfg.CaddyAdminSocket),
		frps:  frpsapi.New(cfg.FrpsAPI, cfg.FrpsAPIPasswordFile()),
		opts:  render.OptionsFrom(cfg),
	}
	d.authz = authz.New(log.With("component", "plugin"), d.frps, cfg.Limits, time.Duration(cfg.HeartbeatTimeout+5)*time.Second, d.recordLogin)
	return d, nil
}

func (d *Daemon) recordLogin(tokenID, ip, host, version string, fps []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = d.st.RecordLogin(ctx, store.Login{TokenID: tokenID, LastLogin: time.Now(), ClientIP: ip, Hostname: host, Version: version, CAFps: fps})
}

// Run runs the daemon until the context is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	defer d.st.Close()

	// 1. The plugin answers right away from the last snapshot on disk, minus revocations.
	if err := d.loadSnapshotFromDisk(ctx); err != nil {
		d.log.Warn("no authorization snapshot on disk", "err", err)
	}
	pluginLn, err := pluginListener(d.cfg.PluginListen)
	if err != nil {
		return fmt.Errorf("plugin frps: %w", err)
	}
	pluginSrv := &http.Server{Handler: d.authz.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go pluginSrv.Serve(pluginLn)
	defer pluginSrv.Close()

	// 2. The CLI API on a unix socket.
	_ = os.Remove(d.cfg.APISocket)
	apiLn, err := net.Listen("unix", d.cfg.APISocket)
	if err != nil {
		return fmt.Errorf("socket API: %w", err)
	}
	_ = os.Chmod(d.cfg.APISocket, 0o660)
	apiSrv := &http.Server{Handler: d.apiHandler(), ReadHeaderTimeout: 5 * time.Second, ConnContext: peerContext}
	go apiSrv.Serve(apiLn)
	defer apiSrv.Close()

	// 3. Initial revision and crash recovery.
	if err := d.bootstrap(ctx); err != nil {
		return err
	}
	go d.recover(ctx)
	go d.driftLoop(ctx)
	go d.renewLoop(ctx)

	d.log.Info("entrypointd running", "plugin", d.cfg.PluginListen, "api", d.cfg.APISocket)
	<-ctx.Done()
	return nil
}

// pluginListener takes the socket from systemd (entrypointd.socket) when one is passed.
// systemd holds it even while the daemon is down: frps requests queue up
// instead of failing, so a daemon restart does not drop tunnel sessions.
func pluginListener(addr string) (net.Listener, error) {
	if os.Getenv("LISTEN_PID") == strconv.Itoa(os.Getpid()) && os.Getenv("LISTEN_FDS") != "" {
		n, _ := strconv.Atoi(os.Getenv("LISTEN_FDS"))
		if n >= 1 {
			f := os.NewFile(3, "entrypointd.socket")
			ln, err := net.FileListener(f)
			f.Close()
			if err == nil {
				return ln, nil
			}
		}
	}
	return net.Listen("tcp", addr)
}

func (d *Daemon) loadSnapshotFromDisk(ctx context.Context) error {
	b, err := os.ReadFile(d.cfg.AuthSnapshot())
	if err != nil {
		return err
	}
	var snap render.AuthzSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return err
	}
	revoked, err := d.st.RevokedSet(ctx)
	if err != nil {
		return err
	}
	hosts := d.hostsForRevision(ctx, snap.Revision)
	d.authz.Swap(snap, revoked, hosts)
	d.log.Info("authorization snapshot loaded from disk", "revision", snap.Revision)
	return nil
}

func (d *Daemon) hostsForRevision(ctx context.Context, num int64) []string {
	r, err := d.st.Revision(ctx, num)
	if err != nil || r == nil {
		return nil
	}
	return hostsOf(r.Spec)
}

func hostsOf(s *spec.Spec) []string { return s.CertHosts() }

func (d *Daemon) bootstrap(ctx context.Context) error {
	head, err := d.st.Head(ctx)
	if err != nil {
		return err
	}
	if head != nil {
		return nil
	}
	_, err = d.st.Create(ctx, -1, "system", func(_ *store.Tx, _ *spec.Spec) (string, error) {
		return "initial configuration", nil
	}, nil)
	return err
}

// recover finishes an interrupted revision and reapplies the last applied one (idempotently).
func (d *Daemon) recover(ctx context.Context) {
	backoff := time.Second
	for {
		err := d.recoverOnce(ctx)
		if err == nil {
			return
		}
		d.log.Warn("restoring state after start failed, retrying", "err", err, "za", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (d *Daemon) recoverOnce(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Caddy may start slower than the daemon. Without a working admin API every apply
	// would end in a rollback, so wait (the recover loop retries).
	if _, err := d.caddy.Config(ctx); err != nil {
		return fmt.Errorf("Caddy not available yet: %w", err)
	}
	revs, err := d.st.Revisions(ctx, 1)
	if err != nil {
		return err
	}
	if len(revs) > 0 {
		latest := revs[0]
		switch latest.Status {
		case store.StatusApplying:
			if latest.Attempts >= 2 {
				d.log.Warn("revision interrupted again after a retry; rolling back", "revision", latest.Num)
				return d.rollback(ctx, latest, errors.New("interrupted twice while applying"))
			}
			d.log.Info("retrying a revision interrupted while applying", "revision", latest.Num)
			if err := d.apply(ctx, latest); err != nil {
				return nil // apply rolled back and marked it failed itself
			}
			return nil
		case store.StatusPending:
			if err := d.apply(ctx, latest); err != nil {
				return nil
			}
			return nil
		}
	}
	last, err := d.st.LastApplied(ctx)
	if err != nil {
		return err
	}
	if last == nil {
		// No revision was ever applied (e.g. the first start failed):
		// make a new revision from the current configuration.
		rev, err := d.st.Create(ctx, -1, "system", func(_ *store.Tx, _ *spec.Spec) (string, error) {
			return "reapplying the configuration after a failed start", nil
		}, nil)
		if err != nil {
			return err
		}
		return d.apply(ctx, rev)
	}
	return d.applyComponents(ctx, last)
}

// applyComponents applies a revision to all components without changing its status.
func (d *Daemon) applyComponents(ctx context.Context, rev *store.Revision) error {
	caddyJSON, err := render.Caddy(rev.Spec, d.opts)
	if err != nil {
		return err
	}
	snap := render.Authz(rev.Num, rev.Spec)
	if err := d.writeSnapshot(ctx, snap, rev.Spec); err != nil {
		return err
	}
	if err := d.applyCaddy(ctx, caddyJSON); err != nil {
		return err
	}
	return d.markComponents(ctx, rev.Num, caddyJSON, snap)
}

func (d *Daemon) markComponents(ctx context.Context, num int64, caddyJSON []byte, snap render.AuthzSnapshot) error {
	ch, _ := render.Hash(caddyJSON)
	sb, _ := json.Marshal(snap)
	sh, _ := render.Hash(sb)
	if err := d.st.SetComponent(ctx, "authz", num, sh); err != nil {
		return err
	}
	return d.st.SetComponent(ctx, "caddy", num, ch)
}

func (d *Daemon) writeSnapshot(ctx context.Context, snap render.AuthzSnapshot, s *spec.Spec) error {
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := pki.WriteFileAtomic(d.cfg.AuthSnapshot(), b, 0o600); err != nil {
		return fmt.Errorf("writing the authorization snapshot: %w", err)
	}
	revoked, err := d.st.RevokedSet(ctx)
	if err != nil {
		return err
	}
	d.authz.Swap(snap, revoked, hostsOf(s))
	return nil
}

func (d *Daemon) applyCaddy(ctx context.Context, caddyJSON []byte) error {
	if err := d.caddy.Load(ctx, caddyJSON); err != nil {
		return err
	}
	// The same content goes to the startup file, so Caddy comes up without entrypointd.
	if err := pki.WriteFileAtomic(d.cfg.CaddyConfig(), caddyJSON, 0o640); err != nil {
		return fmt.Errorf("writing the Caddy startup file: %w", err)
	}
	return nil
}

// apply applies a revision following the steps in section 4 of the specification.
func (d *Daemon) apply(ctx context.Context, rev *store.Revision) error {
	if err := d.st.SetStatus(ctx, rev.Num, store.StatusApplying, ""); err != nil {
		return err
	}
	caddyJSON, err := render.Caddy(rev.Spec, d.opts)
	if err != nil {
		return d.rollback(ctx, rev, err)
	}
	snap := render.Authz(rev.Num, rev.Spec)
	if err := d.writeSnapshot(ctx, snap, rev.Spec); err != nil {
		return d.rollback(ctx, rev, err)
	}
	if delay := d.cfg.Testing.ApplyDelay.Duration; delay > 0 {
		d.log.Warn("test mode: pause after writing the snapshot", "revision", rev.Num, "pause", delay)
		time.Sleep(delay)
	}
	if err := d.applyCaddy(ctx, caddyJSON); err != nil {
		return d.rollback(ctx, rev, err)
	}
	if err := d.st.SetStatus(ctx, rev.Num, store.StatusApplied, ""); err != nil {
		return err
	}
	if err := d.markComponents(ctx, rev.Num, caddyJSON, snap); err != nil {
		return err
	}
	d.log.Info("revision applied", "revision", rev.Num, "summary", rev.Summary)
	return nil
}

// rollback restores the last applied revision in the snapshot (minus revocations) and in Caddy.
func (d *Daemon) rollback(ctx context.Context, rev *store.Revision, cause error) error {
	d.log.Error("applying the revision failed, rolling back", "revision", rev.Num, "err", cause)
	_ = d.st.SetStatus(ctx, rev.Num, store.StatusFailed, cause.Error())
	last, err := d.st.LastApplied(ctx)
	if err != nil || last == nil {
		// Nothing was applied before: authorize nothing rather than keep the failed revision (Strix vuln-0009).
		empty := &spec.Spec{}
		if werr := d.writeSnapshot(ctx, render.Authz(0, empty), empty); werr != nil {
			d.log.Error("restoring an empty snapshot failed", "err", werr)
		}
		return fmt.Errorf("%v (no revision to restore)", cause)
	}
	snap := render.Authz(last.Num, last.Spec)
	if err := d.writeSnapshot(ctx, snap, last.Spec); err != nil {
		d.log.Error("restoring the snapshot failed", "err", err)
	}
	// On a /load error Caddy keeps its previous configuration; if /load succeeded
	// but writing the file failed, restore Caddy too.
	if caddyJSON, err := render.Caddy(last.Spec, d.opts); err == nil {
		if cur, err := d.caddy.Config(ctx); err == nil {
			h1, _ := render.Hash(cur)
			h2, _ := render.Hash(caddyJSON)
			if h1 != h2 {
				_ = d.applyCaddy(ctx, caddyJSON)
			}
		}
	}
	return cause
}

// validate checks the configuration before the revision is saved.
func (d *Daemon) validate(s *spec.Spec) error {
	if err := s.Validate(d.cfg.PortPool.Start, d.cfg.PortPool.End); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "ep-validate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	o := d.opts
	o.StorageDir = filepath.Join(tmp, "storage")
	o.AdminSocket = filepath.Join(tmp, "admin.sock")
	cfg, err := render.Caddy(s, o)
	if err != nil {
		return err
	}
	f := filepath.Join(tmp, "caddy.json")
	if err := os.WriteFile(f, cfg, 0o600); err != nil {
		return err
	}
	if _, err := os.Stat(d.cfg.CaddyBin); err != nil {
		return nil // no binary (unit tests): Caddy validation skipped
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.cfg.CaddyBin, "validate", "--config", f)
	cmd.Env = []string{"HOME=" + tmp, "XDG_DATA_HOME=" + tmp, "XDG_CONFIG_HOME=" + tmp}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Caddy rejected the rendered configuration: %s", lastLine(out))
	}
	return nil
}

func lastLine(b []byte) string {
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '\n' {
			return s[i+1:]
		}
	}
	return s
}

// change creates a revision and applies it.
func (d *Daemon) change(ctx context.Context, baseRev *int64, author string, mut store.Mutation) (*store.Revision, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	base := int64(-1)
	if baseRev != nil {
		base = *baseRev
	}
	rev, err := d.st.Create(ctx, base, author, mut, d.validate)
	if err != nil {
		return nil, err
	}
	applyErr := d.apply(ctx, rev)
	final, err := d.st.Revision(ctx, rev.Num)
	if err != nil {
		return nil, err
	}
	return final, applyErr
}

// driftLoop compares Caddy's configuration with the desired one and restores it on a mismatch.
func (d *Daemon) driftLoop(ctx context.Context) {
	t := time.NewTicker(d.cfg.DriftInterval.Duration)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.checkDrift(ctx)
		}
	}
}

func (d *Daemon) checkDrift(ctx context.Context) {
	if !d.mu.TryLock() {
		return
	}
	defer d.mu.Unlock()
	last, err := d.st.LastApplied(ctx)
	if err != nil || last == nil {
		return
	}
	want, err := render.Caddy(last.Spec, d.opts)
	if err != nil {
		return
	}
	cur, err := d.caddy.Config(ctx)
	if err != nil {
		return // Caddy is down; it loads the startup file when it starts
	}
	h1, _ := render.Hash(cur)
	h2, _ := render.Hash(want)
	if h1 == h2 {
		return
	}
	d.log.Warn("Caddy configuration drift detected; reapplying", "revision", last.Num)
	if err := d.applyCaddy(ctx, want); err != nil {
		d.log.Error("reapplying after drift failed", "err", err)
		return
	}
	_ = d.markComponents(ctx, last.Num, want, render.Authz(last.Num, last.Spec))
}
