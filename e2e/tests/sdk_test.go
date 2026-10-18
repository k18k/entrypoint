package tests

// Zestaw e2e 9: SDK (kontrakt bazowy i adapter node:http) w macierzy wersji Node.js.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	h "github.com/k18k/entrypoint/e2e/harness"
)

type sdkCase struct {
	container string
	label     string
}

var sdkMatrix = []sdkCase{
	{h.ClientA, "Node 24"},
	{h.ClientN22, "Node 22"},
}

func versions(e *h.Env, c string) string {
	out, _ := e.Sh(c, `node -p process.version`)
	return strings.TrimSpace(out)
}

// Kontrakt bazowy i adapter w macierzy: ruch, WebSocket, drain z kodem 1001 (adapter), wyjście procesu.
func TestSDK_Matrix(t *testing.T) {
	e := apiSetup(t)
	var lines []string
	for _, sc := range sdkMatrix {
		for _, mode := range []string{"base", "adapter"} {
			_, connect := e.Token("api")
			rep := strings.ToLower(strings.ReplaceAll(fmt.Sprintf("%s-%s", mode, strings.Fields(sc.label)[1]), ".", ""))
			a := e.StartApp(h.AppOptions{Container: sc.container, Connect: connect, Replica: rep, Mode: mode, DrainMs: 5000, Wait: true})
			e.WaitReplicas("api", []string{rep}, 30*time.Second)
			e.WaitHTTPS("https://api.ep.test/whoami", 200)
			code, body, err := get(e.HTTPClient(), "https://api.ep.test/whoami")
			if err != nil || code != 200 || replicaOf(body) != rep {
				t.Fatalf("%s %s: %d %v %s", sc.label, mode, code, err, body)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			ws, _, err := websocket.Dial(ctx, "wss://api.ep.test/ws", &websocket.DialOptions{HTTPClient: e.HTTPClient()})
			if err != nil {
				cancel()
				t.Fatalf("%s %s: WebSocket: %v", sc.label, mode, err)
			}
			ws.Write(ctx, websocket.MessageText, []byte("x"))
			_, m, _ := ws.Read(ctx)
			t0 := time.Now()
			a.Drain(5000)
			_, _, rerr := ws.Read(ctx)
			closeCode := websocket.CloseStatus(rerr)
			wsAfter := time.Since(t0)
			cancel()
			e.Eventually(30*time.Second, 300*time.Millisecond, "koniec drainu", func() error {
				if e.Procs(sc.container, "frpc") > 0 || e.Procs(sc.container, "entrypoint-connector") > 0 {
					return errors.New("frpc albo konektor działa")
				}
				log, _ := e.Sh(sc.container, "cat "+a.Log)
				if !strings.Contains(log, `"drained":true`) {
					return errors.New("drain SDK nie zakończony")
				}
				return nil
			})
			lines = append(lines, fmt.Sprintf("%s (%s) %s: echo WS %q, zamknięcie WS po %s kodem %d", sc.label, versions(e, sc.container), mode, m, ms(wsAfter), closeCode))
			if mode == "adapter" && closeCode != websocket.StatusGoingAway {
				t.Errorf("%s: adapter nie zamknął WebSocketu kodem 1001 (%d)", sc.label, closeCode)
			}
			if mode == "base" && wsAfter < 4*time.Second {
				t.Errorf("%s: kontrakt bazowy zerwał WebSocket przed drainTimeout (%s)", sc.label, wsAfter)
			}
			e.StopAllApps()
		}
	}
	result(t, "%s", strings.Join(lines, "; "))
}

// kill -9 procesu rodzica kończy konektor i frpc (kontrakt bazowy i adapter).
func TestSDK_KillParent(t *testing.T) {
	e := apiSetup(t)
	var lines []string
	for _, mode := range []string{"base", "adapter"} {
		_, connect := e.Token("api")
		a := e.StartApp(h.AppOptions{Container: h.ClientA, Connect: connect, Replica: "k" + mode, Mode: mode, Wait: true})
		t0 := time.Now()
		a.Kill9()
		e.Eventually(10*time.Second, 100*time.Millisecond, "koniec konektora i frpc", func() error {
			if n := e.Procs(h.ClientA, "entrypoint-connector") + e.Procs(h.ClientA, "frpc"); n > 0 {
				return fmt.Errorf("procesów: %d", n)
			}
			return nil
		})
		gone := time.Since(t0)
		e.WaitReplicas("api", nil, 10*time.Second)
		lines = append(lines, fmt.Sprintf("%s: konektor i frpc zakończone po %s, replika zniknęła z grupy", mode, ms(gone)))
	}
	result(t, "%s", strings.Join(lines, "; "))
}

// CLI SDK (npx @k18k/entrypoint tunnel) dla aplikacji w dowolnym języku; SIGTERM uruchamia drain.
func TestSDK_CLI(t *testing.T) {
	e := apiSetup(t)
	_, connect := e.Token("api")
	e.WriteFile(h.ClientA, "/run/ep/cli.connect", connect, "600")
	e.MustSh(h.ClientA, `nohup node -e "require('http').createServer((q,s)=>{if(q.url==='/health')return s.end('ok');s.end(JSON.stringify({replica:'cli'}))}).listen(3000,'127.0.0.1')" > /tmp/plain.log 2>&1 &`)
	e.MustSh(h.ClientA, `cd /app && ENTRYPOINT_CONNECT="$(cat /run/ep/cli.connect)" nohup npx --no-install entrypoint-tunnel tunnel --upstream http://127.0.0.1:3000 --health /health --replica cli --drain-timeout 3s > /tmp/cli.log 2>&1 & echo $! > /tmp/cli.pid`)
	e.WaitReplicas("api", []string{"cli"}, 60*time.Second)
	e.WaitHTTPS("https://api.ep.test/", 200)
	_, body, _ := get(e.HTTPClient(), "https://api.ep.test/")
	t0 := time.Now()
	e.MustSh(h.ClientA, `pkill -TERM -f "[e]ntrypoint-tunnel tunnel"`)
	e.WaitReplicas("api", nil, 10*time.Second)
	withdrawn := time.Since(t0)
	e.Eventually(20*time.Second, 200*time.Millisecond, "koniec CLI", func() error {
		if e.Procs(h.ClientA, "frpc") > 0 || e.Procs(h.ClientA, "entrypoint-connector") > 0 {
			return errors.New("działa")
		}
		return nil
	})
	done := time.Since(t0)
	log, _ := e.Sh(h.ClientA, "cat /tmp/cli.log")
	result(t, "CLI: odpowiedź %s; SIGTERM: wycofanie po %s, koniec po %s; log: %s", strings.TrimSpace(body), ms(withdrawn), ms(done), strings.ReplaceAll(firstLines(log, 3), "\n", " | "))
	if replicaOf(body) != "cli" || done > 10*time.Second {
		t.Fatal("CLI SDK nie działa zgodnie z oczekiwaniem")
	}
}

func firstLines(s string, n int) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "entrypoint") {
			out = append(out, l)
		}
		if len(out) == n {
			break
		}
	}
	return strings.Join(out, "\n")
}
