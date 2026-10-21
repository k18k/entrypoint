package tests

import (
	"strings"
	"testing"

	h "github.com/k18k/entrypoint/e2e/harness"
)

// The e2e environment comes up correctly: systemd on the gateway, Pebble, CoreDNS, clients, chaos.
func TestSmoke_Environment(t *testing.T) {
	e := h.New(t)
	e.SetDNS(map[string]string{"smoke.ep.test": "172.28.0.99"})
	e.ResetGateway(h.Gateway)
	if out, _ := e.Exec(h.Gateway, "systemctl", "is-system-running"); strings.TrimSpace(out) != "running" {
		failed, _ := e.Exec(h.Gateway, "systemctl", "--failed", "--no-legend")
		t.Fatalf("systemd: %s %s", out, failed)
	}
	if _, err := h.PebbleRoots(); err != nil {
		t.Fatalf("Pebble: %v", err)
	}
	if out := e.MustExec(h.Gateway, "getent", "hosts", "smoke.ep.test"); !strings.Contains(out, "172.28.0.99") {
		t.Fatalf("DNS on the gateway: %s", out)
	}
	for _, c := range []string{h.ClientA, h.ClientB, h.Attacker} {
		e.MustExec(c, "frpc", "-v")
		e.MustExec(c, "node", "-e", "import('@k18k/entrypoint-ts').then(m => { if (!m.tunnel) process.exit(1) })")
		e.MustSh(c, "cd /app && node -e \"import('@k18k/entrypoint-ts').then(m => console.log(m.findConnector()))\"")
	}
	if out := e.MustSh(h.Gateway, "curl -s http://"+h.UpstreamVPN+"/ping"); !strings.Contains(out, "vpn-upstream") {
		t.Fatalf("upstream over the private network: %s", out)
	}
	if _, err := h.Run(nil, "docker", "run", "--rm", "--network", "container:"+h.ClientA, "--cap-add", "NET_ADMIN", "ep-e2e-chaos",
		"tc qdisc add dev eth0 root netem delay 10ms && tc qdisc del dev eth0 root"); err != nil {
		t.Fatalf("chaos: %v", err)
	}
}
