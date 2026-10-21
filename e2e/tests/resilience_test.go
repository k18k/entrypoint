package tests

// e2e suite 11 (v0.1 part): resilience to client network failures and Caddy restarts.

import (
	"context"
	"errors"
	"fmt"
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
	h.Experiment(t)
	testCut(t, 5*time.Second)
}
func TestResilience_NetworkCut60s(t *testing.T) {
	h.Experiment(t)
	testCut(t, 60*time.Second)
}

func TestResilience_PacketLoss(t *testing.T) {
	h.Experiment(t)
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
	h.Experiment(t)
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
	_ = fmt.Sprint
}
