package tests

// Eksperyment 3 i zestaw e2e 7: trwałe odwołanie tokenów (3.1, 3.1b, 3.2, 3.3).

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	h "github.com/k18k/entrypoint/e2e/harness"
)

// revokeSetup: serwis api z atakującym frpc (token A) i serwis b z repliką przez konektor (token B).
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

// proxyOnline: niedostępne API frps (restart) oznacza, że proxy nie jest online.
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
	e.Eventually(90*time.Second, 250*time.Millisecond, "zerwanie sesji A", func() error {
		if proxyOnline(e, nameA) {
			return errors.New("proxy A nadal online")
		}
		return nil
	})
	kicked := time.Since(t0)
	// A nie łączy się ponownie (frpc ponawia logowanie co kilka sekund).
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
	result(t, "%s, heartbeat A=%d: sesja A zerwana po %s; A połączony ponownie po 30 s: %v; logowania A odrzucane z ep:revoked: %v; ruch B: błędy %d z %d (ostatni po %s), rozłączenia B: %v",
		mode, heartbeat, ms(kicked), reconnected, rejected, bad, len(win), ms(gapEnd), st.HasEvent("disconnected"))
	if reconnected {
		t.Fatal("odwołany token połączył się ponownie")
	}
	if now {
		if gapEnd > 10*time.Second {
			t.Fatalf("B wrócił dopiero po %s", gapEnd)
		}
	} else if bad > 0 || st.HasEvent("disconnected") {
		t.Fatal("odwołanie A przerwało B")
	}
}

// 3.1: klient A (frpc bez konektora, z heartbeat) i B; odwołanie tokenu A.
func TestRevoke_3_1_KickWithPing(t *testing.T) { testRevokeSession(t, 10, false) }

// 3.1: A wyłącza heartbeat (nie współpracuje); brama i tak zrywa sesję.
func TestRevoke_3_1_KickSilentClient(t *testing.T) {
	h.Experiment(t)
	testRevokeSession(t, -1, false)
}

// 3.1 (rezerwowo): natychmiastowe zerwanie restartem frps.
func TestRevoke_3_1_KickWithFrpsRestart(t *testing.T) { testRevokeSession(t, 10, true) }

// 3.1b: wybrano odrzucanie Ping, więc mierzymy wpływ długiej niedostępności entrypointd na sesje.
func TestRevoke_3_1b_LongDaemonOutage(t *testing.T) {
	h.Experiment(t)
	e := apiSetup(t)
	_, ca := e.Token("api")
	a := e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	load := e.StartLoad("https://api.ep.test/whoami", 2, 50*time.Millisecond)
	time.Sleep(2 * time.Second)
	// Pełna niedostępność: także gniazdo trzymane przez systemd.
	e.MustExec(h.Gateway, "systemctl", "stop", "entrypointd.socket", "entrypointd.service")
	t0 := time.Now()
	time.Sleep(45 * time.Second)
	t1 := time.Now()
	e.MustExec(h.Gateway, "systemctl", "start", "entrypointd.socket", "entrypointd.service")
	e.Eventually(60*time.Second, 300*time.Millisecond, "replika wraca", func() error {
		s := a.MustStats()
		if s.EventTime("healthy") > t1.UnixMilli() {
			return nil
		}
		return errors.New("jeszcze nie")
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
	result(t, "entrypointd i gniazdo zatrzymane na 45 s: sesja zerwana po %s, pierwszy błąd ruchu po %s, błędów łącznie %d; po starcie replika zdrowa po %s",
		ms(time.Duration(disc-t0.UnixMilli())*time.Millisecond), ms(firstErr), errs, ms(back))
	if back > 30*time.Second {
		t.Fatalf("po powrocie entrypointd replika wróciła dopiero po %s", back)
	}
}

// 3.2: odwołanie w trakcie częściowego stosowania rewizji, potem rollback.
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
	// Rewizja N: stosowanie zatrzymuje się po zapisie snapshotu (tryb testowy).
	done := make(chan error, 1)
	go func() {
		_, err := e.EP("route", "add", "x.ep.test", "--to", h.UpstreamVPN)
		done <- err
	}()
	time.Sleep(3 * time.Second)
	// W trakcie: odwołanie tokenu A, nowa sesja C, awaria Caddy.
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
	// Heartbeat sprawdza sesje wobec przywróconego snapshotu: A zostaje zerwany.
	e.Eventually(30*time.Second, 300*time.Millisecond, "zerwanie sesji A", func() error {
		if !a.MustStats().HasEvent("revoked") {
			return errors.New("A jeszcze połączony")
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
	result(t, "rewizja %d: status %s (%s); snapshot po rollbacku = rewizja %d (poprzednia %d); token A odwołany po rollbacku: %v, A zerwany: %v; sesja C otwarta w trakcie sprawdzona i nadal aktywna: %v; repliki: %v; błąd CLI: %v",
		revs[0].Num, revs[0].Status, revs[0].Error, st.Status.AuthzRevision, before.Status.LastApplied.Num, revoked, aStats.HasEvent("revoked"), !cStats.HasEvent("disconnected"), reps, err != nil)
	if revs[0].Status != "failed" || st.Status.AuthzRevision != before.Status.LastApplied.Num {
		t.Fatal("rewizja nie została wycofana")
	}
	if !revoked || !aStats.HasEvent("revoked") {
		t.Fatal("odwołanie nie przetrwało rollbacku")
	}
	if strings.Join(reps, ",") != "c" {
		t.Fatalf("repliki po rollbacku: %v", reps)
	}
}

// 3.3: backup o 10:00, odwołanie o 11:00, utrata maszyny, restore z 10:00.
func TestRevoke_3_3_RestoreInvalidatesTokens(t *testing.T) {
	e := apiSetup(t)
	idA, ca := e.Token("api")
	_, cb := e.Token("api")
	a := e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	pw := "haslo-backupu-e2e-123"
	e.MustSh(h.Gateway, "cd /shared && rm -f b.age && ENTRYPOINT_BACKUP_PASSWORD="+pw+" entrypoint backup -o /shared/b.age")
	e.MustEP("token", "revoke", idA) // „11:00”: odwołanie po backupie
	a.WaitState("revoked", 60*time.Second)
	e.StopAllApps()
	// Utrata maszyny; nowa maszyna pod innym adresem IP, ta sama nazwa bramy.
	e.StopContainer(h.Gateway)
	e.SetDNSWithGateway(h.Gateway2IP, map[string]string{"api.ep.test": h.Gateway2IP})
	e.ResetGateway(h.Gateway2)
	e.Install(h.InstallOptions{Container: h.Gateway2, PublicIP: h.Gateway2IP})
	out, err := e.Exec(h.Gateway2, "sh", "-c", "ENTRYPOINT_BACKUP_PASSWORD="+pw+" entrypoint restore /shared/b.age --yes")
	if err != nil {
		t.Fatal(err)
	}
	// Przed pierwszym logowaniem: oba odtworzone tokeny (także B, nieodwołany przed utratą) są unieważnione.
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
	e.Eventually(30*time.Second, 500*time.Millisecond, "odrzucenie starego tokenu B", func() error {
		bLog, _ = e.Sh(h.ClientB, "cat /tmp/app-b.log")
		if strings.Contains(bLog, "start nie powiódł się") {
			return nil
		}
		return errors.New("aplikacja b jeszcze działa")
	})
	bHealthy := strings.Contains(bLog, `"tunnelEvent":"healthy"`) || strings.Contains(bLog, `"ready":true`)
	// Nowy token działa, certyfikat HTTPS z backupu obsługuje ruch pod nowym IP.
	var res struct {
		Token struct{ Connect string }
	}
	s2, _ := e.EPOn(h.Gateway2, "token", "create", "--service", "api", "--json")
	_ = jsonUnmarshal(s2, &res)
	e.Secrets = append(e.Secrets, res.Token.Connect)
	e.StartApp(h.AppOptions{Container: h.ClientA, Connect: res.Token.Connect, Replica: "n", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	result(t, "po restore: odtworzone tokeny unieważnione: %v (%d); stary token B odrzucony: %v (zdrowy=%v); nowy token działa pod nowym IP; wyjście restore: %s",
		allRevoked, len(toks), strings.Contains(bLog, "odwołany"), bHealthy, strings.ReplaceAll(strings.TrimSpace(out), "\n", " | "))
	if !allRevoked || bHealthy {
		t.Fatal("odtworzony token zalogował się po restore")
	}
	// Sprzątanie: powrót do bramy pod pierwotnym adresem dla kolejnych testów.
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
