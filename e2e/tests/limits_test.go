package tests

// e2e suite Limits: per-IP connection limits on the public side (nftables) and isolation of
// services under a connection flood (issues #2 and #8).

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	h "github.com/k18k/entrypoint/e2e/harness"
)

// flood.js opens N connections to the gateway, holds them, and reports how many stayed open.
const floodJS = `
const net = require("net");
const [host, port, n, holdMs] = process.argv.slice(2);
let open = 0, refused = 0, timedOut = 0, peak = 0;
const socks = [];
for (let i = 0; i < Number(n); i++) {
  const s = net.connect({ host, port: Number(port) });
  s.setTimeout(5000);
  s.on("connect", () => { open++; peak = Math.max(peak, open); });
  s.on("timeout", () => { timedOut++; s.destroy(); });
  s.on("error", () => { refused++; });
  s.on("close", (hadErr) => { if (s.connecting === false && !hadErr) open--; });
  socks.push(s);
}
setTimeout(() => {
  const alive = socks.filter((s) => !s.destroyed && s.readyState === "open").length;
  console.log(JSON.stringify({ alive, peak, refused, timedOut }));
  process.exit(0);
}, Number(holdMs));
`

// Thresholds fixed before measuring: with 40 connections per IP the attacker holds at most 40;
// service B has no errors and p95 within its baseline + 200 ms; service A still answers other clients.
func TestLimits_ConnectionFloodPerIP(t *testing.T) {
	e := h.New(t)
	e.StopAllApps()
	t.Cleanup(e.StopAllApps)
	e.SetDNS(map[string]string{"a.ep.test": h.GatewayIP, "b.ep.test": h.GatewayIP})
	e.ResetGateway(h.Gateway)
	e.Install(h.InstallOptions{Extra: []string{"--per-ip-connections", "40", "--per-ip-rate", "50"}})
	for _, s := range []string{"a", "b"} {
		e.Service(s, "--health", "/health")
		e.Route(s+".ep.test", "--service", s)
	}
	_, ca := e.Token("a")
	_, cb := e.Token("b")
	e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Service: "a", Replica: "a", Wait: true})
	e.StartApp(h.AppOptions{Container: h.ClientB, Connect: cb, Service: "b", Replica: "b", Wait: true, CtlPort: 9001})
	e.WaitHTTPS("https://a.ep.test/whoami", 200)
	e.WaitHTTPS("https://b.ep.test/whoami", 200)

	load := e.StartLoad("https://b.ep.test/whoami", 2, 50*time.Millisecond)
	time.Sleep(5 * time.Second)
	baseline := h.P95(load.Snapshot())
	t0 := time.Now()
	e.WriteFile(h.Attacker, "/tmp/flood.js", floodJS, "644")
	out := e.MustSh(h.Attacker, "node /tmp/flood.js "+h.GatewayIP+" 443 300 15000")
	aCode, _, aErr := get(e.HTTPClient(), "https://a.ep.test/whoami")
	ss := load.Stop()
	win := h.Window(ss, t0, time.Now())
	bad := h.Count(win, func(s h.Sample) bool { return s.Status != 200 })
	during := h.P95(win)
	var fl struct{ Alive, Peak, Refused, TimedOut int }
	json.Unmarshal([]byte(strings.TrimSpace(out)), &fl)
	rules := e.MustExec(h.Gateway, "nft", "list", "table", "inet", "entrypoint")
	result(t, "attacker: %d connections open at the end (peak %d), %d refused, %d timed out; service B: %d errors of %d, p95 %s (baseline %s); service A for other clients: %d %v; reject counters: %s",
		fl.Alive, fl.Peak, fl.Refused, fl.TimedOut, bad, len(win), ms(during), ms(baseline), aCode, aErr, counters(rules))
	if fl.Peak > 40 || fl.Refused+fl.TimedOut < 200 {
		t.Fatal("the per-IP connection limit did not hold")
	}
	if bad > 0 || during > baseline+200*time.Millisecond {
		t.Fatal("service B degraded by the flood on A")
	}
	if aErr != nil || aCode != 200 {
		t.Fatal("service A unavailable to other clients")
	}
}

// counters returns the packet counts of the rules that drop or reject.
func counters(rules string) string {
	var out []string
	for _, l := range strings.Split(rules, "\n") {
		if strings.Contains(l, "counter packets") {
			f := strings.Fields(l[strings.Index(l, "counter packets"):])
			out = append(out, f[2]+" "+f[len(f)-1])
		}
	}
	return strings.Join(out, ", ")
}
