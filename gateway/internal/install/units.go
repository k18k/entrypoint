package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/k18k/entrypoint/gateway/internal/config"
)

const unitDir = "/etc/systemd/system"

const hardening = `NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes
UMask=0027`

// UnitNames are all units of the installation.
var UnitNames = []string{
	"entrypoint.target",
	"entrypointd.socket",
	"entrypointd.service",
	"entrypoint-caddy.service",
	"entrypoint-frps.service",
	"entrypoint-frps-reload.path",
	"entrypoint-frps-reload.service",
	"entrypoint-headscale.service",
	"entrypoint-tailscaled.service",
	"entrypoint-panel.service",
}

// PanelUnit runs the web panel (config.Panel).
const PanelUnit = "entrypoint-panel.service"

// VPNUnits run the built-in VPN (config.VPN).
var VPNUnits = []string{"entrypoint-headscale.service", "entrypoint-tailscaled.service"}

func units(cfg config.Config, configPath string) map[string]string {
	u := baseUnits(cfg, configPath)
	if cfg.VPN.Enabled() {
		u["entrypoint.target"] = strings.Replace(u["entrypoint.target"], "entrypoint-frps-reload.path\n", "entrypoint-frps-reload.path "+strings.Join(VPNUnits, " ")+"\n", 1)
		for k, v := range vpnUnits(cfg) {
			u[k] = v
		}
	}
	if cfg.Panel.Enabled() {
		u["entrypoint.target"] = strings.Replace(u["entrypoint.target"], "entrypoint-frps-reload.path", "entrypoint-frps-reload.path "+PanelUnit, 1)
		u[PanelUnit] = fmt.Sprintf(`[Unit]
Description=Entrypoint: web panel
PartOf=entrypoint.target
After=network-online.target entrypointd.service

[Service]
User=%[1]s
Group=%[1]s
SupplementaryGroups=%[2]s
ExecStart=%[3]s/entrypoint-panel --config %[4]s
Restart=always
RestartSec=1
%[5]s
UMask=0077

[Install]
WantedBy=entrypoint.target
`, config.UserPanel, config.UserDaemon, filepath.Join(config.OptDir, "current", "bin"), configPath, hardening)
	}
	return u
}

func vpnUnits(cfg config.Config) map[string]string {
	bin := filepath.Join(config.OptDir, "current", "bin")
	td := cfg.TailscaleDir()
	tsFlags := fmt.Sprintf("--state=%s/tailscaled.state --statedir=%s --socket=%s --tun=ep-vpn0", td, td, config.TailscaleSocket)
	return map[string]string{
		"entrypoint-headscale.service": fmt.Sprintf(`[Unit]
Description=Entrypoint: Headscale (VPN control server)
PartOf=entrypoint.target
After=network-online.target
Wants=network-online.target

[Service]
User=%[1]s
Group=%[1]s
ExecStart=%[2]s/headscale serve -c %[3]s
Restart=always
RestartSec=1
RuntimeDirectory=entrypoint-headscale
RuntimeDirectoryMode=0750
ReadWritePaths=%[4]s
%[5]s

[Install]
WantedBy=entrypoint.target
`, config.UserVPN, bin, cfg.HeadscaleConfig(), cfg.HeadscaleDir(), hardening),
		// Root with a TUN device, like the upstream unit: Caddy reaches node addresses through it.
		"entrypoint-tailscaled.service": fmt.Sprintf(`[Unit]
Description=Entrypoint: tailscaled (the gateway as a VPN node)
PartOf=entrypoint.target
After=network-online.target entrypoint-headscale.service
Wants=network-online.target

[Service]
Type=notify
ExecStart=%[1]s/tailscaled %[2]s --port=41641 --no-logs-no-support
ExecStopPost=%[1]s/tailscaled %[2]s --cleanup
Restart=on-failure
RuntimeDirectory=entrypoint-tailscale
RuntimeDirectoryMode=0755

[Install]
WantedBy=entrypoint.target
`, bin, tsFlags),
	}
}

// vpnGroup lets entrypointd use Headscale's socket and configuration.
func vpnGroup(cfg config.Config) string {
	if cfg.VPN.Enabled() {
		return " " + config.UserVPN
	}
	return ""
}

func baseUnits(cfg config.Config, configPath string) map[string]string {
	bin := filepath.Join(config.OptDir, "current", "bin")
	dd := cfg.DataDir
	caddyHome := filepath.Join(cfg.CaddyDir(), "home")

	return map[string]string{
		"entrypoint.target": `[Unit]
Description=Entrypoint: service publishing gateway
Wants=entrypointd.socket entrypointd.service entrypoint-caddy.service entrypoint-frps.service entrypoint-frps-reload.path

[Install]
WantedBy=multi-user.target
`,
		"entrypointd.socket": fmt.Sprintf(`[Unit]
Description=Entrypoint: frps plugin and TLS consent socket (entrypointd)
PartOf=entrypoint.target

[Socket]
ListenStream=%s
Backlog=4096
NoDelay=yes

[Install]
WantedBy=entrypoint.target sockets.target
`, cfg.PluginListen),
		"entrypointd.service": fmt.Sprintf(`[Unit]
Description=Entrypoint: control plane (entrypointd)
PartOf=entrypoint.target
Requires=entrypointd.socket
After=network-online.target entrypointd.socket
Wants=network-online.target

[Service]
User=%[1]s
Group=%[1]s
SupplementaryGroups=%[2]s %[3]s%[8]s
ExecStart=%[4]s/entrypointd --config %[5]s
Restart=always
RestartSec=1
RuntimeDirectory=entrypoint
RuntimeDirectoryMode=0750
ReadWritePaths=%[6]s
%[7]s
UMask=0077

[Install]
WantedBy=entrypoint.target
`, config.UserDaemon, config.UserCaddy, config.UserFrps, bin, configPath, dd, hardening, vpnGroup(cfg)),
		"entrypoint-caddy.service": fmt.Sprintf(`[Unit]
Description=Entrypoint: Caddy (HTTP and TLS)
PartOf=entrypoint.target
After=network-online.target
Wants=network-online.target

[Service]
User=%[1]s
Group=%[1]s
ExecStart=%[2]s/caddy run --config %[3]s
Environment=HOME=%[4]s XDG_DATA_HOME=%[4]s XDG_CONFIG_HOME=%[4]s
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
Restart=always
RestartSec=1
LimitNOFILE=1048576
RuntimeDirectory=entrypoint-caddy
RuntimeDirectoryMode=0750
ReadWritePaths=%[5]s
%[6]s

[Install]
WantedBy=entrypoint.target
`, config.UserCaddy, bin, cfg.CaddyConfig(), caddyHome, cfg.CaddyDir(), strings.Replace(hardening, "NoNewPrivileges=yes\n", "", 1)),
		"entrypoint-frps.service": fmt.Sprintf(`[Unit]
Description=Entrypoint: frps (tunnels)
PartOf=entrypoint.target
After=network-online.target
Wants=network-online.target

[Service]
User=%[1]s
Group=%[1]s
ExecStart=%[2]s/frps -c %[3]s
Restart=always
RestartSec=1
LimitNOFILE=1048576
%[4]s

[Install]
WantedBy=entrypoint.target
`, config.UserFrps, bin, cfg.FrpsConfig(), hardening),
		"entrypoint-frps-reload.path": fmt.Sprintf(`[Unit]
Description=Entrypoint: restart frps when the tunnel port certificate changes
PartOf=entrypoint.target

[Path]
PathChanged=%s
Unit=entrypoint-frps-reload.service

[Install]
WantedBy=entrypoint.target
`, cfg.TunnelReloadTrigger()),
		"entrypoint-frps-reload.service": `[Unit]
Description=Entrypoint: restart frps (new tunnel port certificate)

[Service]
Type=oneshot
ExecStart=/bin/systemctl try-restart entrypoint-frps.service
`,
	}
}

func writeUnits(cfg config.Config, configPath string) error {
	// Defense in depth: no interpolated value may carry unit-file syntax.
	if err := cfg.Check(); err != nil {
		return err
	}
	if !filepath.IsAbs(configPath) || strings.ContainsAny(configPath, "\n\r") {
		return fmt.Errorf("invalid config path %q", configPath)
	}
	for name, body := range units(cfg, configPath) {
		if err := os.WriteFile(filepath.Join(unitDir, name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}
