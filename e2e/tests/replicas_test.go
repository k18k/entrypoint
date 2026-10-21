package tests

// Experiment 1 and e2e suite 5: frp groups and drain (tests 1.1–1.10).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	h "github.com/k18k/entrypoint/e2e/harness"
)

func twoReplicas(t *testing.T, e *h.Env, mode string, drainMs int) (*h.App, *h.App) {
	_, ca := e.Token("api")
	_, cb := e.Token("api")
	a := e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Mode: mode, DrainMs: drainMs, Wait: true})
	b := e.StartApp(h.AppOptions{Container: h.ClientB, Connect: cb, Replica: "b", Mode: mode, DrainMs: drainMs, Wait: true})
	e.WaitReplicas("api", []string{"a", "b"}, 30*time.Second)
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	return a, b
}

// 1.1: new requests on existing Caddy keep-alives after a replica is withdrawn.
func TestReplicas_1_1_KeepAliveAfterWithdraw_Adapter(t *testing.T) {
	e := apiSetup(t)
	a, _ := twoReplicas(t, e, "adapter", 60000)
	load := e.StartLoad("https://api.ep.test/whoami", 4, 20*time.Millisecond)
	time.Sleep(3 * time.Second)
	if a.MustStats().Requests == 0 {
		t.Fatal("replica a got no traffic before the drain")
	}
	a.Drain(60000)
	a.WaitState("withdrawn", 10*time.Second)
	t0 := time.Now()
	time.Sleep(5 * time.Second)
	after5 := a.MustStats().Requests
	time.Sleep(5 * time.Second)
	after15 := a.MustStats().Requests
	ss := load.Stop()
	errs := h.Count(h.Window(ss, t0.Add(-5*time.Second), time.Now()), func(s h.Sample) bool { return s.Status != 200 })
	result(t, "adapter: requests to a in the 5–15 s window after withdrawal: %d; client errors during the drain: %d of %d", after15-after5, errs, len(h.Window(ss, t0.Add(-5*time.Second), time.Now())))
	if after15 != after5 {
		t.Fatalf("the withdrawn replica got %d new requests after 5 s", after15-after5)
	}
	if errs > 0 {
		t.Fatalf("%d requests failed during the drain", errs)
	}
}

func TestReplicas_1_1_KeepAliveAfterWithdraw_Base(t *testing.T) {
	h.Experiment(t)
	e := apiSetup(t)
	a, _ := twoReplicas(t, e, "base", 10000)
	load := e.StartLoad("https://api.ep.test/whoami", 4, 20*time.Millisecond)
	time.Sleep(3 * time.Second)
	a.Drain(10000)
	a.WaitState("withdrawn", 10*time.Second)
	t0 := time.Now()
	e.Eventually(30*time.Second, 200*time.Millisecond, "replica a frpc exit", func() error {
		if e.Procs(h.ClientA, "frpc") > 0 {
			return errors.New("frpc still running")
		}
		return nil
	})
	tEnd := time.Since(t0)
	before := a.MustStats()
	time.Sleep(5 * time.Second)
	after := a.MustStats()
	ss := load.Stop()
	var lastToA time.Time
	for _, s := range ss {
		if s.Replica == "a" && s.Status == 200 {
			lastToA = s.At
		}
	}
	errs := h.Count(h.Window(ss, t0, time.Now()), func(s h.Sample) bool { return s.Status != 200 })
	result(t, "base contract: last request to a %s after withdrawal; frpc exited after %s; new requests after drainTimeout: %d; client errors: %d",
		ms(lastToA.Sub(t0)), ms(tEnd), after.Requests-before.Requests, errs)
	if after.Requests != before.Requests {
		t.Fatal("replica a gets requests after drainTimeout")
	}
}

// 1.2a: a graceful drain during an upload that fits within drainTimeout.
func TestReplicas_1_2a_UploadDuringDrain(t *testing.T) {
	e := apiSetup(t)
	_, ca := e.Token("api")
	a := e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", DrainMs: 30000, Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	ch := upload(e.HTTPClient(), "https://api.ep.test/upload", 8<<20, 1<<20) // ~8 s
	time.Sleep(2 * time.Second)
	a.Drain(30000)
	r := <-ch
	st := a.MustStats()
	result(t, "8 MB upload during the drain: status %d, time %s, uploadsOK=%d, aborted=%d", r.status, ms(r.took), st.UploadsOK, st.UploadsAborted)
	if r.err != nil || r.status != 200 || !strings.Contains(r.body, r.want) {
		t.Fatalf("upload: err=%v status=%d body=%s", r.err, r.status, r.body)
	}
	if st.UploadsOK != 1 {
		t.Fatal("the app did not receive the complete data")
	}
}

// 1.2b: immediate token revocation during an upload.
func TestReplicas_1_2b_RevokeDuringUpload(t *testing.T) {
	e := apiSetup(t)
	id, ca := e.Token("api")
	a := e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	ch := upload(e.HTTPClient(), "https://api.ep.test/upload", 60<<20, 1<<20) // ~60 s
	time.Sleep(2 * time.Second)
	t0 := time.Now()
	e.MustEP("token", "revoke", id)
	var r uploadResult
	select {
	case r = <-ch:
	case <-time.After(90 * time.Second):
		t.Fatal("the upload was not interrupted")
	}
	took := time.Since(t0)
	time.Sleep(time.Second)
	st := a.MustStats()
	result(t, "revocation during upload: interrupted after %s, status %d, err=%v, uploadsOK=%d, aborted=%d, tunnel state=%s",
		ms(took), r.status, r.err, st.UploadsOK, st.UploadsAborted, st.State)
	if r.err == nil && r.status == 200 {
		t.Fatal("the upload succeeded despite the revocation")
	}
	if st.UploadsOK != 0 || st.UploadsAborted != 1 {
		t.Fatalf("app: uploadsOK=%d aborted=%d", st.UploadsOK, st.UploadsAborted)
	}
	e.Eventually(30*time.Second, 500*time.Millisecond, "connector exits with a revoked event", func() error {
		if !a.MustStats().HasEvent("revoked") {
			return errors.New("no revoked event")
		}
		return nil
	})
}

func readSSE(ctx context.Context, e *h.Env, url string) (<-chan string, <-chan error) {
	lines := make(chan string, 100)
	done := make(chan error, 1)
	go func() {
		c := e.HTTPClient()
		c.Timeout = 0
		req, _ := httpNewRequest(ctx, url)
		resp, err := c.Do(req)
		if err != nil {
			done <- err
			return
		}
		defer resp.Body.Close()
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				lines <- string(buf[:n])
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()
	return lines, done
}

// 1.3: SSE and WebSocket through a replica being withdrawn.
func TestReplicas_1_3_StreamsDuringDrain_Adapter(t *testing.T) {
	testStreams(t, "adapter", 20000)
}

func TestReplicas_1_3_StreamsDuringDrain_Base(t *testing.T) {
	h.Experiment(t)
	testStreams(t, "base", 10000)
}

func testStreams(t *testing.T, mode string, drainMs int) {
	e := apiSetup(t)
	_, ca := e.Token("api")
	a := e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Mode: mode, DrainMs: drainMs, Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "wss://api.ep.test/ws", &websocket.DialOptions{HTTPClient: e.HTTPClient()})
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(ctx, websocket.MessageText, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, m, err := ws.Read(ctx); err != nil || string(m) != "a:x" {
		t.Fatalf("echo WS: %q %v", m, err)
	}
	_, sseDone := readSSE(ctx, e, "https://api.ep.test/sse")
	time.Sleep(time.Second)
	t0 := time.Now()
	a.Drain(drainMs)
	wsErr := make(chan error, 1)
	go func() {
		for {
			if _, _, err := ws.Read(ctx); err != nil {
				wsErr <- err
				return
			}
		}
	}()
	limit := time.Duration(drainMs)*time.Millisecond + 10*time.Second
	var wsE, sseE error
	var wsAt, sseAt time.Duration
	for wsE == nil || sseE == nil {
		select {
		case wsE = <-wsErr:
			wsAt = time.Since(t0)
		case sseE = <-sseDone:
			sseAt = time.Since(t0)
		case <-time.After(limit):
			t.Fatalf("streams were not closed within %s (ws=%v sse=%v)", limit, wsE, sseE)
		}
	}
	code := websocket.CloseStatus(wsE)
	result(t, "%s: WebSocket closed after %s, code %d (%v); SSE ended after %s (%v)", mode, ms(wsAt), code, wsE, ms(sseAt), sseE)
	drain := time.Duration(drainMs) * time.Millisecond
	if mode == "adapter" {
		if code != websocket.StatusGoingAway {
			t.Fatalf("adapter: expected code 1001, got %d", code)
		}
		if wsAt >= drain || sseAt >= drain {
			t.Fatal("adapter: streams closed only after drainTimeout")
		}
	} else {
		if wsAt < drain-time.Second || sseAt < drain-time.Second {
			t.Fatalf("base contract: stream dropped before drainTimeout (ws %s, sse %s)", wsAt, sseAt)
		}
	}
}

// 1.4: withdrawal from the group, draining connections and frpc exit as separate, observable steps.
func TestReplicas_1_4_DrainSteps(t *testing.T) {
	e := apiSetup(t)
	_, ca := e.Token("api")
	a := e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Mode: "base", DrainMs: 8000, Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	c := e.HTTPClient()
	slow := make(chan int, 1)
	go func() {
		code, _, _ := get(c, "https://api.ep.test/slow?ms=4000")
		slow <- code
	}()
	time.Sleep(500 * time.Millisecond)
	t0 := time.Now()
	a.Drain(8000)
	// Step 1: the replica leaves the group, frpc keeps running.
	e.WaitReplicas("api", nil, 5*time.Second)
	t1 := time.Since(t0)
	if e.Procs(h.ClientA, "frpc") == 0 {
		t.Fatal("frpc already exited in step 1")
	}
	// Step 2: a started request finishes correctly after the withdrawal.
	code := <-slow
	t2 := time.Since(t0)
	if code != 200 {
		t.Fatalf("request while draining: status %d", code)
	}
	if e.Procs(h.ClientA, "frpc") == 0 {
		t.Fatal("frpc exited before drainTimeout ended")
	}
	// Step 3: after drainTimeout the connector stops frpc.
	e.Eventually(15*time.Second, 200*time.Millisecond, "frpc exit", func() error {
		if e.Procs(h.ClientA, "frpc") > 0 {
			return errors.New("frpc running")
		}
		return nil
	})
	t3 := time.Since(t0)
	st := a.MustStats()
	result(t, "step 1 (withdrawn, gone from status) after %s; step 2 (4 s request finished with 200) after %s; step 3 (frpc exit) after %s; events: draining=%v withdrawn=%v",
		ms(t1), ms(t2), ms(t3), st.HasEvent("draining"), st.HasEvent("withdrawn"))
	if !st.HasEvent("draining") || !st.HasEvent("withdrawn") {
		t.Fatal("no drain step events")
	}
}

// 1.5: one replica returns errors, the other is healthy.
func TestReplicas_1_5_UnhealthyReplica(t *testing.T) {
	e := apiSetup(t)
	a, _ := twoReplicas(t, e, "adapter", 30000)
	load := e.StartLoad("https://api.ep.test/whoami", 4, 20*time.Millisecond)
	time.Sleep(2 * time.Second)
	t0 := time.Now()
	a.SetHealthy(false)
	removed := e.WaitReplicas("api", []string{"b"}, 20*time.Second)
	limit := 1*time.Second*2 + 5*time.Second // interval × attempts + 5 s
	time.Sleep(10 * time.Second)
	ss := load.Stop()
	var last500 time.Duration
	n500 := 0
	for _, s := range h.Window(ss, t0, time.Now()) {
		if s.Status != 200 {
			n500++
			last500 = s.At.Sub(t0)
		}
	}
	tail := h.Window(ss, t0.Add(limit), time.Now())
	bad := h.Count(tail, func(s h.Sample) bool { return s.Status != 200 })
	result(t, "sick replica removed from the group after %s (limit %s); errors: %d, last after %s; errors after the limit: %d of %d",
		ms(removed), ms(limit), n500, ms(last500), bad, len(tail))
	if removed > limit {
		t.Fatalf("removal took %s", removed)
	}
	if bad > 0 {
		t.Fatalf("%d errors after the sick replica was removed", bad)
	}
}

// 1.6: entrypointd crash under traffic (kill -9, systemd restarts the daemon; systemd holds the plugin socket).
func TestReplicas_1_6_EntrypointdCrash(t *testing.T) {
	e := apiSetup(t)
	_, ca := e.Token("api")
	_, cb := e.Token("api")
	e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	load := e.StartLoad("https://api.ep.test/whoami", 2, 50*time.Millisecond)
	time.Sleep(2 * time.Second)
	t0 := time.Now()
	pidBefore := strings.TrimSpace(e.MustExec(h.Gateway, "systemctl", "show", "-p", "MainPID", "--value", "entrypointd"))
	// Several crashes in a row, including during a new login.
	for i := 0; i < 3; i++ {
		e.MustExec(h.Gateway, "systemctl", "kill", "-s", "KILL", "entrypointd")
		if i == 0 {
			e.StartApp(h.AppOptions{Container: h.ClientB, Connect: cb, Replica: "b"})
		}
		time.Sleep(3 * time.Second)
	}
	e.WaitReplicas("api", []string{"a", "b"}, 30*time.Second)
	back := time.Since(t0)
	time.Sleep(12 * time.Second) // at least one full heartbeat cycle after the crashes
	ss := load.Stop()
	win := h.Window(ss, t0, time.Now())
	bad := h.Count(win, func(s h.Sample) bool { return s.Status != 200 })
	pidAfter := strings.TrimSpace(e.MustExec(h.Gateway, "systemctl", "show", "-p", "MainPID", "--value", "entrypointd"))
	result(t, "3× kill -9 entrypointd under traffic: request errors %d of %d; new replica b logged in %s after the first crash; PID %s -> %s",
		bad, len(win), ms(back), pidBefore, pidAfter)
	if bad > 0 {
		t.Fatalf("%d traffic errors during entrypointd crashes", bad)
	}
	if pidBefore == pidAfter {
		t.Fatal("entrypointd was not restarted")
	}
}

// 1.7: Caddy cold start while entrypointd is down.
func TestReplicas_1_7_CaddyColdStartWithoutDaemon(t *testing.T) {
	e := apiSetup(t)
	_, ca := e.Token("api")
	e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	e.MustExec(h.Gateway, "systemctl", "stop", "entrypointd")
	e.MustExec(h.Gateway, "systemctl", "restart", "entrypoint-caddy")
	t0 := time.Now()
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	result(t, "Caddy serves traffic after %s when restarted without entrypointd", ms(time.Since(t0)))
	e.MustExec(h.Gateway, "systemctl", "start", "entrypointd")
}

// 1.8: removing the last replica.
func TestReplicas_1_8_LastReplicaRemoved(t *testing.T) {
	e := apiSetup(t)
	_, ca := e.Token("api")
	a := e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	load := e.StartLoad("https://api.ep.test/whoami", 2, 50*time.Millisecond)
	time.Sleep(2 * time.Second)
	t0 := time.Now()
	a.Kill9()
	e.Eventually(10*time.Second, 100*time.Millisecond, "502/503 response", func() error {
		for _, s := range h.Window(load.Snapshot(), t0, time.Now()) {
			if s.Status == 502 || s.Status == 503 {
				return nil
			}
		}
		return errors.New("missing")
	})
	time.Sleep(3 * time.Second)
	ss := load.Stop()
	var first5xx time.Duration
	var slowest time.Duration
	for _, s := range h.Window(ss, t0, time.Now()) {
		if (s.Status == 502 || s.Status == 503) && first5xx == 0 {
			first5xx = s.At.Sub(t0)
		}
	}
	// Longest response time after removal: no connection hangs on the removed replica.
	for i := 1; i < len(ss); i++ {
		if ss[i].At.After(t0) {
			if d := ss[i].At.Sub(ss[i-1].At); d > slowest {
				slowest = d
			}
		}
	}
	gone := e.Procs(h.ClientA, "frpc") == 0 && e.Procs(h.ClientA, "entrypoint-connector") == 0
	result(t, "kill -9 of the last replica: first 502/503 after %s; longest gap between responses %s; connector and frpc exited: %v", ms(first5xx), ms(slowest), gone)
	if first5xx > time.Second {
		t.Fatalf("502/503 only after %s", first5xx)
	}
	if slowest > 5*time.Second {
		t.Fatalf("a connection hung for %s", slowest)
	}
	if !gone {
		t.Fatal("the connector or frpc survived the parent's death")
	}
}

// 1.9: restart frps.
func TestReplicas_1_9_FrpsRestart(t *testing.T) {
	h.Experiment(t)
	e := apiSetup(t)
	twoReplicas(t, e, "adapter", 30000)
	port := e.Status().Status.Services[0].Port
	e.MustExec(h.Gateway, "systemctl", "restart", "entrypoint-frps")
	t0 := time.Now()
	back := e.WaitReplicas("api", []string{"a", "b"}, 60*time.Second)
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	samePort := e.Status().Status.Services[0].Port == port
	result(t, "after an frps restart both replicas came back after %s, port unchanged: %v", ms(time.Since(t0)), samePort)
	if back > 30*time.Second {
		t.Fatalf("replicas came back only after %s", back)
	}
}

// 1.10: a new replica with a health check gets no connections before its first passing check.
func TestReplicas_1_10_NoTrafficBeforeHealthy(t *testing.T) {
	h.Experiment(t)
	e := apiSetup(t)
	_, ca := e.Token("api")
	_, cb := e.Token("api")
	e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	b := e.StartApp(h.AppOptions{Container: h.ClientB, Connect: cb, Replica: "b", Unhealthy: true})
	e.Eventually(30*time.Second, 300*time.Millisecond, "replica b control panel", func() error { _, err := b.Stats(); return err })
	load := e.StartLoad("https://api.ep.test/whoami", 4, 20*time.Millisecond)
	time.Sleep(8 * time.Second)
	before := b.MustStats().Requests
	inStatus := e.ReplicaSuffixes("api")
	b.SetHealthy(true)
	b.WaitState("healthy", 20*time.Second)
	e.WaitReplicas("api", []string{"a", "b"}, 20*time.Second)
	time.Sleep(5 * time.Second)
	afterSteady := b.MustStats().Requests
	load.Stop()
	// Under steady traffic Caddy keeps using keep-alive connections to a; the new replica gets
	// traffic only on new connections (e.g. when concurrency grows).
	burst := e.StartLoad("https://api.ep.test/whoami", 24, 10*time.Millisecond)
	time.Sleep(5 * time.Second)
	burst.Stop()
	after := b.MustStats().Requests
	result(t, "after healthy under steady traffic of 4 connections: requests to b %d; after growing to 24 connections: %d", afterSteady, after)
	result(t, "replica with a failing health check: requests before healthy %d, replicas in status %v; requests after healthy %d", before, inStatus, after)
	if before != 0 {
		t.Fatalf("the replica got %d requests before its first passing check", before)
	}
	if after == 0 {
		t.Fatal("the healthy replica gets no traffic")
	}
}
