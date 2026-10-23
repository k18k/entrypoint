// Package install sets up and removes the gateway installation: users, directories,
// the private CA, the frps and Caddy startup configuration and the systemd units.
package install

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/k18k/entrypoint/gateway/internal/config"
	"github.com/k18k/entrypoint/gateway/internal/panel"
	"github.com/k18k/entrypoint/gateway/internal/pki"
	"github.com/k18k/entrypoint/gateway/internal/render"
	"github.com/k18k/entrypoint/gateway/internal/spec"
	"github.com/k18k/entrypoint/gateway/internal/store"
)

// Options are the installation parameters.
type Options struct {
	Config     config.Config
	ConfigPath string
	Out        io.Writer
	NoStart    bool
}

type ids struct{ uid, gid int }

func lookup(name string) (ids, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return ids{}, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	g, err := user.LookupGroup(name)
	if err != nil {
		return ids{}, err
	}
	gid, _ := strconv.Atoi(g.Gid)
	return ids{uid, gid}, nil
}

func groupID(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(g.Gid)
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func ensureUser(name string) error {
	if _, err := user.Lookup(name); err == nil {
		return nil
	}
	nologin := "/usr/sbin/nologin"
	if _, err := os.Stat(nologin); err != nil {
		nologin = "/bin/false"
	}
	return run("useradd", "--system", "--user-group", "--no-create-home", "--home-dir", "/nonexistent", "--shell", nologin, name)
}

type dir struct {
	path  string
	owner string // user ("" = root)
	group string // group ("" = root)
	mode  os.FileMode
}

func chownName(path, owner, group string) error {
	uid, gid := 0, 0
	if owner != "" {
		i, err := lookup(owner)
		if err != nil {
			return err
		}
		uid = i.uid
	}
	if group != "" {
		g, err := groupID(group)
		if err != nil {
			return err
		}
		gid = g
	}
	return os.Lchown(path, uid, gid)
}

func ensureDir(d dir) error {
	if err := os.MkdirAll(d.path, 0o700); err != nil {
		return err
	}
	if err := chownName(d.path, d.owner, d.group); err != nil {
		return err
	}
	return os.Chmod(d.path, d.mode)
}

func writeOwned(path string, data []byte, mode os.FileMode, owner, group string) error {
	if err := pki.WriteFileAtomic(path, data, mode); err != nil {
		return err
	}
	if err := chownName(path, owner, group); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Install sets up the installation. Running it again destroys neither data nor the CA.
func Install(o Options) error {
	if os.Geteuid() != 0 {
		return errors.New("install requires root (sudo)")
	}
	if !exists("/run/systemd/system") {
		return errors.New("only systems with systemd are supported")
	}
	if !exists(filepath.Join(config.OptDir, "current", "bin", "caddy")) {
		return errors.New("no programs in /opt/entrypoint/current; run install.sh first")
	}
	out := o.Out
	cfg := o.Config

	// The existing configuration wins for the gateway name (changing it needs a new certificate).
	if prev, err := config.Load(o.ConfigPath); err == nil {
		if cfg.GatewayName != "" && cfg.GatewayName != prev.GatewayName {
			return fmt.Errorf("the gateway is already installed as %s; renaming is not supported in v0.1", prev.GatewayName)
		}
		cfg.GatewayName = prev.GatewayName
		if prev.VPN.Enabled() && cfg.VPN.Name != prev.VPN.Name {
			return fmt.Errorf("the VPN is already set up as %s; changing or removing it is not supported", prev.VPN.Name)
		}
		fmt.Fprintf(out, "Existing installation %s: updating services, data is kept.\n", prev.GatewayName)
	}
	if err := cfg.Check(); err != nil {
		return err
	}
	if _, err := spec.NormalizeHost(cfg.GatewayName); err != nil || strings.HasPrefix(cfg.GatewayName, "*.") {
		return fmt.Errorf("--gateway-name must be the gateway's DNS name, e.g. tunnel.example.com")
	}
	if cfg.VPN.Enabled() {
		if h, err := spec.NormalizeHost(cfg.VPN.Name); err != nil || h != cfg.VPN.Name || strings.HasPrefix(h, "*.") || h == cfg.GatewayName {
			return fmt.Errorf("--vpn-name must be its own DNS name, e.g. vpn.example.com")
		}
		if err := hostFree(cfg, cfg.VPN.Name, "--vpn-name"); err != nil {
			return err
		}
	}
	if cfg.Panel.Enabled() {
		if h, err := spec.NormalizeHost(cfg.Panel.Name); err != nil || h != cfg.Panel.Name || strings.HasPrefix(h, "*.") || h == cfg.GatewayName || h == cfg.VPN.Name {
			return fmt.Errorf("--panel-name must be its own DNS name, e.g. panel.example.com")
		}
		if err := hostFree(cfg, cfg.Panel.Name, "--panel-name"); err != nil {
			return err
		}
	}

	users := []string{config.UserDaemon, config.UserCaddy, config.UserFrps}
	if cfg.VPN.Enabled() {
		users = append(users, config.UserVPN)
	}
	if cfg.Panel.Enabled() {
		users = append(users, config.UserPanel)
	}
	for _, u := range users {
		if err := ensureUser(u); err != nil {
			return err
		}
	}
	// entrypointd needs Caddy's admin socket, writes the certificate frps reads and manages VPN nodes.
	if err := run("usermod", "-a", "-G", strings.TrimSpace(config.UserCaddy+","+config.UserFrps+strings.ReplaceAll(vpnGroup(cfg), " ", ",")), config.UserDaemon); err != nil {
		return err
	}
	if cfg.Panel.Enabled() {
		// The panel talks to entrypointd through its API socket.
		if err := run("usermod", "-a", "-G", config.UserDaemon, config.UserPanel); err != nil {
			return err
		}
	}

	dd := cfg.DataDir
	dirs := []dir{
		{config.EtcDir, "", "", 0o755},
		{dd, "", "", 0o755},
		{filepath.Join(dd, "db"), config.UserDaemon, config.UserDaemon, 0o700},
		{filepath.Join(dd, "ca"), config.UserDaemon, config.UserDaemon, 0o700},
		{filepath.Join(dd, "state"), config.UserDaemon, config.UserDaemon, 0o700},
		{filepath.Join(dd, "secrets"), "", config.UserDaemon, 0o750},
		// setgid: new files inherit the group of the process that reads them.
		{cfg.TunnelDir(), config.UserDaemon, config.UserFrps, os.ModeSetgid | 0o750},
		{cfg.CaddyDir(), config.UserDaemon, config.UserCaddy, os.ModeSetgid | 0o750},
		{cfg.CaddyStorage(), config.UserCaddy, config.UserCaddy, 0o700},
		{filepath.Join(cfg.CaddyDir(), "home"), config.UserCaddy, config.UserCaddy, 0o700},
		{filepath.Join(dd, "frps"), "", config.UserFrps, 0o750},
	}
	if cfg.VPN.Enabled() {
		dirs = append(dirs,
			dir{cfg.HeadscaleDir(), config.UserVPN, config.UserVPN, 0o700},
			dir{cfg.TailscaleDir(), "", "", 0o700})
	}
	if cfg.Panel.Enabled() {
		dirs = append(dirs, dir{cfg.PanelDir(), config.UserPanel, config.UserPanel, 0o700})
	}
	for _, d := range dirs {
		if err := ensureDir(d); err != nil {
			return fmt.Errorf("directory %s: %w", d.path, err)
		}
	}
	if cfg.VPN.Enabled() {
		// Restored files arrive owned by root, and the user's UID may differ on a new machine.
		if err := run("chown", "-R", config.UserVPN+":"+config.UserVPN, cfg.HeadscaleDir()); err != nil {
			return err
		}
	}
	if cfg.Panel.Enabled() {
		if err := run("chown", "-R", config.UserPanel+":"+config.UserPanel, cfg.PanelDir()); err != nil {
			return err
		}
	}

	if err := cfg.Save(o.ConfigPath); err != nil {
		return err
	}

	// Private CA (key only on the server, 0600) and the tunnel port certificate.
	if !exists(cfg.CAKey()) {
		ca, keyPEM, err := pki.NewCA("entrypoint CA " + cfg.GatewayName)
		if err != nil {
			return err
		}
		if err := writeOwned(cfg.CAKey(), keyPEM, 0o600, config.UserDaemon, config.UserDaemon); err != nil {
			return err
		}
		if err := writeOwned(cfg.CACert(), ca.CertPEM, 0o644, config.UserDaemon, config.UserDaemon); err != nil {
			return err
		}
		fmt.Fprintln(out, "Created the private tunnel CA.")
	}
	if !exists(cfg.TunnelCert()) {
		ca, err := pki.LoadCA(cfg.CACert(), cfg.CAKey())
		if err != nil {
			return err
		}
		certPEM, keyPEM, err := ca.IssueServer(cfg.GatewayName)
		if err != nil {
			return err
		}
		if err := writeOwned(cfg.TunnelKey(), keyPEM, 0o640, config.UserDaemon, config.UserFrps); err != nil {
			return err
		}
		if err := writeOwned(cfg.TunnelCert(), certPEM, 0o644, config.UserDaemon, config.UserFrps); err != nil {
			return err
		}
		fmt.Fprintf(out, "Issued the tunnel port certificate for %s.\n", cfg.GatewayName)
	}
	if !exists(cfg.TunnelReloadTrigger()) {
		if err := writeOwned(cfg.TunnelReloadTrigger(), []byte("init\n"), 0o644, config.UserDaemon, config.UserFrps); err != nil {
			return err
		}
	}

	// Password of the local frps API (replica state for entrypointd).
	if !exists(cfg.FrpsAPIPasswordFile()) {
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		if err := writeOwned(cfg.FrpsAPIPasswordFile(), []byte(base64.RawURLEncoding.EncodeToString(b)+"\n"), 0o640, "", config.UserDaemon); err != nil {
			return err
		}
	}
	pw, err := os.ReadFile(cfg.FrpsAPIPasswordFile())
	if err != nil {
		return err
	}
	frpsCfg, err := FrpsConfig(cfg, strings.TrimSpace(string(pw)))
	if err != nil {
		return err
	}
	if err := writeOwned(cfg.FrpsConfig(), frpsCfg, 0o640, "", config.UserFrps); err != nil {
		return err
	}

	// Caddy startup file: only on the first install; entrypointd maintains it afterwards.
	if !exists(cfg.CaddyConfig()) {
		empty := &spec.Spec{}
		empty.Normalize()
		c, err := render.Caddy(empty, render.OptionsFrom(cfg))
		if err != nil {
			return err
		}
		if err := writeOwned(cfg.CaddyConfig(), c, 0o640, config.UserDaemon, config.UserCaddy); err != nil {
			return err
		}
	}

	if cfg.VPN.Enabled() {
		hc, err := HeadscaleConfig(cfg)
		if err != nil {
			return err
		}
		if err := writeOwned(cfg.HeadscaleConfig(), hc, 0o640, "", config.UserVPN); err != nil {
			return err
		}
	}

	nft, _ := firewallUnit()
	if nft != "" {
		if err := writeOwned(firewallFile, []byte(FirewallRules(cfg)), 0o644, "", ""); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(out, "warning: nftables (nft) is not installed, so per-IP connection limits are off; install it: apt install nftables")
	}

	if err := writeUnits(cfg, o.ConfigPath); err != nil {
		return err
	}
	if err := os.Symlink(filepath.Join(config.OptDir, "current", "bin", "entrypoint"), "/usr/local/bin/entrypoint"); err != nil && !os.IsExist(err) {
		return err
	}
	if err := run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if o.NoStart {
		return nil
	}
	start := []string{"entrypoint.target", "entrypointd.socket", "entrypointd.service", "entrypoint-caddy.service", "entrypoint-frps.service", "entrypoint-frps-reload.path"}
	if cfg.VPN.Enabled() {
		start = append(start, VPNUnits...)
	}
	if cfg.Panel.Enabled() {
		start = append(start, PanelUnit)
	}
	if nft != "" {
		start = append(start, FirewallUnit)
	}
	if err := run("systemctl", append([]string{"enable"}, start...)...); err != nil {
		return err
	}
	// Restarting picks up new programs after a reinstall; on the first install it just starts them.
	_ = run("systemctl", "stop", "entrypointd.service", "entrypointd.socket")
	if err := run("systemctl", append([]string{"restart"}, start...)...); err != nil {
		return err
	}
	if err := waitHealthy(cfg, 60*time.Second); err != nil {
		return err
	}
	if cfg.VPN.Enabled() {
		if err := joinVPN(cfg, out); err != nil {
			return err
		}
	}
	if cfg.Panel.Enabled() {
		if keys, err := panel.LoadKeys(cfg); err == nil && len(keys.Keys) == 0 {
			fmt.Fprintf(out, "\nPanel: https://%s\nSign-in is with passkeys only. Register yours: sudo entrypoint panel register\n", cfg.Panel.Name)
		}
	}
	return nil
}

// hostFree rejects a name for the gateway's own sites that existing routes already use.
func hostFree(cfg config.Config, name, flag string) error {
	if !exists(cfg.DBPath()) {
		return nil
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	head, err := st.Head(context.Background())
	if err != nil || head == nil {
		return err
	}
	for _, r := range head.Spec.Routes {
		if r.Host == name {
			return fmt.Errorf("%s %s is used by route %s; remove the route first", flag, name, r.ID)
		}
	}
	return nil
}

func waitHealthy(cfg config.Config, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Caddy is "active" before its admin socket exists; changes need the socket.
		if exists(cfg.APISocket) && exists(cfg.CaddyAdminSocket) {
			units := []string{"is-active", "--quiet", "entrypointd.service", "entrypoint-caddy.service", "entrypoint-frps.service"}
			if cfg.VPN.Enabled() {
				units = append(units, VPNUnits...)
			}
			if cfg.Panel.Enabled() {
				units = append(units, PanelUnit)
			}
			if err := run("systemctl", units...); err == nil {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("the services did not start in time; check: entrypoint logs entrypointd")
}

// FrpsConfig renders the frps startup configuration.
func FrpsConfig(cfg config.Config, apiPassword string) ([]byte, error) {
	host, portS, _ := strings.Cut(cfg.FrpsAPI, ":")
	apiPort, _ := strconv.Atoi(portS)
	c := map[string]any{
		"bindAddr":      "0.0.0.0",
		"bindPort":      cfg.TunnelPort,
		"proxyBindAddr": "127.0.0.1",
		"allowPorts":    []any{map[string]int{"start": cfg.PortPool.Start, "end": cfg.PortPool.End}},
		"transport": map[string]any{
			"tls": map[string]any{
				"force":    true,
				"certFile": cfg.TunnelCert(),
				"keyFile":  cfg.TunnelKey(),
			},
			"heartbeatTimeout": cfg.HeartbeatTimeout,
			// Pooled work connections a session may hold idle (the connector uses 1).
			"maxPoolCount": 2,
		},
		"webServer": map[string]any{
			"addr":     host,
			"port":     apiPort,
			"user":     "entrypoint",
			"password": apiPassword,
		},
		"log": map[string]any{
			"to":                "console",
			"level":             "info",
			"disablePrintColor": true,
		},
		"detailedErrorsToClient": true,
		"userConnTimeout":        10,
		"httpPlugins": []any{map[string]any{
			"name": "entrypoint",
			"addr": cfg.PluginListen,
			"path": "/frps/handler",
			"ops":  []string{"Login", "NewProxy", "Ping"},
		}},
	}
	return json.MarshalIndent(c, "", "  ")
}
