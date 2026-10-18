// Pakiet install zakłada i usuwa instalację bramy: użytkowników, katalogi,
// prywatne CA, konfigurację startową frps i Caddy oraz jednostki systemd.
package install

import (
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
	"github.com/k18k/entrypoint/gateway/internal/pki"
	"github.com/k18k/entrypoint/gateway/internal/render"
	"github.com/k18k/entrypoint/gateway/internal/spec"
)

// Options to parametry instalacji.
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
	owner string // użytkownik ("" = root)
	group string // grupa ("" = root)
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

// Install zakłada instalację. Ponowne wywołanie nie niszczy danych ani CA.
func Install(o Options) error {
	if os.Geteuid() != 0 {
		return errors.New("instalacja wymaga uprawnień root (sudo)")
	}
	if !exists("/run/systemd/system") {
		return errors.New("wspierany jest wyłącznie system z systemd")
	}
	if !exists(filepath.Join(config.OptDir, "current", "bin", "caddy")) {
		return errors.New("brak programów w /opt/entrypoint/current; najpierw uruchom install.sh")
	}
	out := o.Out
	cfg := o.Config

	// Istniejąca konfiguracja ma pierwszeństwo dla nazwy bramy (zmiana wymaga nowego certyfikatu).
	if prev, err := config.Load(o.ConfigPath); err == nil {
		if cfg.GatewayName != "" && cfg.GatewayName != prev.GatewayName {
			return fmt.Errorf("brama jest już zainstalowana jako %s; zmiana nazwy nie jest obsługiwana w v0.1", prev.GatewayName)
		}
		cfg.GatewayName = prev.GatewayName
		fmt.Fprintf(out, "Istniejąca instalacja %s: aktualizuję usługi, dane zostają.\n", prev.GatewayName)
	}
	if err := cfg.Check(); err != nil {
		return err
	}
	if _, err := spec.NormalizeHost(cfg.GatewayName); err != nil || strings.HasPrefix(cfg.GatewayName, "*.") {
		return fmt.Errorf("--gateway-name musi być nazwą DNS bramy, np. tunnel.example.com")
	}

	for _, u := range []string{config.UserDaemon, config.UserCaddy, config.UserFrps} {
		if err := ensureUser(u); err != nil {
			return err
		}
	}
	// entrypointd potrzebuje socketu admin Caddy i zapisuje certyfikat czytany przez frps.
	if err := run("usermod", "-a", "-G", config.UserCaddy+","+config.UserFrps, config.UserDaemon); err != nil {
		return err
	}

	dd := cfg.DataDir
	dirs := []dir{
		{config.EtcDir, "", "", 0o755},
		{dd, "", "", 0o755},
		{filepath.Join(dd, "db"), config.UserDaemon, config.UserDaemon, 0o700},
		{filepath.Join(dd, "ca"), config.UserDaemon, config.UserDaemon, 0o700},
		{filepath.Join(dd, "state"), config.UserDaemon, config.UserDaemon, 0o700},
		{filepath.Join(dd, "secrets"), "", config.UserDaemon, 0o750},
		// setgid: nowe pliki dziedziczą grupę procesu, który je czyta.
		{cfg.TunnelDir(), config.UserDaemon, config.UserFrps, os.ModeSetgid | 0o750},
		{cfg.CaddyDir(), config.UserDaemon, config.UserCaddy, os.ModeSetgid | 0o750},
		{cfg.CaddyStorage(), config.UserCaddy, config.UserCaddy, 0o700},
		{filepath.Join(cfg.CaddyDir(), "home"), config.UserCaddy, config.UserCaddy, 0o700},
		{filepath.Join(dd, "frps"), "", config.UserFrps, 0o750},
	}
	for _, d := range dirs {
		if err := ensureDir(d); err != nil {
			return fmt.Errorf("katalog %s: %w", d.path, err)
		}
	}

	if err := cfg.Save(o.ConfigPath); err != nil {
		return err
	}

	// Prywatne CA (klucz tylko na serwerze, 0600) i certyfikat portu tunelu.
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
		fmt.Fprintln(out, "Utworzono prywatne CA tunelu.")
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
		fmt.Fprintf(out, "Wystawiono certyfikat portu tunelu dla %s.\n", cfg.GatewayName)
	}
	if !exists(cfg.TunnelReloadTrigger()) {
		if err := writeOwned(cfg.TunnelReloadTrigger(), []byte("init\n"), 0o644, config.UserDaemon, config.UserFrps); err != nil {
			return err
		}
	}

	// Hasło lokalnego API frps (stan replik dla entrypointd).
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

	// Plik startowy Caddy: tylko przy pierwszej instalacji; potem utrzymuje go entrypointd.
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
	if err := run("systemctl", "enable", "entrypoint.target", "entrypointd.socket", "entrypointd.service", "entrypoint-caddy.service", "entrypoint-frps.service", "entrypoint-frps-reload.path"); err != nil {
		return err
	}
	// Restart podnosi nowe programy po ponownej instalacji; przy pierwszej po prostu startuje.
	_ = run("systemctl", "stop", "entrypointd.service", "entrypointd.socket")
	if err := run("systemctl", "restart", "entrypoint.target", "entrypointd.socket", "entrypointd.service", "entrypoint-caddy.service", "entrypoint-frps.service", "entrypoint-frps-reload.path"); err != nil {
		return err
	}
	return waitHealthy(cfg, 60*time.Second)
}

func waitHealthy(cfg config.Config, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if exists(cfg.APISocket) {
			if err := run("systemctl", "is-active", "--quiet", "entrypointd.service", "entrypoint-caddy.service", "entrypoint-frps.service"); err == nil {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("usługi nie wystartowały w czasie; sprawdź: entrypoint logs entrypointd")
}

// FrpsConfig renderuje konfigurację startową frps.
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
			"maxPoolCount":     10,
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
