package install

import (
	"strings"
	"testing"

	"github.com/k18k/entrypoint/gateway/internal/config"
)

func TestFirewallRules(t *testing.T) {
	cfg := config.Default()
	r := FirewallRules(cfg)
	for _, want := range []string{
		"delete table inet entrypoint",
		"tcp dport { 80, 443, 55555 } ct state new add @rate4 { ip saddr limit rate over 100/second burst 200 packets } counter drop",
		"add @conn6 { ip6 saddr & ffff:ffff:ffff:ffff:: ct count over 256 } counter reject with tcp reset",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("missing %q in\n%s", want, r)
		}
	}
	cfg.Limits.PerIPConnections, cfg.Limits.PerIPNewConnsPerSecond = 0, 0
	if r := FirewallRules(cfg); strings.Contains(r, "drop") || strings.Contains(r, "reject") {
		t.Errorf("limits off but rules present:\n%s", r)
	}
}
