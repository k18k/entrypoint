package tests

// e2e suite 11: resilience to failures of entrypointd, frps and API clients, Caddy restarts,
// client network cuts and packet loss.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	h "github.com/k18k/entrypoint/e2e/harness"
)

// chaos runs a network command in the client container's network namespace.
func chaos(e *h.Env, container, cmd string) {
	e.T.Helper()
	if _, err := h.Run(nil, "docker", "run", "--rm", "--network", "container:"+container, "--cap-add", "NET_ADMIN", "ep-e2e-chaos", cmd); err != nil {
		e.T.Fatal(err)
	}
}

func cutNetwork(e *h.Env, container string, d time.Duration) {
	chaos(e, container, "iptables -I OUTPUT -d "+h.GatewayIP+" -j DROP && iptables -I INPUT -s "+h.GatewayIP+" -j DROP")
	time.Sleep(d)
	chaos(e, container, "iptables -D OUTPUT -d "+h.GatewayIP+" -j DROP && iptables -D INPUT -s "+h.GatewayIP+" -j DROP")
}

func testCut(t *testing.T, d time.Duration) {
	e := apiSetup(t)
	_, connect := e.Token("api")
	a := e.StartApp(h.AppOptions{Container: h.ClientA, Connect: connect, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	load := e.StartLoad("https://api.ep.test/whoami", 2, 100*time.Millisecond)
	time.Sleep(2 * time.Second)
	t0 := time.Now()
	cutNetwork(e, h.ClientA, d)
	t1 := time.Now()
	e.Eventually(90*time.Second, 200*time.Millisecond, "traffic after the network is back", func() error {
		for _, s := range h.Window(load.Snapshot(), t1, time.Now()) {
			if s.Status == 200 && s.At.After(t1.Add(500*time.Millisecond)) {
				return nil
			}
		}
		return errors.New("no successful responses")
	})
	recovered := time.Now()
	time.Sleep(3 * time.Second)
	ss := load.Stop()
	var firstOK time.Duration
	for _, s := range h.Window(ss, t1, time.Now()) {
		if s.Status == 200 {
			firstOK = s.At.Sub(t1)
			break
		}
	}
	after := h.Window(ss, recovered.Add(time.Second), time.Now())
	bad := h.Count(after, func(s h.Sample) bool { return s.Status != 200 })
	st := a.MustStats()
	result(t, "client network cut for %s: first successful response %s after it came back; session dropped: %v; errors after return: %d of %d",
		d, ms(firstOK), st.HasEvent("disconnected"), bad, len(after))
	_ = t0
	if bad > 0 {
		t.Fatal("errors after the network came back")
	}
	if firstOK > 30*time.Second {
		t.Fatalf("the tunnel came back only after %s", firstOK)
	}
}

func TestResilience_NetworkCut5s(t *testing.T) {
	testCut(t, 5*time.Second)
}
func TestResilience_NetworkCut60s(t *testing.T) {
	testCut(t, 60*time.Second)
}

func TestResilience_PacketLoss(t *testing.T) {
	e := apiSetup(t)
	_, connect := e.Token("api")
	e.StartApp(h.AppOptions{Container: h.ClientA, Connect: connect, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	chaos(e, h.ClientA, "tc qdisc add dev eth0 root netem loss 10% delay 50ms 20ms")
	defer chaos(e, h.ClientA, "tc qdisc del dev eth0 root")
	load := e.StartLoad("https://api.ep.test/whoami", 4, 50*time.Millisecond)
	time.Sleep(30 * time.Second)
	ss := load.Stop()
	bad := h.Count(ss, func(s h.Sample) bool { return s.Status != 200 })
	result(t, "10%% loss and 50±20 ms delay on the client link: errors %d of %d", bad, len(ss))
	if float64(bad) > 0.01*float64(len(ss)) {
		t.Fatalf("too many errors: %d of %d", bad, len(ss))
	}
}

// A Caddy restart drops connections; HTTP comes back and the WebSocket client must reconnect.
func TestResilience_CaddyRestart(t *testing.T) {
	e := apiSetup(t)
	_, connect := e.Token("api")
	e.StartApp(h.AppOptions{Container: h.ClientA, Connect: connect, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "wss://api.ep.test/ws", &websocket.DialOptions{HTTPClient: e.HTTPClient()})
	if err != nil {
		t.Fatal(err)
	}
	load := e.StartLoad("https://api.ep.test/whoami", 2, 50*time.Millisecond)
	time.Sleep(time.Second)
	t0 := time.Now()
	e.MustExec(h.Gateway, "systemctl", "restart", "entrypoint-caddy")
	_, _, wsErr := ws.Read(ctx)
	wsDrop := time.Since(t0)
	var reconnect time.Duration
	e.Eventually(30*time.Second, 200*time.Millisecond, "WebSocket reconnect", func() error {
		c, _, err := websocket.Dial(ctx, "wss://api.ep.test/ws", &websocket.DialOptions{HTTPClient: e.HTTPClient()})
		if err != nil {
			return err
		}
		c.Close(websocket.StatusNormalClosure, "")
		reconnect = time.Since(t0)
		return nil
	})
	time.Sleep(2 * time.Second)
	ss := load.Stop()
	var lastBad time.Duration
	for _, s := range h.Window(ss, t0, time.Now()) {
		if s.Status != 200 {
			lastBad = s.At.Sub(t0)
		}
	}
	result(t, "Caddy restart: WebSocket dropped after %s (%v), reconnect possible after %s; last HTTP error after %s", ms(wsDrop), wsErr != nil, ms(reconnect), ms(lastBad))
	if wsErr == nil || reconnect > 15*time.Second || lastBad > 15*time.Second {
		t.Fatal("Caddy restart: outage too long")
	}
}

// recovery measures a failure under load: errors during it, and the time until the last error.
func recovery(e *h.Env, url string, fail func()) (bad, total int, lastBad time.Duration) {
	load := e.StartLoad(url, 2, 50*time.Millisecond)
	time.Sleep(2 * time.Second)
	t0 := time.Now()
	fail()
	time.Sleep(20 * time.Second)
	ss := h.Window(load.Stop(), t0, time.Now())
	for _, s := range ss {
		if s.Status != 200 {
			bad++
			lastBad = s.At.Sub(t0)
		}
	}
	return bad, len(ss), lastBad
}

// kill -9 of entrypointd: public traffic and existing tunnels do not notice; systemd restarts it
// and configuration changes and new tunnel logins work again.
func TestResilience_EntrypointdCrash(t *testing.T) {
	e := apiSetup(t)
	_, connect := e.Token("api")
	e.StartApp(h.AppOptions{Container: h.ClientA, Connect: connect, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	bad, total, _ := recovery(e, "https://api.ep.test/whoami", func() {
		e.MustExec(h.Gateway, "systemctl", "kill", "-s", "KILL", "entrypointd.service")
	})
	var back time.Duration
	t0 := time.Now()
	e.Eventually(30*time.Second, 200*time.Millisecond, "entrypointd answers", func() error {
		_, err := e.EP("status")
		back = time.Since(t0)
		return err
	})
	e.Service("after", "--health", "/health")
	_, c2 := e.Token("api")
	e.StartApp(h.AppOptions{Container: h.ClientB, Connect: c2, Replica: "b", Wait: true})
	result(t, "kill -9 entrypointd under load: %d errors of %d; API back within %s; change and new tunnel login after it: ok", bad, total, ms(back))
	if bad > 0 {
		t.Fatal("public traffic noticed the entrypointd crash")
	}
}

// frps restart: every tunnel drops; connectors log in again on their own.
func TestResilience_FrpsRestart(t *testing.T) {
	e := apiSetup(t)
	_, connect := e.Token("api")
	a := e.StartApp(h.AppOptions{Container: h.ClientA, Connect: connect, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	bad, total, lastBad := recovery(e, "https://api.ep.test/whoami", func() {
		e.MustExec(h.Gateway, "systemctl", "restart", "entrypoint-frps")
	})
	st := a.MustStats()
	result(t, "frps restart under load: %d errors of %d, last error %s after the restart; the connector reconnected: %v",
		bad, total, ms(lastBad), st.HasEvent("disconnected") && st.State == "healthy")
	if lastBad > 15*time.Second {
		t.Fatalf("tunnels came back only after %s", lastBad)
	}
	if st.State != "healthy" {
		t.Fatalf("the replica did not come back: %s", st.State)
	}
}

// API clients that die mid-request or hold the socket open (a stuck CLI or panel) do not block
// entrypointd: the CLI keeps answering and changes still apply.
func TestResilience_APIClientFailure(t *testing.T) {
	e := apiSetup(t)
	// 50 clients send half a request and hang; 20 send a body shorter than announced and die.
	e.MustSh(h.Gateway, `cd /tmp && for i in $(seq 1 50); do (printf 'GET /v1/status HTTP/1.1
Host: x
'; sleep 30) | nc -U /run/entrypoint/api.sock >/dev/null 2>&1 & done
for i in $(seq 1 20); do printf 'POST /v1/services HTTP/1.1
Host: x
Content-Type: application/json
Content-Length: 500

{"name":' | timeout 1 nc -U /run/entrypoint/api.sock >/dev/null 2>&1; done; true`)
	t0 := time.Now()
	_, err := e.EP("status")
	took := time.Since(t0)
	e.Service("still-works")
	services := e.MustEP("service", "list")
	e.MustSh(h.Gateway, "pkill -f '[n]c -U /run/entrypoint/api.sock' || true")
	result(t, "50 stuck and 20 aborted API clients: CLI status in %s (err %v); a change applied afterwards: %v",
		ms(took), err, strings.Contains(services, "still-works"))
	if err != nil || took > 3*time.Second || !strings.Contains(services, "still-works") {
		t.Fatal("broken API clients blocked entrypointd")
	}
	if strings.Contains(services, "\"name\":") {
		t.Fatal("a truncated request created something")
	}
}
