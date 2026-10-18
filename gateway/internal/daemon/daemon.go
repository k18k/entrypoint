// Pakiet daemon to entrypointd: stan, stosowanie rewizji, plugin frps, API dla CLI.
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

	mu sync.Mutex // jedna zmiana lub stosowanie naraz
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
	d.authz = authz.New(log.With("komponent", "plugin"), d.frps, cfg.Limits.FailedLoginsPerMinute, cfg.Limits.OnDemandCertsPerHour, d.recordLogin)
	return d, nil
}

func (d *Daemon) recordLogin(tokenID, ip, host, version string, fps []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = d.st.RecordLogin(ctx, store.Login{TokenID: tokenID, LastLogin: time.Now(), ClientIP: ip, Hostname: host, Version: version, CAFps: fps})
}

// Run uruchamia demona do czasu anulowania kontekstu.
func (d *Daemon) Run(ctx context.Context) error {
	defer d.st.Close()

	// 1. Plugin od razu odpowiada z ostatniego snapshotu na dysku, z odjętymi odwołaniami.
	if err := d.loadSnapshotFromDisk(ctx); err != nil {
		d.log.Warn("brak snapshotu autoryzacji na dysku", "err", err)
	}
	pluginLn, err := pluginListener(d.cfg.PluginListen)
	if err != nil {
		return fmt.Errorf("plugin frps: %w", err)
	}
	pluginSrv := &http.Server{Handler: d.authz.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go pluginSrv.Serve(pluginLn)
	defer pluginSrv.Close()

	// 2. API dla CLI na sockecie unixowym.
	_ = os.Remove(d.cfg.APISocket)
	apiLn, err := net.Listen("unix", d.cfg.APISocket)
	if err != nil {
		return fmt.Errorf("socket API: %w", err)
	}
	_ = os.Chmod(d.cfg.APISocket, 0o660)
	apiSrv := &http.Server{Handler: d.apiHandler(), ReadHeaderTimeout: 5 * time.Second, ConnContext: peerContext}
	go apiSrv.Serve(apiLn)
	defer apiSrv.Close()

	// 3. Rewizja początkowa i odtworzenie po awarii.
	if err := d.bootstrap(ctx); err != nil {
		return err
	}
	go d.recover(ctx)
	go d.driftLoop(ctx)
	go d.renewLoop(ctx)

	d.log.Info("entrypointd działa", "plugin", d.cfg.PluginListen, "api", d.cfg.APISocket)
	<-ctx.Done()
	return nil
}

// pluginListener bierze gniazdo od systemd (entrypointd.socket), jeśli zostało przekazane.
// systemd trzyma je także wtedy, gdy demon nie działa: zapytania frps czekają w kolejce
// zamiast kończyć się błędem, więc restart demona nie zrywa sesji tuneli.
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
	d.log.Info("snapshot autoryzacji wczytany z dysku", "rewizja", snap.Revision)
	return nil
}

func (d *Daemon) hostsForRevision(ctx context.Context, num int64) []string {
	r, err := d.st.Revision(ctx, num)
	if err != nil || r == nil {
		return nil
	}
	return hostsOf(r.Spec)
}

func hostsOf(s *spec.Spec) []string {
	var hs []string
	seen := map[string]bool{}
	for _, r := range s.Routes {
		if !seen[r.Host] {
			seen[r.Host] = true
			hs = append(hs, r.Host)
		}
	}
	return hs
}

func (d *Daemon) bootstrap(ctx context.Context) error {
	head, err := d.st.Head(ctx)
	if err != nil {
		return err
	}
	if head != nil {
		return nil
	}
	_, err = d.st.Create(ctx, -1, "system", func(_ *store.Tx, _ *spec.Spec) (string, error) {
		return "konfiguracja początkowa", nil
	}, nil)
	return err
}

// recover dokańcza przerwaną rewizję i ponownie stosuje ostatnią zastosowaną (idempotentnie).
func (d *Daemon) recover(ctx context.Context) {
	backoff := time.Second
	for {
		err := d.recoverOnce(ctx)
		if err == nil {
			return
		}
		d.log.Warn("odtwarzanie stanu po starcie nie powiodło się, ponawiam", "err", err, "za", backoff)
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
	// Caddy może startować wolniej niż demon. Bez działającego admin API każda próba
	// stosowania zakończyłaby się wycofaniem, więc czekamy (pętla recover ponawia).
	if _, err := d.caddy.Config(ctx); err != nil {
		return fmt.Errorf("Caddy jeszcze niedostępny: %w", err)
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
				d.log.Warn("rewizja przerwana po ponowieniu; wycofuję", "rewizja", latest.Num)
				return d.rollback(ctx, latest, errors.New("przerwana dwukrotnie w trakcie stosowania"))
			}
			d.log.Info("ponawiam rewizję przerwaną w trakcie stosowania", "rewizja", latest.Num)
			if err := d.apply(ctx, latest); err != nil {
				return nil // apply sam wycofał i oznaczył failed
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
		// Żadna rewizja nie została zastosowana (np. pierwszy start przerwany błędem):
		// nowa rewizja z bieżącą konfiguracją.
		rev, err := d.st.Create(ctx, -1, "system", func(_ *store.Tx, _ *spec.Spec) (string, error) {
			return "ponowne zastosowanie konfiguracji po nieudanym starcie", nil
		}, nil)
		if err != nil {
			return err
		}
		return d.apply(ctx, rev)
	}
	return d.applyComponents(ctx, last)
}

// applyComponents stosuje rewizję do wszystkich komponentów bez zmiany jej statusu.
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
		return fmt.Errorf("zapis snapshotu autoryzacji: %w", err)
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
	// Ta sama treść trafia do pliku startowego, żeby Caddy wstał bez entrypointd.
	if err := pki.WriteFileAtomic(d.cfg.CaddyConfig(), caddyJSON, 0o640); err != nil {
		return fmt.Errorf("zapis pliku startowego Caddy: %w", err)
	}
	return nil
}

// apply stosuje rewizję według kroków z sekcji 4 specyfikacji.
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
		d.log.Warn("tryb testowy: pauza po zapisie snapshotu", "rewizja", rev.Num, "pauza", delay)
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
	d.log.Info("rewizja zastosowana", "rewizja", rev.Num, "opis", rev.Summary)
	return nil
}

// rollback przywraca ostatnią zastosowaną rewizję w snapshocie (z odjętymi odwołaniami) i w Caddy.
func (d *Daemon) rollback(ctx context.Context, rev *store.Revision, cause error) error {
	d.log.Error("stosowanie rewizji nie powiodło się, wycofuję", "rewizja", rev.Num, "err", cause)
	_ = d.st.SetStatus(ctx, rev.Num, store.StatusFailed, cause.Error())
	last, err := d.st.LastApplied(ctx)
	if err != nil || last == nil {
		return fmt.Errorf("%v (brak rewizji do przywrócenia)", cause)
	}
	snap := render.Authz(last.Num, last.Spec)
	if err := d.writeSnapshot(ctx, snap, last.Spec); err != nil {
		d.log.Error("przywrócenie snapshotu nie powiodło się", "err", err)
	}
	// Caddy przy błędzie /load zostaje przy poprzedniej konfiguracji; jeśli /load się udał,
	// a zawiódł zapis pliku, przywracamy też Caddy.
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

// validate sprawdza konfigurację przed zapisem rewizji.
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
		return nil // brak binarki (testy jednostkowe) — walidacja Caddy pominięta
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.cfg.CaddyBin, "validate", "--config", f)
	cmd.Env = []string{"HOME=" + tmp, "XDG_DATA_HOME=" + tmp, "XDG_CONFIG_HOME=" + tmp}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Caddy odrzucił wyrenderowaną konfigurację: %s", lastLine(out))
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

// change tworzy rewizję i ją stosuje.
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

// driftLoop porównuje konfigurację Caddy z oczekiwaną i przywraca ją przy rozbieżności.
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
		return // Caddy nie działa; po starcie wczyta plik startowy
	}
	h1, _ := render.Hash(cur)
	h2, _ := render.Hash(want)
	if h1 == h2 {
		return
	}
	d.log.Warn("wykryto dryf konfiguracji Caddy; stosuję ponownie", "rewizja", last.Num)
	if err := d.applyCaddy(ctx, want); err != nil {
		d.log.Error("ponowne zastosowanie po dryfie nie powiodło się", "err", err)
		return
	}
	_ = d.markComponents(ctx, last.Num, want, render.Authz(last.Num, last.Spec))
}
