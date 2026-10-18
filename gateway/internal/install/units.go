package install

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/k18k/entrypoint/gateway/internal/config"
)

const unitDir = "/etc/systemd/system"

// UnitNames to wszystkie jednostki instalacji.
var UnitNames = []string{
	"entrypoint.target",
	"entrypointd.service",
	"entrypoint-caddy.service",
	"entrypoint-frps.service",
	"entrypoint-frps-reload.path",
	"entrypoint-frps-reload.service",
}

func units(cfg config.Config, configPath string) map[string]string {
	bin := filepath.Join(config.OptDir, "current", "bin")
	dd := cfg.DataDir
	caddyHome := filepath.Join(cfg.CaddyDir(), "home")
	hardening := `NoNewPrivileges=yes
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
	return map[string]string{
		"entrypoint.target": `[Unit]
Description=Entrypoint: brama publikowania usług
Wants=entrypointd.service entrypoint-caddy.service entrypoint-frps.service entrypoint-frps-reload.path

[Install]
WantedBy=multi-user.target
`,
		"entrypointd.service": fmt.Sprintf(`[Unit]
Description=Entrypoint: warstwa sterowania (entrypointd)
PartOf=entrypoint.target
After=network-online.target
Wants=network-online.target

[Service]
User=%[1]s
Group=%[1]s
SupplementaryGroups=%[2]s %[3]s
ExecStart=%[4]s/entrypointd --config %[5]s
Restart=always
RestartSec=1
RuntimeDirectory=entrypoint
RuntimeDirectoryMode=0750
ReadWritePaths=%[6]s
%[7]s

[Install]
WantedBy=entrypoint.target
`, config.UserDaemon, config.UserCaddy, config.UserFrps, bin, configPath, dd, hardening),
		"entrypoint-caddy.service": fmt.Sprintf(`[Unit]
Description=Entrypoint: Caddy (HTTP i TLS)
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
Description=Entrypoint: frps (tunele)
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
Description=Entrypoint: restart frps po zmianie certyfikatu portu tunelu
PartOf=entrypoint.target

[Path]
PathChanged=%s
Unit=entrypoint-frps-reload.service

[Install]
WantedBy=entrypoint.target
`, cfg.TunnelReloadTrigger()),
		"entrypoint-frps-reload.service": `[Unit]
Description=Entrypoint: restart frps (nowy certyfikat portu tunelu)

[Service]
Type=oneshot
ExecStart=/bin/systemctl try-restart entrypoint-frps.service
`,
	}
}

func writeUnits(cfg config.Config, configPath string) error {
	for name, body := range units(cfg, configPath) {
		if err := os.WriteFile(filepath.Join(unitDir, name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}
