package tests

// Experiment 3 and e2e suite 7: durable token revocation (3.1, 3.1b, 3.2, 3.3).

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	h "github.com/k18k/entrypoint/e2e/harness"
)

// revokeSetup: an api service with an attacking frpc (token A) and a service b with a replica through the connector (token B).
func revokeSetup(t *testing.T, heartbeat int) (*h.Env, string, string, *h.App) {
	e := apiSetup(t)
	e.SetDNS(map[string]string{"api.ep.test": h.GatewayIP, "b.ep.test": h.GatewayIP})
	e.Service("b", "--health", "/health")
	e.Route("b.ep.test", "--service", "b")
	idA, ca := e.Token("api")
	_, cb := e.Token("b")
	ba, _ := h.DecodeBundle(ca)
	attackerServer(e, 8081)
	e.RawFrpc(h.Attacker, "a", ba, fmt.Sprintf(`[{"name": "api.%s.atk", "type": "tcp", "localIP": "127.0.0.1", "localPort": 8081}]`, ba.TokenID()), heartbeat)
	appB := e.StartApp(h.AppOptions{Container: h.ClientB, Connect: cb, Service: "b", Replica: "b", Wait: true})
	e.WaitHTTPS("https://api.ep.test/", 200)
	e.WaitHTTPS("https://b.ep.test/whoami", 200)
	return e, idA, ba.TokenID(), appB
}

// proxyOnline: an unavailable frps API (restart) means the proxy is not online.
func proxyOnline(e *h.Env, name string) bool {
	m, err := e.TryFrpsProxies(h.Gateway)
	return err == nil && m[name] == "online"
}

func testRevokeSession(t *testing.T, heartbeat int, now bool) {
	e, idA, tidA, appB := revokeSetup(t, heartbeat)
	nameA := "api." + tidA + ".atk"
	loadB := e.StartLoad("https://b.ep.test/whoami", 2, 50*time.Millisecond)
	time.Sleep(2 * time.Second)
	args := []string{"token", "revoke", idA}
	if now {
		args = append(args, "--now")
	}
	t0 := time.Now()
	e.MustEP(args...)
	e.Eventually(90*time.Second, 250*time.Millisecond, "session A dropped", func() error {
		if proxyOnline(e, nameA) {
			return errors.New("proxy A still online")
		}
		return nil
	})
	kicked := time.Since(t0)
	// A does not reconnect (frpc retries the login every few seconds).
	time.Sleep(15 * time.Second)
	reconnected := proxyOnline(e, nameA)
	ss := loadB.Stop()
	win := h.Window(ss, t0, time.Now())
	bad := h.Count(win, func(s h.Sample) bool { return s.Status != 200 })
	var gapEnd time.Duration
	for _, s := range win {
		if s.Status != 200 {
			gapEnd = s.At.Sub(t0)
		}
	}
	st := appB.MustStats()
	frpcLog := e.RawFrpcLog(h.Attacker, "a")
	rejected := strings.Contains(frpcLog, "ep:revoked")
	mode := "Ping"
	if now {
		mode = "restart frps (--now)"
	}
	result(t, "%s, heartbeat A=%d: session A dropped after %s; A reconnected after 30 s: %v; A logins rejected with ep:revoked: %v; traffic B: errors %d of %d (last after %s), B disconnects: %v",
		mode, heartbeat, ms(kicked), reconnected, rejected, bad, len(win), ms(gapEnd), st.HasEvent("disconnected"))
	if reconnected {
		t.Fatal("the revoked token reconnected")
	}
	if now {
		if gapEnd > 10*time.Second {
			t.Fatalf("B came back only after %s", gapEnd)
		}
	} else if bad > 0 || st.HasEvent("disconnected") {
		t.Fatal("revoking A interrupted B")
	}
}

// 3.1: client A (frpc without the connector, with heartbeat) and B; revoke token A.
func TestRevoke_3_1_KickWithPing(t *testing.T) { testRevokeSession(t, 10, false) }

// 3.1: A disables heartbeats (uncooperative); the gateway drops the session anyway.
func TestRevoke_3_1_KickSilentClient(t *testing.T) {
	h.Experiment(t)
	testRevokeSession(t, -1, false)
}

// 3.1 (fallback): immediate drop by restarting frps.
func TestRevoke_3_1_KickWithFrpsRestart(t *testing.T) { testRevokeSession(t, 10, true) }

// 3.1b: Ping rejection was chosen, so measure how a long entrypointd outage affects sessions.
func TestRevoke_3_1b_LongDaemonOutage(t *testing.T) {
	h.Experiment(t)
	e := apiSetup(t)
	_, ca := e.Token("api")
	a := e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	load := e.StartLoad("https://api.ep.test/whoami", 2, 50*time.Millisecond)
	time.Sleep(2 * time.Second)
	// Full outage: including the socket held by systemd.
	e.MustExec(h.Gateway, "systemctl", "stop", "entrypointd.socket", "entrypointd.service")
	t0 := time.Now()
	time.Sleep(45 * time.Second)
	t1 := time.Now()
	e.MustExec(h.Gateway, "systemctl", "start", "entrypointd.socket", "entrypointd.service")
	e.Eventually(60*time.Second, 300*time.Millisecond, "replica comes back", func() error {
		s := a.MustStats()
		if s.EventTime("healthy") > t1.UnixMilli() {
			return nil
		}
		return errors.New("not yet")
	})
	back := time.Since(t1)
	ss := load.Stop()
	st := a.MustStats()
	var firstErr time.Duration = -1
	for _, s := range h.Window(ss, t0, t1) {
		if s.Status != 200 {
			firstErr = s.At.Sub(t0)
			break
		}
	}
	errs := h.Count(h.Window(ss, t0, time.Now()), func(s h.Sample) bool { return s.Status != 200 })
	disc := st.EventTime("disconnected")
	result(t, "entrypointd and its socket stopped for 45 s: session dropped after %s, first traffic error after %s, %d errors in total; after the start the replica is healthy after %s",
		ms(time.Duration(disc-t0.UnixMilli())*time.Millisecond), ms(firstErr), errs, ms(back))
	if back > 30*time.Second {
		t.Fatalf("after entrypointd came back the replica returned only after %s", back)
	}
}

// 3.2: revocation while a revision is partly applied, then a rollback.
func TestRevoke_3_2_RevokeDuringFailedApply(t *testing.T) {
	e := apiSetup(t, "--test-apply-delay", "6s")
	idA, ca := e.Token("api")
	_, cc := e.Token("api")
	a := e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Wait: true})
	var before struct {
		Status struct {
			LastApplied struct{ Num int64 }
		}
	}
	e.EPJSON(&before, "status")
	// Revision N: applying pauses after the snapshot is written (test mode).
	done := make(chan error, 1)
	go func() {
		_, err := e.EP("route", "add", "x.ep.test", "--to", h.UpstreamVPN)
		done <- err
	}()
	time.Sleep(3 * time.Second)
	// Meanwhile: revoke token A, a new session C, a Caddy failure.
	e.MustEP("token", "revoke", idA)
	c := e.StartApp(h.AppOptions{Container: h.ClientB, Connect: cc, Replica: "c", Wait: true})
	e.MustExec(h.Gateway, "systemctl", "stop", "entrypoint-caddy")
	err := <-done
	e.MustExec(h.Gateway, "systemctl", "start", "entrypoint-caddy")
	var revs []struct {
		Num    int64
		Status string
		Error  string
	}
	e.EPJSON(&revs, "revision", "list")
	st := e.Status()
	// The heartbeat checks sessions against the restored snapshot: A is dropped.
	e.Eventually(30*time.Second, 300*time.Millisecond, "session A dropped", func() error {
		if !a.MustStats().HasEvent("revoked") {
			return errors.New("A still connected")
		}
		return nil
	})
	aStats := a.MustStats()
	cStats := c.MustStats()
	var toks []struct {
		ID      string
		Revoked bool
	}
	e.EPJSON(&toks, "token", "list")
	revoked := false
	for _, tk := range toks {
		if tk.ID == idA {
			revoked = tk.Revoked
		}
	}
	reps := e.ReplicaSuffixes("api")
	result(t, "revision %d: status %s (%s); snapshot after rollback = revision %d (previous %d); token A revoked after rollback: %v, A dropped: %v; session C opened meanwhile checked and still active: %v; replicas: %v; CLI error: %v",
		revs[0].Num, revs[0].Status, revs[0].Error, st.Status.AuthzRevision, before.Status.LastApplied.Num, revoked, aStats.HasEvent("revoked"), !cStats.HasEvent("disconnected"), reps, err != nil)
	if revs[0].Status != "failed" || st.Status.AuthzRevision != before.Status.LastApplied.Num {
		t.Fatal("the revision was not rolled back")
	}
	if !revoked || !aStats.HasEvent("revoked") {
		t.Fatal("the revocation did not survive the rollback")
	}
	if strings.Join(reps, ",") != "c" {
		t.Fatalf("replicas after rollback: %v", reps)
	}
}

// 3.3: backup at 10:00, revocation at 11:00, machine lost, restore from 10:00.
func TestRevoke_3_3_RestoreInvalidatesTokens(t *testing.T) {
	e := apiSetup(t)
	idA, ca := e.Token("api")
	_, cb := e.Token("api")
	a := e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	pw := "backup-password-e2e-123"
	e.MustSh(h.Gateway, "cd /shared && rm -f b.age && ENTRYPOINT_BACKUP_PASSWORD="+pw+" entrypoint backup -o /shared/b.age")
	e.MustEP("token", "revoke", idA) // "11:00": revocation after the backup
	a.WaitState("revoked", 60*time.Second)
	e.StopAllApps()
	// Machine lost; a new machine at a different IP address, same gateway name.
	e.StopContainer(h.Gateway)
	e.SetDNSWithGateway(h.Gateway2IP, map[string]string{"api.ep.test": h.Gateway2IP})
	e.ResetGateway(h.Gateway2)
	e.Install(h.InstallOptions{Container: h.Gateway2, PublicIP: h.Gateway2IP})
	out, err := e.Exec(h.Gateway2, "sh", "-c", "ENTRYPOINT_BACKUP_PASSWORD="+pw+" entrypoint restore /shared/b.age --yes")
	if err != nil {
		t.Fatal(err)
	}
	// Before the first login: both restored tokens (including B, not revoked before the loss) are invalidated.
	var toks []struct {
		ID      string
		Revoked bool
	}
	s, _ := e.EPOn(h.Gateway2, "token", "list", "--json")
	_ = jsonUnmarshal(s, &toks)
	allRevoked := len(toks) == 2
	for _, tk := range toks {
		allRevoked = allRevoked && tk.Revoked
	}
	e.StartApp(h.AppOptions{Container: h.ClientB, Connect: cb, Replica: "b"})
	var bLog string
	e.Eventually(30*time.Second, 500*time.Millisecond, "old token B rejected", func() error {
		bLog, _ = e.Sh(h.ClientB, "cat /tmp/app-b.log")
		if strings.Contains(bLog, "start failed") {
			return nil
		}
		return errors.New("app b still running")
	})
	bHealthy := strings.Contains(bLog, `"tunnelEvent":"healthy"`) || strings.Contains(bLog, `"ready":true`)
	// A new token works, the HTTPS certificate from the backup serves traffic at the new IP.
	var res struct {
		Token struct{ Connect string }
	}
	s2, _ := e.EPOn(h.Gateway2, "token", "create", "--service", "api", "--json")
	_ = jsonUnmarshal(s2, &res)
	e.Secrets = append(e.Secrets, res.Token.Connect)
	e.StartApp(h.AppOptions{Container: h.ClientA, Connect: res.Token.Connect, Replica: "n", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	result(t, "after restore: restored tokens invalidated: %v (%d); old token B rejected: %v (healthy=%v); new token works at the new IP; restore output: %s",
		allRevoked, len(toks), strings.Contains(bLog, "revoked"), bHealthy, strings.ReplaceAll(strings.TrimSpace(out), "\n", " | "))
	if !allRevoked || bHealthy {
		t.Fatal("a restored token logged in after restore")
	}
	// Cleanup: go back to the gateway at the original address for the next tests.
	e.StopContainer(h.Gateway2)
}

// Issue #1: entrypointd dies right after a revoke (revocation already durable, session not yet
// dropped). A modified client (raw frpc, no connector) keeps a long-lived connection open.
// After restart the session must be dropped within heartbeatTimeout and re-login refused.
func TestRevoke_CrashRightAfterRevoke(t *testing.T) {
	e, idA, tidA, _ := revokeSetup(t, 10)
	nameA := "api." + tidA + ".atk"
	// Long-lived streaming response through the revoked client's tunnel.
	c := e.HTTPClient()
	c.Timeout = 0
	resp, err := c.Get("https://api.ep.test/stream")
	if err != nil {
		t.Fatal(err)
	}
	streamDone := make(chan time.Time, 1)
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := resp.Body.Read(buf); err != nil {
				streamDone <- time.Now()
				return
			}
		}
	}()
	e.MustEP("token", "revoke", idA)
	t0 := time.Now()
	e.MustExec(h.Gateway, "systemctl", "kill", "-s", "KILL", "entrypointd")
	var ended time.Time
	select {
	case ended = <-streamDone:
	case <-time.After(60 * time.Second):
		t.Fatal("stream of revoked token still open after 60 s")
	}
	e.Eventually(30*time.Second, 250*time.Millisecond, "proxy of revoked token offline", func() error {
		if proxyOnline(e, nameA) {
			return errors.New("still online")
		}
		return nil
	})
	time.Sleep(15 * time.Second)
	reconnected := proxyOnline(e, nameA)
	rejected := strings.Contains(e.RawFrpcLog(h.Attacker, "a"), "ep:revoked")
	result(t, "revoke + kill -9 entrypointd: stream closed after %s, reconnected=%v, re-login refused with ep:revoked=%v",
		ms(ended.Sub(t0)), reconnected, rejected)
	if ended.Sub(t0) > 45*time.Second || reconnected || !rejected {
		t.Fatal("revocation not enforced after daemon crash")
	}
}
