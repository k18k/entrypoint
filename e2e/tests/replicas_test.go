package tests

// Eksperyment 1 i zestaw e2e 5: grupy frp i drain (testy 1.1–1.10).

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

// 1.1: nowe żądania na istniejących keep-alive Caddy po wycofaniu repliki.
func TestReplicas_1_1_KeepAliveAfterWithdraw_Adapter(t *testing.T) {
	e := apiSetup(t)
	a, _ := twoReplicas(t, e, "adapter", 60000)
	load := e.StartLoad("https://api.ep.test/whoami", 4, 20*time.Millisecond)
	time.Sleep(3 * time.Second)
	if a.MustStats().Requests == 0 {
		t.Fatal("replika a nie dostała ruchu przed drainem")
	}
	a.Drain(60000)
	a.WaitState("withdrawn", 10*time.Second)
	t0 := time.Now()
	time.Sleep(5 * time.Second)
	after5 := a.MustStats().Requests
	time.Sleep(10 * time.Second)
	after15 := a.MustStats().Requests
	ss := load.Stop()
	errs := h.Count(h.Window(ss, t0.Add(-5*time.Second), time.Now()), func(s h.Sample) bool { return s.Status != 200 })
	result(t, "adapter: żądania do a w oknie 5–15 s po wycofaniu: %d; błędy klienta w trakcie drainu: %d z %d", after15-after5, errs, len(h.Window(ss, t0.Add(-5*time.Second), time.Now())))
	if after15 != after5 {
		t.Fatalf("wycofana replika dostała %d nowych żądań po 5 s", after15-after5)
	}
	if errs > 0 {
		t.Fatalf("%d żądań zakończonych błędem w trakcie drainu", errs)
	}
}

func TestReplicas_1_1_KeepAliveAfterWithdraw_Base(t *testing.T) {
	e := apiSetup(t)
	a, _ := twoReplicas(t, e, "base", 10000)
	load := e.StartLoad("https://api.ep.test/whoami", 4, 20*time.Millisecond)
	time.Sleep(3 * time.Second)
	a.Drain(10000)
	a.WaitState("withdrawn", 10*time.Second)
	t0 := time.Now()
	e.Eventually(30*time.Second, 200*time.Millisecond, "koniec frpc repliki a", func() error {
		if e.Procs(h.ClientA, "frpc") > 0 {
			return errors.New("frpc nadal działa")
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
	result(t, "kontrakt bazowy: ostatnie żądanie do a %s po wycofaniu; frpc zakończony po %s; po drainTimeout nowych żądań: %d; błędy klienta: %d",
		ms(lastToA.Sub(t0)), ms(tEnd), after.Requests-before.Requests, errs)
	if after.Requests != before.Requests {
		t.Fatal("replika a dostaje żądania po drainTimeout")
	}
}

// 1.2a: łagodny drain w trakcie uploadu mieszczącego się w drainTimeout.
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
	result(t, "upload 8 MB w trakcie drainu: kod %d, czas %s, uploadsOK=%d, aborted=%d", r.status, ms(r.took), st.UploadsOK, st.UploadsAborted)
	if r.err != nil || r.status != 200 || !strings.Contains(r.body, r.want) {
		t.Fatalf("upload: err=%v status=%d body=%s", r.err, r.status, r.body)
	}
	if st.UploadsOK != 1 {
		t.Fatal("aplikacja nie odebrała kompletnych danych")
	}
}

// 1.2b: natychmiastowe odwołanie tokenu w trakcie uploadu.
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
		t.Fatal("upload nie został przerwany")
	}
	took := time.Since(t0)
	time.Sleep(time.Second)
	st := a.MustStats()
	result(t, "odwołanie w trakcie uploadu: przerwanie po %s, kod %d, err=%v, uploadsOK=%d, aborted=%d, stan tunelu=%s",
		ms(took), r.status, r.err, st.UploadsOK, st.UploadsAborted, st.State)
	if r.err == nil && r.status == 200 {
		t.Fatal("upload zakończył się sukcesem mimo odwołania")
	}
	if st.UploadsOK != 0 || st.UploadsAborted != 1 {
		t.Fatalf("aplikacja: uploadsOK=%d aborted=%d", st.UploadsOK, st.UploadsAborted)
	}
	e.Eventually(30*time.Second, 500*time.Millisecond, "konektor kończy się zdarzeniem revoked", func() error {
		if !a.MustStats().HasEvent("revoked") {
			return errors.New("brak zdarzenia revoked")
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

// 1.3: SSE i WebSocket przez wycofywaną replikę.
func TestReplicas_1_3_StreamsDuringDrain_Adapter(t *testing.T) {
	testStreams(t, "adapter", 20000)
}

func TestReplicas_1_3_StreamsDuringDrain_Base(t *testing.T) {
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
			t.Fatalf("strumienie nie zostały zamknięte w %s (ws=%v sse=%v)", limit, wsE, sseE)
		}
	}
	code := websocket.CloseStatus(wsE)
	result(t, "%s: WebSocket zamknięty po %s, kod %d (%v); SSE zakończone po %s (%v)", mode, ms(wsAt), code, wsE, ms(sseAt), sseE)
	drain := time.Duration(drainMs) * time.Millisecond
	if mode == "adapter" {
		if code != websocket.StatusGoingAway {
			t.Fatalf("adapter: oczekiwano kodu 1001, jest %d", code)
		}
		if wsAt >= drain || sseAt >= drain {
			t.Fatal("adapter: strumienie zamknięte dopiero po drainTimeout")
		}
	} else {
		if wsAt < drain-time.Second || sseAt < drain-time.Second {
			t.Fatalf("kontrakt bazowy: strumień zerwany przed drainTimeout (ws %s, sse %s)", wsAt, sseAt)
		}
	}
}

// 1.4: wycofanie z grupy, wygaszanie połączeń i koniec frpc jako osobne, obserwowalne kroki.
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
	// Krok 1: replika znika z grupy, frpc działa dalej.
	e.WaitReplicas("api", nil, 5*time.Second)
	t1 := time.Since(t0)
	if e.Procs(h.ClientA, "frpc") == 0 {
		t.Fatal("frpc zakończony już w kroku 1")
	}
	// Krok 2: rozpoczęte żądanie kończy się poprawnie po wycofaniu.
	code := <-slow
	t2 := time.Since(t0)
	if code != 200 {
		t.Fatalf("żądanie w trakcie wygaszania: kod %d", code)
	}
	if e.Procs(h.ClientA, "frpc") == 0 {
		t.Fatal("frpc zakończony przed końcem drainTimeout")
	}
	// Krok 3: po drainTimeout konektor kończy frpc.
	e.Eventually(15*time.Second, 200*time.Millisecond, "koniec frpc", func() error {
		if e.Procs(h.ClientA, "frpc") > 0 {
			return errors.New("frpc działa")
		}
		return nil
	})
	t3 := time.Since(t0)
	st := a.MustStats()
	result(t, "krok 1 (wycofanie, brak w statusie) po %s; krok 2 (żądanie 4 s zakończone 200) po %s; krok 3 (koniec frpc) po %s; zdarzenia: draining=%v withdrawn=%v",
		ms(t1), ms(t2), ms(t3), st.HasEvent("draining"), st.HasEvent("withdrawn"))
	if !st.HasEvent("draining") || !st.HasEvent("withdrawn") {
		t.Fatal("brak zdarzeń kroków drainu")
	}
}

// 1.5: jedna replika zwraca błędy, druga jest zdrowa.
func TestReplicas_1_5_UnhealthyReplica(t *testing.T) {
	e := apiSetup(t)
	a, _ := twoReplicas(t, e, "adapter", 30000)
	load := e.StartLoad("https://api.ep.test/whoami", 4, 20*time.Millisecond)
	time.Sleep(2 * time.Second)
	t0 := time.Now()
	a.SetHealthy(false)
	removed := e.WaitReplicas("api", []string{"b"}, 20*time.Second)
	limit := 1*time.Second*2 + 5*time.Second // interwał × liczba prób + 5 s
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
	result(t, "chora replika usunięta z grupy po %s (limit %s); błędy: %d, ostatni po %s; błędy po limicie: %d z %d",
		ms(removed), ms(limit), n500, ms(last500), bad, len(tail))
	if removed > limit {
		t.Fatalf("usunięcie trwało %s", removed)
	}
	if bad > 0 {
		t.Fatalf("%d błędów po usunięciu chorej repliki", bad)
	}
}

// 1.6: awaria entrypointd przy ruchu (kill -9, systemd restartuje demona; gniazdo pluginu trzyma systemd).
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
	// Kilka awarii pod rząd, w tym w trakcie nowego logowania.
	for i := 0; i < 3; i++ {
		e.MustExec(h.Gateway, "systemctl", "kill", "-s", "KILL", "entrypointd")
		if i == 0 {
			e.StartApp(h.AppOptions{Container: h.ClientB, Connect: cb, Replica: "b"})
		}
		time.Sleep(3 * time.Second)
	}
	e.WaitReplicas("api", []string{"a", "b"}, 30*time.Second)
	back := time.Since(t0)
	time.Sleep(12 * time.Second) // co najmniej jeden pełny cykl heartbeat po awariach
	ss := load.Stop()
	win := h.Window(ss, t0, time.Now())
	bad := h.Count(win, func(s h.Sample) bool { return s.Status != 200 })
	pidAfter := strings.TrimSpace(e.MustExec(h.Gateway, "systemctl", "show", "-p", "MainPID", "--value", "entrypointd"))
	result(t, "3× kill -9 entrypointd przy ruchu: błędy żądań %d z %d; nowa replika b zalogowana po %s od pierwszej awarii; PID %s -> %s",
		bad, len(win), ms(back), pidBefore, pidAfter)
	if bad > 0 {
		t.Fatalf("%d błędów ruchu przy awariach entrypointd", bad)
	}
	if pidBefore == pidAfter {
		t.Fatal("entrypointd nie został zrestartowany")
	}
}

// 1.7: zimny start Caddy przy niedziałającym entrypointd.
func TestReplicas_1_7_CaddyColdStartWithoutDaemon(t *testing.T) {
	e := apiSetup(t)
	_, ca := e.Token("api")
	e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	e.MustExec(h.Gateway, "systemctl", "stop", "entrypointd")
	e.MustExec(h.Gateway, "systemctl", "restart", "entrypoint-caddy")
	t0 := time.Now()
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	result(t, "Caddy po restarcie bez entrypointd obsługuje ruch po %s", ms(time.Since(t0)))
	e.MustExec(h.Gateway, "systemctl", "start", "entrypointd")
}

// 1.8: usunięcie ostatniej repliki.
func TestReplicas_1_8_LastReplicaRemoved(t *testing.T) {
	e := apiSetup(t)
	_, ca := e.Token("api")
	a := e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	load := e.StartLoad("https://api.ep.test/whoami", 2, 50*time.Millisecond)
	time.Sleep(2 * time.Second)
	t0 := time.Now()
	a.Kill9()
	e.Eventually(10*time.Second, 100*time.Millisecond, "odpowiedź 502/503", func() error {
		for _, s := range h.Window(load.Snapshot(), t0, time.Now()) {
			if s.Status == 502 || s.Status == 503 {
				return nil
			}
		}
		return errors.New("brak")
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
	// Najdłuższy czas odpowiedzi po usunięciu: żadne połączenie nie wisi na usuniętej replice.
	for i := 1; i < len(ss); i++ {
		if ss[i].At.After(t0) {
			if d := ss[i].At.Sub(ss[i-1].At); d > slowest {
				slowest = d
			}
		}
	}
	gone := e.Procs(h.ClientA, "frpc") == 0 && e.Procs(h.ClientA, "entrypoint-connector") == 0
	result(t, "kill -9 ostatniej repliki: pierwsza odpowiedź 502/503 po %s; najdłuższa przerwa między odpowiedziami %s; konektor i frpc zakończone: %v", ms(first5xx), ms(slowest), gone)
	if first5xx > time.Second {
		t.Fatalf("502/503 dopiero po %s", first5xx)
	}
	if slowest > 5*time.Second {
		t.Fatalf("połączenie wisiało %s", slowest)
	}
	if !gone {
		t.Fatal("konektor albo frpc przeżył śmierć rodzica")
	}
}

// 1.9: restart frps.
func TestReplicas_1_9_FrpsRestart(t *testing.T) {
	e := apiSetup(t)
	twoReplicas(t, e, "adapter", 30000)
	port := e.Status().Status.Services[0].Port
	e.MustExec(h.Gateway, "systemctl", "restart", "entrypoint-frps")
	t0 := time.Now()
	back := e.WaitReplicas("api", []string{"a", "b"}, 60*time.Second)
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	samePort := e.Status().Status.Services[0].Port == port
	result(t, "po restarcie frps obie repliki wróciły po %s, port bez zmian: %v", ms(time.Since(t0)), samePort)
	if back > 30*time.Second {
		t.Fatalf("repliki wróciły dopiero po %s", back)
	}
}

// 1.10: nowa replika z health checkiem nie dostaje połączeń przed pierwszym udanym sprawdzeniem.
func TestReplicas_1_10_NoTrafficBeforeHealthy(t *testing.T) {
	e := apiSetup(t)
	_, ca := e.Token("api")
	_, cb := e.Token("api")
	e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	b := e.StartApp(h.AppOptions{Container: h.ClientB, Connect: cb, Replica: "b", Unhealthy: true})
	e.Eventually(30*time.Second, 300*time.Millisecond, "panel repliki b", func() error { _, err := b.Stats(); return err })
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
	// Przy stałym ruchu Caddy używa dalej połączeń keep-alive do a; nowa replika dostaje
	// ruch dopiero na nowych połączeniach (np. przy wzroście współbieżności).
	burst := e.StartLoad("https://api.ep.test/whoami", 24, 10*time.Millisecond)
	time.Sleep(5 * time.Second)
	burst.Stop()
	after := b.MustStats().Requests
	result(t, "po zdrowiu przy stałym ruchu 4 połączeń: żądań do b %d; po wzroście do 24 połączeń: %d", afterSteady, after)
	result(t, "replika z niezdanym health checkiem: żądań przed zdrowiem %d, repliki w statusie %v; po zdrowiu żądań %d", before, inStatus, after)
	if before != 0 {
		t.Fatalf("replika dostała %d żądań przed pierwszym udanym sprawdzeniem", before)
	}
	if after == 0 {
		t.Fatal("zdrowa replika nie dostaje ruchu")
	}
}
