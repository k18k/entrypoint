package config

import "testing"

// Values rendered into systemd units must not carry unit-file syntax (Strix finding:
// pluginListen with "\nExecStartPre=..." from a restored backup ran as root).
func TestCheckRejectsUnitInjection(t *testing.T) {
	ok := Default()
	ok.GatewayName = "tunnel.example.com"
	if err := ok.Check(); err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	bad := map[string]func(*Config){
		"pluginListen newline": func(c *Config) { c.PluginListen = "127.0.0.1:7400\nExecStartPre=/bin/sh -c 'id > /root/pwned'\n" },
		"pluginListen public":  func(c *Config) { c.PluginListen = "0.0.0.0:7400" },
		"frpsAPI host":         func(c *Config) { c.FrpsAPI = "example.com:7500" },
		"dataDir relative":     func(c *Config) { c.DataDir = "var/lib/entrypoint" },
		"dataDir newline":      func(c *Config) { c.DataDir = "/var/lib/entrypoint\nExecStart=/bin/sh" },
		"caddyBin dotdot":      func(c *Config) { c.CaddyBin = "/opt/entrypoint/../../tmp/x" },
		"apiSocket space":      func(c *Config) { c.APISocket = "/run/entrypoint/api.sock ExecStart=x" },
	}
	for name, mut := range bad {
		c := ok
		mut(&c)
		if c.Check() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
