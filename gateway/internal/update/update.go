// Package update replaces the installed version with a new release (spec section 13):
// maintenance mode, a signed download, a database copy, forward-only migrations, switching the
// `current` symlink, restarting in order and a health check, with an automatic rollback.
package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/k18k/entrypoint/gateway/internal/api"
	"github.com/k18k/entrypoint/gateway/internal/config"
	"github.com/k18k/entrypoint/gateway/internal/install"
	"github.com/k18k/entrypoint/gateway/internal/maintenance"
	"github.com/k18k/entrypoint/gateway/internal/release"
	"github.com/k18k/entrypoint/gateway/internal/store"
	"github.com/k18k/entrypoint/gateway/internal/version"
)

const repo = "k18k/entrypoint"

// Options of an update.
type Options struct {
	Config     config.Config
	ConfigPath string
	From       string        // a directory with the release files instead of GitHub (tests, offline)
	Version    string        // a version to install instead of the latest release
	HealthTime time.Duration // how long the new version must stay healthy
	TestPause  time.Duration // tests only: a pause after maintenance mode starts
	Out        io.Writer
}

var tarballRe = regexp.MustCompile(`^entrypoint_(.+)_linux_` + runtime.GOARCH + `\.tar\.gz$`)

// Run updates the gateway. On any failure after the switch it rolls back; the error says what happened.
func Run(ctx context.Context, o Options) error {
	if os.Geteuid() != 0 {
		return errors.New("update requires root (sudo)")
	}
	cfg := o.Config
	if m := maintenance.Get(cfg); m != nil {
		return fmt.Errorf("maintenance mode is already on (%s); end it first: sudo entrypoint maintenance off", m.Reason)
	}
	tmp, err := os.MkdirTemp(filepath.Join(config.OptDir, "releases"), ".update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	// Download and verify before touching anything.
	files, err := fetch(ctx, o, tmp)
	if err != nil {
		return err
	}
	sums, err := release.VerifySums(files["SHA256SUMS"], files["SHA256SUMS.sig"], cfg.Testing.ReleaseKeys)
	if err != nil {
		return err
	}
	var tarball, newVersion string
	for name := range sums {
		if m := tarballRe.FindStringSubmatch(name); m != nil {
			tarball, newVersion = name, m[1]
		}
	}
	if tarball == "" {
		return fmt.Errorf("the release has no gateway for linux_%s", runtime.GOARCH)
	}
	if newVersion == version.Version {
		return fmt.Errorf("version %s is already installed", newVersion)
	}
	tarPath := filepath.Join(tmp, tarball)
	if err := download(ctx, o, tarball, tarPath); err != nil {
		return err
	}
	if err := release.CheckFile(tarPath, sums[tarball]); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "Release %s verified (signature and SHA256).\n", newVersion)

	// 1. Maintenance mode: changes are refused from here on, revocations are journaled.
	if err := maintenance.Enable(cfg, fmt.Sprintf("update %s -> %s", version.Version, newVersion)); err != nil {
		return err
	}
	fmt.Fprintln(o.Out, "Maintenance mode on: configuration changes are blocked, token revocations still work.")
	time.Sleep(o.TestPause)

	// 2. Unpack into releases/<version>.
	relDir := filepath.Join(config.OptDir, "releases", newVersion)
	staging := filepath.Join(tmp, "release")
	if err := release.Extract(tarPath, staging); err != nil {
		return endMaintenance(cfg, err)
	}
	_ = os.RemoveAll(relDir)
	if err := os.Rename(staging, relDir); err != nil {
		return endMaintenance(cfg, err)
	}

	current := filepath.Join(config.OptDir, "current")
	oldTarget, err := os.Readlink(current)
	if err != nil {
		return endMaintenance(cfg, err)
	}

	// 3. Stop entrypointd (its socket keeps frps requests queued), then copy the database.
	if err := systemctl("stop", "entrypointd.service"); err != nil {
		return endMaintenance(cfg, err)
	}
	backupDB := filepath.Join(cfg.DataDir, "db", "pre-update.db")
	_ = os.Remove(backupDB)
	if err := snapshot(ctx, cfg, backupDB); err != nil {
		_ = systemctl("start", "entrypointd.service")
		return endMaintenance(cfg, fmt.Errorf("database copy: %w", err))
	}

	fail := func(step string, err error) error {
		fmt.Fprintf(o.Out, "Update failed (%s): %v\nRolling back to %s...\n", step, err, version.Version)
		if rerr := rollback(ctx, cfg, o, oldTarget, backupDB); rerr != nil {
			return fmt.Errorf("%s: %v; the rollback failed too: %v (the database copy is %s)", step, err, rerr, backupDB)
		}
		return fmt.Errorf("%s: %v; rolled back to %s", step, err, version.Version)
	}

	// 4. Forward-only migrations with the new version, as the daemon's user.
	if out, err := runAs(config.UserDaemon, filepath.Join(relDir, "bin", "entrypointd"), "--migrate", "--config", o.ConfigPath); err != nil {
		return fail("migration", fmt.Errorf("%v: %s", err, strings.TrimSpace(out)))
	}

	// 5. Switch `current`.
	if err := switchCurrent(current, relDir); err != nil {
		return fail("switch", err)
	}

	// 6. Restart in order with the new version's units (entrypoint install keeps all data).
	if out, err := reinstall(ctx, filepath.Join(current, "bin", "entrypoint"), o.ConfigPath); err != nil {
		return fail("restart", fmt.Errorf("%v: %s", err, lastLines(string(out), 5)))
	}

	// 7. Health check.
	fmt.Fprintf(o.Out, "Version %s started; checking its health for %s...\n", newVersion, o.HealthTime)
	if err := healthCheck(ctx, cfg, o.HealthTime); err != nil {
		return fail("health check", err)
	}

	// Done: the revocations are in the new database already.
	_ = os.Remove(backupDB)
	if err := maintenance.Disable(cfg); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "Updated to %s. Maintenance mode off. The previous version stays in %s.\n", newVersion, oldTarget)
	return nil
}

func endMaintenance(cfg config.Config, err error) error {
	if derr := maintenance.Disable(cfg); derr != nil {
		return fmt.Errorf("%v (and maintenance mode is still on: %v)", err, derr)
	}
	return err
}

// rollback (step 8): the previous symlink and database, plus the revocations journaled meanwhile,
// applied before anything accepts logins again.
func rollback(ctx context.Context, cfg config.Config, o Options, oldTarget, backupDB string) error {
	_ = systemctl("stop", "entrypointd.service", "entrypoint-frps.service")
	// A crash-looping new version may have hit systemd's start limit, which would refuse the restart.
	_ = systemctl("reset-failed")
	if err := switchCurrent(filepath.Join(config.OptDir, "current"), oldTarget); err != nil {
		return err
	}
	db := cfg.DBPath()
	for _, f := range []string{db + "-wal", db + "-shm"} {
		_ = os.Remove(f)
	}
	if err := copyFile(backupDB, db); err != nil {
		return err
	}
	revs, err := maintenance.Revocations(cfg)
	if err != nil {
		return err
	}
	st, err := store.Open(db)
	if err != nil {
		return err
	}
	for _, r := range revs {
		if err := st.Revoke(ctx, r.TokenID, r.Reason+" (kept through an update rollback)"); err != nil {
			st.Close()
			return err
		}
	}
	st.Close()
	if err := chownDaemon(db, db+"-wal", db+"-shm"); err != nil {
		return err
	}
	if out, err := reinstall(ctx, filepath.Join(config.OptDir, "current", "bin", "entrypoint"), o.ConfigPath); err != nil {
		return fmt.Errorf("restarting %s: %v: %s", version.Version, err, lastLines(string(out), 5))
	}
	if err := maintenance.Disable(cfg); err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "Rolled back to %s; %d revocation(s) from the update kept. Maintenance mode off.\n", version.Version, len(revs))
	return nil
}

// healthCheck requires the services to stay up for d: every unit active, entrypointd answering with
// Caddy and frps reachable, and no unit restarted by systemd meanwhile.
func healthCheck(ctx context.Context, cfg config.Config, d time.Duration) error {
	units := []string{"entrypointd.service", "entrypoint-caddy.service", "entrypoint-frps.service"}
	if cfg.VPN.Enabled() {
		units = append(units, install.VPNUnits...)
	}
	if cfg.Panel.Enabled() {
		units = append(units, install.PanelUnit)
	}
	restarts := func() map[string]string {
		m := map[string]string{}
		for _, u := range units {
			out, _ := exec.Command("systemctl", "show", "-p", "NRestarts", "--value", u).Output()
			m[u] = strings.TrimSpace(string(out))
		}
		return m
	}
	base := restarts()
	c := api.NewClient(cfg.APISocket)
	deadline := time.Now().Add(d)
	var last error
	for {
		last = nil
		if err := exec.Command("systemctl", append([]string{"is-active", "--quiet"}, units...)...).Run(); err != nil {
			last = errors.New("not all services are active")
		} else {
			cx, cancel := context.WithTimeout(ctx, 5*time.Second)
			var st api.Status
			err := c.Do(cx, "GET", "/v1/status", nil, &st)
			cancel()
			switch {
			case err != nil:
				last = fmt.Errorf("entrypointd: %v", err)
			case !st.CaddyOK || !st.FrpsOK:
				last = fmt.Errorf("Caddy ok %v, frps ok %v", st.CaddyOK, st.FrpsOK)
			}
		}
		for u, n := range restarts() {
			if n != base[u] {
				return fmt.Errorf("%s restarted (crashed) during the check", u)
			}
		}
		if time.Now().After(deadline) {
			return last
		}
		time.Sleep(time.Second)
	}
}

// fetch reads SHA256SUMS and its signature from the release.
func fetch(ctx context.Context, o Options, tmp string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, name := range []string{"SHA256SUMS", "SHA256SUMS.sig"} {
		p := filepath.Join(tmp, name)
		if err := download(ctx, o, name, p); err != nil {
			return nil, err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		out[name] = b
	}
	return out, nil
}

func download(ctx context.Context, o Options, name, dest string) error {
	if o.From != "" {
		return copyFile(filepath.Join(o.From, filepath.Base(name)), dest)
	}
	tag := "latest/download"
	if o.Version != "" {
		tag = "download/v" + strings.TrimPrefix(o.Version, "v")
	}
	url := fmt.Sprintf("https://github.com/%s/releases/%s/%s", repo, tag, name)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// snapshot copies the database consistently (VACUUM INTO) while entrypointd is stopped.
func snapshot(ctx context.Context, cfg config.Config, dest string) error {
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer chownDaemon(cfg.DBPath(), cfg.DBPath()+"-wal", cfg.DBPath()+"-shm")
	defer st.Close()
	return st.Snapshot(ctx, dest)
}

// reinstall runs `entrypoint install` of a version: its units, then a restart in order.
func reinstall(ctx context.Context, bin, cfgPath string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, "install")
	cmd.Env = append(os.Environ(), "ENTRYPOINT_CONFIG="+cfgPath)
	return cmd.CombinedOutput()
}

func switchCurrent(current, target string) error {
	tmp := current + ".new"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, current)
}

func systemctl(args ...string) error {
	if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func daemonIDs() (int, int, error) {
	u, err := user.Lookup(config.UserDaemon)
	if err != nil {
		return 0, 0, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return uid, gid, nil
}

func chownDaemon(paths ...string) error {
	uid, gid, err := daemonIDs()
	if err != nil {
		return err
	}
	for _, p := range paths {
		if err := os.Chown(p, uid, gid); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// runAs runs a program as a system user, so files it creates (SQLite's WAL) keep the right owner.
func runAs(name, prog string, args ...string) (string, error) {
	uid, gid, err := daemonIDs()
	if err != nil || name != config.UserDaemon {
		return "", fmt.Errorf("user %s: %v", name, err)
	}
	cmd := exec.Command(prog, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, " | ")
}
