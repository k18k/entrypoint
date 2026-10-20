// Pakiet config opisuje konfigurację startową bramy (/etc/entrypoint/entrypoint.json)
// i układ katalogów instalacji.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// Użytkownicy systemowi poszczególnych procesów.
const (
	UserDaemon = "entrypoint"
	UserCaddy  = "entrypoint-caddy"
	UserFrps   = "entrypoint-frps"
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
	TrustedRoots []string `json:"trustedRoots,omitempty"` // dodatkowe zaufane CA katalogu ACME (testy)
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

// Testing zawiera przełączniki używane wyłącznie przez testy e2e.
type Testing struct {
	ApplyDelay    Duration `json:"applyDelay,omitempty"`    // pauza po zapisie snapshotu, przed Caddy
	ACMEProfile   string   `json:"acmeProfile,omitempty"`   // profil ACME (krótkie certyfikaty w testach odnowień)
	RenewInterval Duration `json:"renewInterval,omitempty"` // jak często Caddy sprawdza odnowienia
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
	HeartbeatTimeout int      `json:"heartbeatTimeout"` // sekundy, frps
	DriftInterval    Duration `json:"driftInterval"`
	Testing          Testing  `json:"testing,omitempty"`
}

// Default zwraca konfigurację z wartościami domyślnymi.
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
	}
}

// Load wczytuje konfigurację i uzupełnia brakujące pola wartościami domyślnymi.
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
		return errors.New("brak gatewayName")
	case c.TunnelPort <= 0:
		return errors.New("niepoprawny tunnelPort")
	case c.PortPool.Start <= 1024 || c.PortPool.End > 65535 || c.PortPool.Start > c.PortPool.End:
		return errors.New("niepoprawna pula portów")
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

// Ścieżki w katalogu danych.
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
