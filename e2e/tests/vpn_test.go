package tests

// e2e suite VPN: the built-in VPN (Headscale behind Caddy, the gateway as a node).

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	h "github.com/k18k/entrypoint/e2e/harness"
)

const vpnName = "vpn.ep.test"

var vpnKeyRe = regexp.MustCompile(`--auth-key (\S+)`)

// vpnSetup installs a gateway with the VPN, joins client-a as a node serving HTTP on
// port 8090 and routes v.ep.test to it. It returns the node's address and the key used.
func vpnSetup(t *testing.T) (*h.Env, string, string) {
	e := h.New(t)
	e.StopAllApps()
	e.SetDNS(map[string]string{vpnName: h.GatewayIP, "v.ep.test": h.GatewayIP})
	e.ResetGateway(h.Gateway)
	e.Install(h.InstallOptions{Extra: []string{"--vpn-name", vpnName}})
	// Clients reach the control server only over HTTPS with a public certificate.
	e.WaitHTTPS("https://"+vpnName+"/health", 200)

	m := vpnKeyRe.FindStringSubmatch(e.MustEP("vpn", "key"))
	if m == nil {
		t.Fatal("vpn key: no key in the output")
	}
	key := m[1]
	e.Secrets = append(e.Secrets, key)
	roots, err := h.PebbleRootsPEM()
	if err != nil {
		t.Fatal(err)
	}
	e.MustSh(h.ClientA, "pkill -x tailscaled; [ -f /tmp/ts/node.pid ] && kill $(cat /tmp/ts/node.pid); rm -rf /tmp/ts; mkdir -p /tmp/ts")
	e.WriteFile(h.ClientA, "/tmp/ts/roots.pem", string(roots), "644")
	e.WriteFile(h.ClientA, "/tmp/ts/key", key, "600")
	e.MustSh(h.ClientA, `SSL_CERT_FILE=/tmp/ts/roots.pem setsid tailscaled --tun=userspace-networking --state=/tmp/ts/state --socket=/tmp/ts/sock --port=0 --no-logs-no-support >/tmp/ts/tailscaled.log 2>&1 < /dev/null &
setsid node -e "require('http').createServer((q, s) => s.end('vpn-node')).listen(8090)" >/tmp/ts/node.log 2>&1 < /dev/null &
echo $! > /tmp/ts/node.pid`)
	e.MustSh(h.ClientA, "sleep 1; tailscale --socket=/tmp/ts/sock up --login-server https://"+vpnName+" --auth-key file:/tmp/ts/key --hostname client-a --timeout 60s")
	ip := strings.TrimSpace(e.MustSh(h.ClientA, "tailscale --socket=/tmp/ts/sock ip -4"))
	e.Route("v.ep.test", "--to", ip+":8090")
	e.WaitHTTPS("https://v.ep.test/", 200)
	return e, ip, key
}

func vpnNodes(e *h.Env) map[string]string {
	var nodes []struct {
		ID        int      `json:"id"`
		Name      string   `json:"given_name"`
		Addresses []string `json:"ip_addresses"`
	}
	e.EPJSON(&nodes, "vpn", "nodes", "--json")
	out := map[string]string{}
	for _, n := range nodes {
		out[n.Name] = strings.Join(n.Addresses, " ")
	}
	return out
}

// A machine joins with a key from the CLI and serves through a route to its VPN address;
// the control server's name is reserved; removing the node cuts the route.
func TestVPN_NodeAsUpstream(t *testing.T) {
	e, ip, key := vpnSetup(t)
	c := e.HTTPClient()
	code, body, err := get(c, "https://v.ep.test/")
	nodes := vpnNodes(e)
	_, reservedErr := e.EP("route", "add", vpnName, "--to", h.UpstreamVPN)
	// The key never shows up in the gateway's logs or in process arguments.
	logs := e.Journal(h.Gateway, "entrypoint-headscale") + e.Journal(h.Gateway, "entrypoint-tailscaled") +
		e.MustSh(h.ClientA, "cat /tmp/ts/tailscaled.log; ps -eo args")
	var id string
	for _, l := range strings.Split(e.MustEP("vpn", "nodes"), "\n") {
		if f := strings.Fields(l); len(f) > 1 && f[1] == "client-a" {
			id = f[0]
		}
	}
	e.MustEP("vpn", "rm", id)
	var after int
	e.Eventually(60*time.Second, time.Second, "route to the removed node fails", func() error {
		after, _, _ = get(c, "https://v.ep.test/")
		if after == 200 {
			return fmt.Errorf("still 200")
		}
		return nil
	})
	result(t, "node %s: %d %q %v; nodes %v; route on %s rejected: %v; key in logs: %v; after vpn rm: %d",
		ip, code, body, err, nodes, vpnName, reservedErr != nil, strings.Contains(logs, key), after)
	if code != 200 || body != "vpn-node" || nodes["gateway"] == "" || nodes["client-a"] == "" {
		t.Fatal("no traffic to the VPN node")
	}
	if reservedErr == nil || !strings.Contains(reservedErr.Error(), "reserved for the VPN") {
		t.Fatalf("route on the control server's name: %v", reservedErr)
	}
	if strings.Contains(logs, key) {
		t.Fatal("the join key leaked")
	}
}

// Backup and restore on a fresh container: the VPN comes back with its nodes, the gateway
// keeps its VPN address, and the route to the node works again without rejoining.
func TestVPN_BackupRestore(t *testing.T) {
	e, ip, _ := vpnSetup(t)
	before := vpnNodes(e)
	pw := "backup-password-e2e-789"
	e.MustSh(h.Gateway, "rm -f /shared/vpn.age && ENTRYPOINT_BACKUP_PASSWORD="+pw+" entrypoint backup -o /shared/vpn.age")
	e.ResetGateway(h.Gateway)
	e.Install(h.InstallOptions{})
	e.MustSh(h.Gateway, "ENTRYPOINT_BACKUP_PASSWORD="+pw+" entrypoint restore /shared/vpn.age --yes")
	e.WaitHTTPS("https://v.ep.test/", 200)
	after := vpnNodes(e)
	_, body, _ := get(e.HTTPClient(), "https://v.ep.test/")
	result(t, "node %s; nodes before %v, after %v; body %q", ip, before, after, body)
	if after["gateway"] != before["gateway"] || after["client-a"] != before["client-a"] || body != "vpn-node" {
		t.Fatal("the VPN did not come back from the backup")
	}
}
