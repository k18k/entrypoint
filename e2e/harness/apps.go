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

// App is a test app running in a client container.
type App struct {
	e         *Env
	Container string
	CtlPort   int
	Replica   string
	Log       string
}

// AppOptions are the test app's parameters (apps/server.js).
type AppOptions struct {
	Container string
	Connect   string
	Service   string
	Replica   string
	Mode      string // adapter or base
	DrainMs   int
	CtlPort   int
	Health    string
	Wait      bool   // wait for a healthy registration
	Unhealthy bool   // start with a failing health check
	Env       string // extra environment variables, e.g. "CONNECT_TO=1 "
}

// StartApp starts the app; the connection bundle goes into a 0600 file, never into arguments.
func (e *Env) StartApp(o AppOptions) *App {
	e.T.Helper()
	if o.Mode == "" {
		o.Mode = "adapter"
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
	unhealthy := ""
	if o.Unhealthy {
		unhealthy = "START_UNHEALTHY=1 "
	}
	script := fmt.Sprintf(`cd /app && `+unhealthy+o.Env+` ENTRYPOINT_CONNECT="$(cat %s)" MODE=%s SERVICE=%s REPLICA=%s DRAIN_MS=%d CTL_PORT=%d HEALTH=%s ENTRYPOINT_CONNECTOR=/app/node_modules/@k18k/entrypoint-linux-x64/bin/entrypoint-connector nohup node server.js > %s 2>&1 & echo $! > /tmp/app-%s.pid`,
		secret, o.Mode, o.Service, o.Replica, o.DrainMs, o.CtlPort, o.Health, a.Log, o.Replica)
	e.MustSh(o.Container, script)
	if o.Wait {
		a.WaitState("healthy", 60*time.Second)
	}
	return a
}

func (a *App) url(path string) string {
	ip := map[string]string{ClientA: IP(21), ClientB: IP(22), ClientN22: IP(24), Attacker: IP(23)}[a.Container]
	return fmt.Sprintf("http://%s:%d%s", ip, a.CtlPort, path)
}

// Stats is the test app's state.
type Stats struct {
	Replica            string `json:"replica"`
	Requests           int    `json:"requests"`
	Connections        int    `json:"connections"`
	OpenConnections    int    `json:"openConnections"`
	UploadsOK          int    `json:"uploadsOK"`
	UploadsAborted     int    `json:"uploadsAborted"`
	LastRequestAt      *int64 `json:"lastRequestAt"`
	RequestsAfterDrain int    `json:"requestsAfterDrain"`
	DrainStartedAt     *int64 `json:"drainStartedAt"`
	State              string `json:"state"`
	Healthy            bool   `json:"healthy"`
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
		a.e.T.Fatalf("app state %s: %v", a.Replica, err)
	}
	return s
}

// HasEvent reports whether the app got the given tunnel event.
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
	a.e.Eventually(timeout, 300*time.Millisecond, fmt.Sprintf("replica %s in state %s", a.Replica, state), func() error {
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

// Drain starts the replica's drain (like SIGTERM in production).
func (a *App) Drain(timeoutMs int) {
	a.e.T.Helper()
	if err := a.post("/drain", fmt.Sprintf(`{"timeoutMs":%d}`, timeoutMs)); err != nil {
		a.e.T.Fatal(err)
	}
}

// Kill9 kills the Node process (kill -9 of the connector's parent).
func (a *App) Kill9() {
	a.e.T.Helper()
	a.e.MustSh(a.Container, fmt.Sprintf("kill -9 $(cat /tmp/app-%s.pid)", a.Replica))
}

// Stop stops the app without a drain.
func (a *App) Stop() {
	_ = a.post("/exit", "{}")
	time.Sleep(300 * time.Millisecond)
}

// Running reports whether the app's Node process is alive.
func (a *App) Running() bool {
	_, err := a.Stats()
	return err == nil
}

// StopAllApps stops all apps and connectors in the client containers.
func (e *Env) StopAllApps() {
	for _, c := range []string{ClientA, ClientB, ClientN22, Attacker} {
		_, _ = e.Sh(c, "pkill -9 -f 'node server.js'; pkill -9 entrypoint-conn; pkill -9 frpc; rm -f /tmp/*.log; true")
	}
}

// Procs returns the number of processes matching a pattern in a container.
func (e *Env) Procs(container, pattern string) int {
	// pgrep -x on the process name (comm, max 15 characters), so our own shell is not counted.
	if len(pattern) > 15 {
		pattern = pattern[:15]
	}
	out, _ := e.Sh(container, fmt.Sprintf("pgrep -c -x %q || true", pattern))
	n := 0
	fmt.Sscanf(strings.TrimSpace(out), "%d", &n)
	return n
}
