package harness

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func decodeB64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

// App to aplikacja testowa uruchomiona w kontenerze klienta.
type App struct {
	e         *Env
	Container string
	CtlPort   int
	Replica   string
	Log       string
}

// AppOptions to parametry aplikacji testowej (apps/server.js).
type AppOptions struct {
	Container string
	Connect   string
	Service   string
	Replica   string
	Mode      string // express albo base
	DrainMs   int
	CtlPort   int
	Health    string
	Wait      bool // czekaj na zdrową rejestrację
}

// StartApp uruchamia aplikację; pakiet połączenia trafia do pliku 0600, nie do argumentów.
func (e *Env) StartApp(o AppOptions) *App {
	e.T.Helper()
	if o.Mode == "" {
		o.Mode = "express"
	}
	if o.CtlPort == 0 {
		o.CtlPort = 9000
	}
	if o.DrainMs == 0 {
		o.DrainMs = 30000
	}
	if o.Health == "" {
		o.Health = "/health"
	}
	if o.Service == "" {
		o.Service = "api"
	}
	a := &App{e: e, Container: o.Container, CtlPort: o.CtlPort, Replica: o.Replica, Log: fmt.Sprintf("/tmp/app-%s.log", o.Replica)}
	secret := fmt.Sprintf("/run/ep/%s.connect", o.Replica)
	e.WriteFile(o.Container, secret, o.Connect, "600")
	script := fmt.Sprintf(`cd /app && ENTRYPOINT_CONNECT="$(cat %s)" MODE=%s SERVICE=%s REPLICA=%s DRAIN_MS=%d CTL_PORT=%d HEALTH=%s ENTRYPOINT_CONNECTOR=/app/node_modules/@k18k/entrypoint-linux-x64/bin/entrypoint-connector nohup node server.js > %s 2>&1 & echo $! > /tmp/app-%s.pid`,
		secret, o.Mode, o.Service, o.Replica, o.DrainMs, o.CtlPort, o.Health, a.Log, o.Replica)
	e.MustSh(o.Container, script)
	if o.Wait {
		a.WaitState("healthy", 60*time.Second)
	}
	return a
}

func (a *App) url(path string) string {
	ip := map[string]string{ClientA: "172.28.0.21", ClientB: "172.28.0.22", Attacker: "172.28.0.23"}[a.Container]
	return fmt.Sprintf("http://%s:%d%s", ip, a.CtlPort, path)
}

// Stats to stan aplikacji testowej.
type Stats struct {
	Replica            string  `json:"replica"`
	Requests           int     `json:"requests"`
	Connections        int     `json:"connections"`
	OpenConnections    int     `json:"openConnections"`
	UploadsOK          int     `json:"uploadsOK"`
	UploadsAborted     int     `json:"uploadsAborted"`
	LastRequestAt      *int64  `json:"lastRequestAt"`
	RequestsAfterDrain int     `json:"requestsAfterDrain"`
	DrainStartedAt     *int64  `json:"drainStartedAt"`
	State              string  `json:"state"`
	Healthy            bool    `json:"healthy"`
	Events             []struct {
		Event  string `json:"event"`
		T      int64  `json:"t"`
		Reason string `json:"reason"`
	} `json:"events"`
}

func (a *App) Stats() (Stats, error) {
	var s Stats
	resp, err := http.Get(a.url("/stats"))
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	err = json.NewDecoder(resp.Body).Decode(&s)
	return s, err
}

func (a *App) MustStats() Stats {
	a.e.T.Helper()
	s, err := a.Stats()
	if err != nil {
		a.e.T.Fatalf("stan aplikacji %s: %v", a.Replica, err)
	}
	return s
}

// HasEvent mówi, czy aplikacja dostała dane zdarzenie tunelu.
func (s Stats) HasEvent(name string) bool {
	for _, e := range s.Events {
		if e.Event == name {
			return true
		}
	}
	return false
}

func (s Stats) EventTime(name string) int64 {
	for _, e := range s.Events {
		if e.Event == name {
			return e.T
		}
	}
	return 0
}

func (a *App) WaitState(state string, timeout time.Duration) {
	a.e.T.Helper()
	a.e.Eventually(timeout, 300*time.Millisecond, fmt.Sprintf("replika %s w stanie %s", a.Replica, state), func() error {
		s, err := a.Stats()
		if err != nil {
			return err
		}
		if s.State == state || s.HasEvent(state) {
			return nil
		}
		return fmt.Errorf("stan %q", s.State)
	})
}

func (a *App) post(path, body string) error {
	resp, err := http.Post(a.url(path), "application/json", strings.NewReader(body))
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

func (a *App) SetHealthy(ok bool) {
	a.e.T.Helper()
	s := "fail"
	if ok {
		s = "ok"
	}
	if err := a.post("/health/"+s, "{}"); err != nil {
		a.e.T.Fatal(err)
	}
}

// Drain uruchamia drain repliki (jak SIGTERM w produkcji).
func (a *App) Drain(timeoutMs int) {
	a.e.T.Helper()
	if err := a.post("/drain", fmt.Sprintf(`{"timeoutMs":%d}`, timeoutMs)); err != nil {
		a.e.T.Fatal(err)
	}
}

// Kill9 zabija proces Node (kill -9 rodzica konektora).
func (a *App) Kill9() {
	a.e.T.Helper()
	a.e.MustSh(a.Container, fmt.Sprintf("kill -9 $(cat /tmp/app-%s.pid)", a.Replica))
}

// Stop kończy aplikację bez drainu.
func (a *App) Stop() {
	_ = a.post("/exit", "{}")
	time.Sleep(300 * time.Millisecond)
}

// Running mówi, czy proces Node aplikacji żyje.
func (a *App) Running() bool {
	_, err := a.Stats()
	return err == nil
}

// StopAllApps kończy wszystkie aplikacje i konektory w kontenerach klientów.
func (e *Env) StopAllApps() {
	for _, c := range []string{ClientA, ClientB, Attacker} {
		_, _ = e.Sh(c, "pkill -9 -f 'node server.js'; pkill -9 entrypoint-conn; pkill -9 frpc; rm -f /tmp/*.log; true")
	}
}

// Procs zwraca liczbę procesów pasujących do wzorca w kontenerze.
func (e *Env) Procs(container, pattern string) int {
	out, _ := e.Sh(container, fmt.Sprintf("pgrep -f -c %q || true", pattern))
	n := 0
	fmt.Sscanf(strings.TrimSpace(out), "%d", &n)
	return n
}
