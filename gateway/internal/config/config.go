// Package config describes the gateway's startup configuration (/etc/entrypoint/entrypoint.json)
// and the installation's directory layout.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultConfigPath = "/etc/entrypoint/entrypoint.json"
	OptDir            = "/opt/entrypoint"
	EtcDir            = "/etc/entrypoint"
	DataDir           = "/var/lib/entrypoint"

	LetsEncryptProd    = "https://acme-v02.api.letsencrypt.org/directory"
	LetsEncryptStaging = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

// System users of the individual processes.
const (
	UserDaemon = "entrypoint"
	UserCaddy  = "entrypoint-caddy"
	UserFrps   = "entrypoint-frps"
	UserVPN    = "entrypoint-headscale"
	UserPanel  = "entrypoint-panel"
)

type Duration struct{ time.Duration }

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

type ACME struct {
	Email        string   `json:"email"`
	CA           string   `json:"ca"`
	StagingCA    string   `json:"stagingCA"`
	TrustedRoots []string `json:"trustedRoots,omitempty"` // extra trusted roots for the ACME directory (tests)
}

type Limits struct {
	ReplicasPerToken      int `json:"replicasPerToken"`
	FailedLoginsPerMinute int `json:"failedLoginsPerMinute"`
	OnDemandCertsPerHour  int `json:"onDemandCertsPerHour"`
	// Per-token limits on control-plane operations, counted also for valid tokens (issue #2).
	LoginsPerTokenPerMinute   int `json:"loginsPerTokenPerMinute"`
	SessionsPerToken          int `json:"sessionsPerToken"`
	ProxyOpsPerTokenPerMinute int `json:"proxyOpsPerTokenPerMinute"`
}

type PortPool struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// VPN is the built-in VPN: Headscale behind Caddy and the gateway as a Tailscale node.
// Off while Name is empty.
type VPN struct {
	Name   string `json:"name,omitempty"`   // DNS name of the Headscale control server, e.g. vpn.example.com
	Listen string `json:"listen,omitempty"` // Headscale HTTP; metrics listen on the next port
}

func (v VPN) Enabled() bool { return v.Name != "" }

// Panel is the web panel, served by Caddy on its own name. Off while Name is empty.
type Panel struct {
	Name   string `json:"name,omitempty"` // e.g. panel.example.com
	Listen string `json:"listen,omitempty"`
}

func (p Panel) Enabled() bool { return p.Name != "" }

// Testing holds switches used only by the e2e tests.
type Testing struct {
	ApplyDelay    Duration `json:"applyDelay,omitempty"`    // pause after writing the snapshot, before Caddy
	ACMEProfile   string   `json:"acmeProfile,omitempty"`   // ACME profile (short-lived certificates in renewal tests)
	RenewInterval Duration `json:"renewInterval,omitempty"` // how often Caddy checks for renewals
}

type Config struct {
	GatewayName      string   `json:"gatewayName"`
	TunnelPort       int      `json:"tunnelPort"`
	PublicIPs        []string `json:"publicIPs,omitempty"`
	DataDir          string   `json:"dataDir"`
	APISocket        string   `json:"apiSocket"`
	PluginListen     string   `json:"pluginListen"`
	CaddyAdminSocket string   `json:"caddyAdminSocket"`
	FrpsAPI          string   `json:"frpsAPI"`
	CaddyBin         string   `json:"caddyBin"`
	PortPool         PortPool `json:"portPool"`
	ACME             ACME     `json:"acme"`
	Limits           Limits   `json:"limits"`
	HeartbeatTimeout int      `json:"heartbeatTimeout"` // seconds, frps
	DriftInterval    Duration `json:"driftInterval"`
	VPN              VPN      `json:"vpn"`
	Panel            Panel    `json:"panel"`
	Testing          Testing  `json:"testing,omitempty"`
}

// Default returns the configuration with default values.
func Default() Config {
	return Config{
		TunnelPort:       55555,
		DataDir:          DataDir,
		APISocket:        "/run/entrypoint/api.sock",
		PluginListen:     "127.0.0.1:7400",
		CaddyAdminSocket: "/run/entrypoint-caddy/admin.sock",
		FrpsAPI:          "127.0.0.1:7500",
		CaddyBin:         OptDir + "/current/bin/caddy",
		PortPool:         PortPool{Start: 20000, End: 29999},
		ACME:             ACME{CA: LetsEncryptProd, StagingCA: LetsEncryptStaging},
		Limits: Limits{ReplicasPerToken: 10, FailedLoginsPerMinute: 10, OnDemandCertsPerHour: 10,
			LoginsPerTokenPerMinute: 30, SessionsPerToken: 20, ProxyOpsPerTokenPerMinute: 60},
		HeartbeatTimeout: 40,
		DriftInterval:    Duration{30 * time.Second},
		VPN:              VPN{Listen: "127.0.0.1:7600"},
		Panel:            Panel{Listen: "127.0.0.1:7700"},
	}
}

// Load reads the configuration and fills missing fields with defaults.
func Load(path string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, c.Check()
}

func (c Config) Check() error {
	switch {
	case c.GatewayName == "":
		return errors.New("missing gatewayName")
	case c.TunnelPort <= 0:
		return errors.New("invalid tunnelPort")
	case c.PortPool.Start <= 1024 || c.PortPool.End > 65535 || c.PortPool.Start > c.PortPool.End:
		return errors.New("invalid port pool")
	}
	// These values end up in root-owned systemd units or are executed, so they must be
	// plain addresses and absolute paths (no unit-file syntax, e.g. from a restored backup).
	for name, v := range map[string]string{"pluginListen": c.PluginListen, "frpsAPI": c.FrpsAPI, "vpn.listen": c.VPN.Listen, "panel.listen": c.Panel.Listen} {
		ap, err := netip.ParseAddrPort(v)
		if err != nil || !ap.Addr().IsLoopback() {
			return fmt.Errorf("invalid %s %q: expected a loopback address:port", name, v)
		}
	}
	if ap := netip.MustParseAddrPort(c.VPN.Listen); ap.Port() == 0 || ap.Port() == 65535 {
		return fmt.Errorf("invalid vpn.listen %q: the next port is used for metrics", c.VPN.Listen)
	}
	for name, v := range map[string]string{"dataDir": c.DataDir, "apiSocket": c.APISocket, "caddyAdminSocket": c.CaddyAdminSocket, "caddyBin": c.CaddyBin} {
		if !filepath.IsAbs(v) || filepath.Clean(v) != v || strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f || r == ' ' }) {
			return fmt.Errorf("invalid %s %q: expected a clean absolute path", name, v)
		}
	}
	return nil
}

func (c Config) Save(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Paths in the data directory.
func (c Config) DBPath() string              { return filepath.Join(c.DataDir, "db", "entrypoint.db") }
func (c Config) CACert() string              { return filepath.Join(c.DataDir, "ca", "ca.crt") }
func (c Config) CAKey() string               { return filepath.Join(c.DataDir, "ca", "ca.key") }
func (c Config) CANextCert() string          { return filepath.Join(c.DataDir, "ca", "ca-next.crt") }
func (c Config) CANextKey() string           { return filepath.Join(c.DataDir, "ca", "ca-next.key") }
func (c Config) CAOldCert() string           { return filepath.Join(c.DataDir, "ca", "ca-old.crt") }
func (c Config) TunnelDir() string           { return filepath.Join(c.DataDir, "tunnel") }
func (c Config) TunnelCert() string          { return filepath.Join(c.DataDir, "tunnel", "server.crt") }
func (c Config) TunnelKey() string           { return filepath.Join(c.DataDir, "tunnel", "server.key") }
func (c Config) TunnelReloadTrigger() string { return filepath.Join(c.DataDir, "tunnel", "reload") }
func (c Config) CaddyDir() string            { return filepath.Join(c.DataDir, "caddy") }
func (c Config) CaddyConfig() string         { return filepath.Join(c.DataDir, "caddy", "caddy.json") }
func (c Config) CaddyStorage() string        { return filepath.Join(c.DataDir, "caddy", "storage") }
func (c Config) FrpsConfig() string          { return filepath.Join(c.DataDir, "frps", "frps.json") }
func (c Config) AuthSnapshot() string        { return filepath.Join(c.DataDir, "state", "authz.json") }
func (c Config) FrpsAPIPasswordFile() string { return filepath.Join(c.DataDir, "secrets", "frps-api") }
func (c Config) HeadscaleDir() string        { return filepath.Join(c.DataDir, "headscale") }

// HeadscaleConfig is outside Headscale's data directory, so entrypointd can read it without the keys.
func (c Config) HeadscaleConfig() string { return filepath.Join(EtcDir, "headscale.yaml") }
func (c Config) TailscaleDir() string    { return filepath.Join(c.DataDir, "tailscale") }
func (c Config) PanelDir() string        { return filepath.Join(c.DataDir, "panel") }
func (c Config) PanelPasswordFile() string {
	return filepath.Join(c.DataDir, "panel", "admin.hash")
}

// VPNMetrics is Headscale's metrics address, the port after VPN.Listen.
func (c Config) VPNMetrics() string {
	ap := netip.MustParseAddrPort(c.VPN.Listen)
	return netip.AddrPortFrom(ap.Addr(), ap.Port()+1).String()
}

// Runtime sockets of the VPN processes (their units' RuntimeDirectory).
const (
	HeadscaleSocket = "/run/entrypoint-headscale/headscale.sock"
	TailscaleSocket = "/run/entrypoint-tailscale/tailscaled.sock"
)
