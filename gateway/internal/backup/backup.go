// Package backup creates and restores a password-encrypted archive of the gateway's state.
//
// The archive (tar in an age/scrypt container) holds: a consistent SQLite copy, the private CA with
// its key, the tunnel port certificate, Caddy's storage (ACME account, certificates), the VPN state
// (Headscale's database and keys, the gateway's node identity), the startup configuration and a
// version manifest. Logs are not part of the backup.
package backup

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/k18k/entrypoint/gateway/internal/config"
	"github.com/k18k/entrypoint/gateway/internal/install"
	"github.com/k18k/entrypoint/gateway/internal/render"
	"github.com/k18k/entrypoint/gateway/internal/store"
	"github.com/k18k/entrypoint/gateway/internal/version"
)

const Format = 1

type Manifest struct {
	Format      int       `json:"format"`
	Version     string    `json:"version"`
	CreatedAt   time.Time `json:"createdAt"`
	GatewayName string    `json:"gatewayName"`
	LastApplied int64     `json:"lastAppliedRevision"`
	Versions    any       `json:"versions,omitempty"`
}

// Archive entries: name in the archive -> path on disk.
func entries(cfg config.Config) map[string]string {
	return map[string]string{
		"ca":            filepath.Join(cfg.DataDir, "ca"),
		"tunnel":        cfg.TunnelDir(),
		"caddy/storage": cfg.CaddyStorage(),
		"secrets":       filepath.Join(cfg.DataDir, "secrets"),
		"headscale":     cfg.HeadscaleDir(),
		"tailscale":     cfg.TailscaleDir(),
	}
}

// Create writes a backup to w.
func Create(ctx context.Context, cfg config.Config, cfgPath, password string, w io.Writer) (*Manifest, error) {
	if password == "" {
		return nil, errors.New("the backup password must not be empty")
	}
	tmp, err := os.MkdirTemp("", "ep-backup-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	// SQLite copy without stopping the services (VACUUM INTO on the live database in WAL mode).
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, err
	}
	dbCopy := filepath.Join(tmp, "entrypoint.db")
	if err := st.Snapshot(ctx, dbCopy); err != nil {
		st.Close()
		return nil, fmt.Errorf("database copy: %w", err)
	}
	last, _ := st.LastApplied(ctx)
	st.Close()

	m := &Manifest{Format: Format, Version: version.Version, CreatedAt: time.Now().UTC(), GatewayName: cfg.GatewayName}
	if last != nil {
		m.LastApplied = last.Num
	}
	if b, err := os.ReadFile(filepath.Join(config.OptDir, "current", "versions.json")); err == nil {
		var v any
		if json.Unmarshal(b, &v) == nil {
			m.Versions = v
		}
	}

	rcp, err := age.NewScryptRecipient(password)
	if err != nil {
		return nil, err
	}
	aw, err := age.Encrypt(w, rcp)
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(aw)
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := addBytes(tw, "manifest.json", mb, 0o600); err != nil {
		return nil, err
	}
	if err := addFile(tw, "db/entrypoint.db", dbCopy); err != nil {
		return nil, err
	}
	if err := addFile(tw, "etc/entrypoint.json", cfgPath); err != nil {
		return nil, err
	}
	for name, dir := range entries(cfg) {
		if err := addTree(tw, name, dir); err != nil {
			return nil, err
		}
	}
	if hdb := filepath.Join(cfg.HeadscaleDir(), "db.sqlite"); cfg.VPN.Enabled() {
		if _, err := os.Stat(hdb); err != nil {
			return nil, fmt.Errorf("VPN database: %w", err)
		}
		cp := filepath.Join(tmp, "headscale.sqlite")
		if err := store.CopyDB(ctx, hdb, cp); err != nil {
			return nil, fmt.Errorf("VPN database copy: %w", err)
		}
		if err := addFile(tw, "headscale/db.sqlite", cp); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := aw.Close(); err != nil {
		return nil, err
	}
	return m, nil
}

func addBytes(tw *tar.Writer, name string, b []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(b)), ModTime: time.Now(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(b)
	return err
}

func addFile(tw *tar.Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: int64(fi.Mode().Perm()), Size: fi.Size(), ModTime: fi.ModTime(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

func addTree(tw *tar.Writer, prefix, root string) error {
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		name := filepath.ToSlash(filepath.Join(prefix, rel))
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Name: name + "/", Mode: 0o700, Typeflag: tar.TypeDir, ModTime: time.Now()})
		}
		if !d.Type().IsRegular() {
			return nil // skip links and special files
		}
		if strings.Contains(filepath.Base(p), ".tmp-") {
			return nil
		}
		if strings.HasPrefix(filepath.Base(p), "db.sqlite") {
			return nil // Headscale's live database: added as a consistent copy
		}
		return addFile(tw, name, p)
	})
}

// Restore restores a backup onto an installed gateway. All restored tokens are
// invalidated before frps and the plugin start accepting logins.
func Restore(ctx context.Context, cfgPath, password string, r io.Reader, out io.Writer) (*Manifest, int, error) {
	if os.Geteuid() != 0 {
		return nil, 0, errors.New("restore requires root (sudo)")
	}
	cur, err := config.Load(cfgPath)
	if err != nil {
		return nil, 0, fmt.Errorf("install the same gateway version first (install.sh, entrypoint install): %w", err)
	}
	id, err := age.NewScryptIdentity(password)
	if err != nil {
		return nil, 0, err
	}
	ar, err := age.Decrypt(r, id)
	if err != nil {
		return nil, 0, errors.New("cannot decrypt the backup (wrong password or corrupted file)")
	}
	tmp, err := os.MkdirTemp("", "ep-restore-")
	if err != nil {
		return nil, 0, err
	}
	defer os.RemoveAll(tmp)
	if err := extract(tar.NewReader(ar), tmp); err != nil {
		return nil, 0, err
	}
	var m Manifest
	mb, err := os.ReadFile(filepath.Join(tmp, "manifest.json"))
	if err != nil || json.Unmarshal(mb, &m) != nil || m.Format != Format {
		return nil, 0, errors.New("backup without a valid manifest")
	}
	if m.Version != version.Version {
		return &m, 0, fmt.Errorf("the backup comes from version %s, %s is installed; install the same version", m.Version, version.Version)
	}
	bcfg, err := config.Load(filepath.Join(tmp, "etc", "entrypoint.json"))
	if err != nil {
		return &m, 0, fmt.Errorf("configuration in the backup: %w", err)
	}
	pinHostFields(&bcfg, cur)

	fmt.Fprintln(out, "Stopping services...")
	_ = exec.Command("systemctl", append([]string{"stop", "entrypoint-frps.service", "entrypointd.service", "entrypoint-caddy.service"}, install.VPNUnits...)...).Run()

	dd := bcfg.DataDir
	_ = os.Remove(bcfg.DBPath() + "-wal")
	_ = os.Remove(bcfg.DBPath() + "-shm")
	if err := copyFile(filepath.Join(tmp, "db", "entrypoint.db"), bcfg.DBPath(), 0o600); err != nil {
		return &m, 0, err
	}
	for name, dir := range entries(bcfg) {
		src := filepath.Join(tmp, filepath.FromSlash(name))
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return &m, 0, err
		}
		if err := copyTree(src, dir); err != nil {
			return &m, 0, err
		}
	}
	if err := bcfg.Save(cfgPath); err != nil {
		return &m, 0, err
	}

	// Emergency restore: the backup does not know revocations made before the machine was lost.
	st, err := store.Open(bcfg.DBPath())
	if err != nil {
		return &m, 0, err
	}
	n, err := st.RevokeAll(ctx, "emergency restore from backup "+m.CreatedAt.Format(time.RFC3339))
	if err != nil {
		st.Close()
		return &m, 0, err
	}
	last, _ := st.LastApplied(ctx)
	st.Close()
	_ = os.Remove(bcfg.AuthSnapshot())
	if last != nil {
		if c, err := render.Caddy(last.Spec, render.OptionsFrom(bcfg)); err == nil {
			_ = os.WriteFile(bcfg.CaddyConfig(), c, 0o640)
		}
	}

	// Reinstalling sets owners and permissions (UIDs may differ on a new machine),
	// renders the frps configuration and starts the services.
	if err := fixOwnership(bcfg); err != nil {
		return &m, n, err
	}
	fmt.Fprintln(out, "Starting services...")
	if err := install.Install(install.Options{Config: bcfg, ConfigPath: cfgPath, Out: io.Discard}); err != nil {
		return &m, n, err
	}
	_ = dd
	return &m, n, nil
}

func extract(tr *tar.Reader, dest string) error {
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("corrupted archive: %w", err)
		}
		name := filepath.Clean(filepath.FromSlash(h.Name))
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("path not allowed in the archive: %s", h.Name)
		}
		p := filepath.Join(dest, name)
		if !strings.HasPrefix(p, filepath.Clean(dest)+string(filepath.Separator)) {
			return fmt.Errorf("path outside the target directory: %s", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
			_ = os.Chmod(p, fs.FileMode(h.Mode).Perm())
		default:
			return fmt.Errorf("file type not allowed in the archive: %s", h.Name)
		}
	}
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
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

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(p, target, fi.Mode().Perm())
	})
}

// fixOwnership sets the owners of restored files to this machine's users.
func fixOwnership(cfg config.Config) error {
	type own struct{ path, owner, group string }
	list := []own{
		{filepath.Join(cfg.DataDir, "db"), config.UserDaemon, config.UserDaemon},
		{filepath.Join(cfg.DataDir, "ca"), config.UserDaemon, config.UserDaemon},
		{filepath.Join(cfg.DataDir, "state"), config.UserDaemon, config.UserDaemon},
		{cfg.TunnelDir(), config.UserDaemon, config.UserFrps},
		{cfg.CaddyStorage(), config.UserCaddy, config.UserCaddy},
		{filepath.Join(cfg.DataDir, "secrets"), "root", config.UserDaemon},
		{cfg.CaddyConfig(), config.UserDaemon, config.UserCaddy},
	}
	for _, o := range list {
		if _, err := os.Stat(o.path); err != nil {
			continue
		}
		if out, err := exec.Command("chown", "-R", o.owner+":"+o.group, o.path).CombinedOutput(); err != nil {
			return fmt.Errorf("chown %s: %v: %s", o.path, err, out)
		}
	}
	return nil
}

// pinHostFields keeps machine-specific settings from the current host, never from the
// archive: they end up in root-owned systemd units or are executed, so a crafted backup
// must not be able to change them.
func pinHostFields(b *config.Config, cur config.Config) {
	b.PublicIPs = cur.PublicIPs
	b.DataDir = cur.DataDir
	b.APISocket = cur.APISocket
	b.PluginListen = cur.PluginListen
	b.CaddyAdminSocket = cur.CaddyAdminSocket
	b.FrpsAPI = cur.FrpsAPI
	b.CaddyBin = cur.CaddyBin
	b.Testing = cur.Testing
	b.VPN.Listen = cur.VPN.Listen
}
