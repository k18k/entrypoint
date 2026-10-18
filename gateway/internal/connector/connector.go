// Pakiet connector to proces pośredniczący nad oficjalną binarką frpc.
//
// Buduje konfigurację frpc z pakietu połączenia (zawsze z weryfikacją CA i nazwy serwera
// oraz z health checkiem), prowadzi reconnect i drain, a stan zgłasza zdarzeniami JSON
// na standardowym wyjściu. Nigdy nie wypisuje tokenu ani pakietu połączenia.
package connector

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/k18k/entrypoint/gateway/internal/pki"
	"github.com/k18k/entrypoint/gateway/internal/redact"
	"github.com/k18k/entrypoint/gateway/internal/token"
)

// Options to parametry tunelu.
type Options struct {
	Bundle         token.Bundle
	Service        string
	Upstream       string // http://host:port
	Health         string // ścieżka HTTP; pusta = health check TCP
	HealthInterval int    // sekundy
	HealthMaxFail  int
	DrainTimeout   time.Duration
	Replica        string
	FrpcPath       string
	ParentPipe     bool // koniec stdin = śmierć rodzica
	InsecureDev    bool // tylko deweloperski tryb bez weryfikacji CA
	Events         io.Writer
	Log            io.Writer
}

// Event to zdarzenie dla SDK.
type Event struct {
	Event     string `json:"event"`
	Replica   string `json:"replica,omitempty"`
	Reason    string `json:"reason,omitempty"`
	TimeoutMs int64  `json:"timeoutMs,omitempty"`
	Code      int    `json:"code,omitempty"`
	Time      string `json:"time"`
}

// Kody wyjścia konektora.
const (
	ExitOK      = 0
	ExitError   = 1
	ExitRevoked = 3
	ExitAuth    = 4
)

type Connector struct {
	o         Options
	dir       string
	cfgPath   string
	adminPort int
	adminPass string
	proxyName string

	evMu sync.Mutex
	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{} // zamykany po zakończeniu bieżącego frpc

	state     string // ostatni stan proxy
	connected bool
	fatal     chan Event
	draining  bool
}

var replicaRe = regexp.MustCompile(`[^a-z0-9-]+`)

// DefaultReplica tworzy nazwę repliki z nazwy hosta i losowego sufiksu.
func DefaultReplica() string {
	h, _ := os.Hostname()
	h = strings.Trim(replicaRe.ReplaceAllString(strings.ToLower(h), "-"), "-")
	if len(h) > 20 {
		h = h[:20]
	}
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	if h == "" {
		h = "r"
	}
	return h + "-" + hex.EncodeToString(b)
}

func New(o Options) (*Connector, error) {
	if o.Bundle.Server == "" {
		return nil, errors.New("brak pakietu połączenia (ENTRYPOINT_CONNECT)")
	}
	if o.Service == "" {
		o.Service = o.Bundle.Service
	}
	if o.Bundle.Service != "" && o.Service != o.Bundle.Service {
		return nil, fmt.Errorf("pakiet połączenia jest dla serwisu %q, a nie %q", o.Bundle.Service, o.Service)
	}
	if o.Replica == "" {
		o.Replica = DefaultReplica()
	}
	o.Replica = strings.Trim(replicaRe.ReplaceAllString(strings.ToLower(o.Replica), "-"), "-")
	if o.Replica == "" || len(o.Replica) > 32 {
		return nil, errors.New("nazwa repliki: 1-32 znaki [a-z0-9-]")
	}
	if o.DrainTimeout <= 0 {
		o.DrainTimeout = 30 * time.Second
	}
	if o.HealthInterval <= 0 {
		o.HealthInterval = 2
	}
	if o.HealthMaxFail <= 0 {
		o.HealthMaxFail = 2
	}
	if !o.InsecureDev && len(o.Bundle.CAs) == 0 {
		return nil, errors.New("brak zaufanego CA w pakiecie połączenia; konektor odmawia startu")
	}
	id, _, err := token.Parse(o.Bundle.Token)
	if err != nil {
		return nil, errors.New("niepoprawny token w pakiecie połączenia")
	}
	if o.FrpcPath == "" {
		o.FrpcPath = findFrpc()
	}
	if _, err := os.Stat(o.FrpcPath); err != nil {
		return nil, fmt.Errorf("nie znaleziono frpc (%s); ustaw ENTRYPOINT_FRPC", o.FrpcPath)
	}
	if o.Events == nil {
		o.Events = os.Stdout
	}
	if o.Log == nil {
		o.Log = os.Stderr
	}
	return &Connector{o: o, proxyName: o.Service + "." + id + "." + o.Replica, fatal: make(chan Event, 1)}, nil
}

func findFrpc() string {
	if p := os.Getenv("ENTRYPOINT_FRPC"); p != "" {
		return p
	}
	name := "frpc"
	if isWindows {
		name = "frpc.exe"
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return name
}

func (c *Connector) emit(e Event) {
	c.evMu.Lock()
	defer c.evMu.Unlock()
	e.Time = time.Now().UTC().Format(time.RFC3339Nano)
	if e.Replica == "" {
		e.Replica = c.proxyName
	}
	e.Reason = redact.String(e.Reason)
	b, _ := json.Marshal(e)
	c.o.Events.Write(append(b, '\n'))
}

func (c *Connector) logf(f string, a ...any) {
	fmt.Fprintln(c.o.Log, redact.String("entrypoint-connector: "+fmt.Sprintf(f, a...)))
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (c *Connector) writeConfig(withProxy bool) error {
	u, err := url.Parse(c.o.Upstream)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return fmt.Errorf("upstream musi mieć postać http://host:port, a jest %q", c.o.Upstream)
	}
	host, portS, err := net.SplitHostPort(u.Host)
	if err != nil {
		host, portS = u.Hostname(), "80"
	}
	port, _ := strconv.Atoi(portS)
	var fps []string
	for _, ca := range c.o.Bundle.CAs {
		if fp, err := pki.FingerprintPEM([]byte(ca)); err == nil {
			fps = append(fps, fp)
		}
	}
	tlsCfg := map[string]any{"enable": true, "serverName": c.o.Bundle.Server}
	if !c.o.InsecureDev {
		tlsCfg["trustedCaFile"] = filepath.Join(c.dir, "ca.pem")
	}
	cfg := map[string]any{
		"serverAddr":    c.o.Bundle.Server,
		"serverPort":    c.o.Bundle.Port,
		"loginFailExit": false,
		// Token trafia do frpc wyłącznie przez zmienną środowiskową, nie do pliku.
		"metadatas": map[string]string{"token": "{{ .Envs.EP_FRPC_TOKEN }}", "ca": strings.Join(fps, ",")},
		"transport": map[string]any{
			"tls":               tlsCfg,
			"heartbeatInterval": 10,
			"heartbeatTimeout":  40,
			"poolCount":         1,
			"tcpMux":            true,
		},
		"webServer": map[string]any{"addr": "127.0.0.1", "port": c.adminPort, "user": "connector", "password": c.adminPass},
		"log":       map[string]any{"to": "console", "level": "info", "disablePrintColor": true},
		"proxies":   []any{},
	}
	if withProxy {
		hc := map[string]any{"type": "tcp", "intervalSeconds": c.o.HealthInterval, "maxFailed": c.o.HealthMaxFail, "timeoutSeconds": 2}
		if c.o.Health != "" {
			hc["type"] = "http"
			hc["path"] = c.o.Health
		}
		cfg["proxies"] = []any{map[string]any{
			"name":        c.proxyName,
			"type":        "tcp",
			"localIP":     host,
			"localPort":   port,
			"healthCheck": hc,
		}}
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return pki.WriteFileAtomic(c.cfgPath, b, 0o600)
}

// Run prowadzi tunel do zakończenia. Zwraca kod wyjścia.
func (c *Connector) Run(ctx context.Context, commands <-chan Command) int {
	var err error
	c.dir, err = os.MkdirTemp("", "entrypoint-connector-")
	if err != nil {
		c.emit(Event{Event: "error", Reason: err.Error()})
		return ExitError
	}
	defer os.RemoveAll(c.dir)
	if err := os.Chmod(c.dir, 0o700); err != nil {
		return ExitError
	}
	c.cfgPath = filepath.Join(c.dir, "frpc.json")
	if err := os.WriteFile(filepath.Join(c.dir, "ca.pem"), []byte(strings.Join(c.o.Bundle.CAs, "\n")), 0o600); err != nil {
		return ExitError
	}
	if c.adminPort, err = freePort(); err != nil {
		return ExitError
	}
	pw := make([]byte, 16)
	_, _ = rand.Read(pw)
	c.adminPass = hex.EncodeToString(pw)
	if err := c.writeConfig(true); err != nil {
		c.emit(Event{Event: "error", Reason: err.Error()})
		return ExitError
	}
	if c.o.InsecureDev {
		c.logf("UWAGA: tryb deweloperski bez weryfikacji certyfikatu bramy. Nie używaj go produkcyjnie.")
	}
	c.emit(Event{Event: "starting"})

	supCtx, stopSup := context.WithCancel(ctx)
	defer stopSup()
	supDone := make(chan struct{})
	go func() { c.supervise(supCtx); close(supDone) }()
	go c.pollStatus(supCtx)

	stop := func(code int) int {
		stopSup()
		c.killFrpc()
		<-supDone
		c.emit(Event{Event: "exit", Code: code})
		return code
	}

	for {
		select {
		case <-ctx.Done():
			return stop(ExitOK)
		case e := <-c.fatal:
			c.emit(e)
			if e.Event == "revoked" {
				return stop(ExitRevoked)
			}
			return stop(ExitAuth)
		case cmd, ok := <-commands:
			if !ok {
				// Rodzic zginął (zamknięta rura): kończymy frpc i siebie.
				c.logf("standardowe wejście zamknięte; kończę frpc i konektor")
				return stop(ExitOK)
			}
			switch cmd.Cmd {
			case "drain":
				t := c.o.DrainTimeout
				if cmd.TimeoutMs > 0 {
					t = time.Duration(cmd.TimeoutMs) * time.Millisecond
				}
				return stop(c.drain(ctx, t, commands))
			case "stop":
				return stop(ExitOK)
			}
		}
	}
}

// Command to polecenie od SDK (linia JSON na stdin) albo sygnał.
type Command struct {
	Cmd       string `json:"cmd"` // drain, drained, stop
	TimeoutMs int64  `json:"timeoutMs,omitempty"`
}

// ReadCommands czyta polecenia z r; kanał zamyka się na EOF.
func ReadCommands(r io.Reader) <-chan Command {
	ch := make(chan Command, 4)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			var c Command
			if json.Unmarshal(sc.Bytes(), &c) == nil && c.Cmd != "" {
				ch <- c
			}
		}
	}()
	return ch
}

// drain: 1) wycofanie z grupy, 2) wygaszanie połączeń, 3) zakończenie frpc.
func (c *Connector) drain(ctx context.Context, timeout time.Duration, commands <-chan Command) int {
	c.mu.Lock()
	c.draining = true
	c.mu.Unlock()
	c.emit(Event{Event: "draining", TimeoutMs: timeout.Milliseconds()})
	deadline := time.After(timeout)
	if err := c.withdraw(ctx); err != nil {
		c.logf("wycofanie z grupy nie powiodło się: %v; kończę frpc", err)
		return ExitError
	}
	c.emit(Event{Event: "withdrawn"})
	for {
		select {
		case <-deadline:
			c.emit(Event{Event: "drain_timeout"})
			return ExitOK
		case <-ctx.Done():
			return ExitOK
		case cmd, ok := <-commands:
			if !ok || cmd.Cmd == "drained" || cmd.Cmd == "stop" {
				c.emit(Event{Event: "drained"})
				return ExitOK
			}
		}
	}
}

// withdraw zamyka proxy we frpc (CloseProxy), więc frps nie kieruje do repliki nowych połączeń.
// Istniejące połączenia działają dalej.
func (c *Connector) withdraw(ctx context.Context) error {
	if err := c.writeConfig(false); err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://127.0.0.1:%d/api/reload", c.adminPort), nil)
	req.SetBasicAuth("connector", c.adminPass)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("reload frpc: kod %d", resp.StatusCode)
	}
	// Czekamy, aż proxy zniknie ze statusu frpc.
	for i := 0; i < 50; i++ {
		ph, _ := c.proxyPhase(ctx)
		if ph == "" {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("proxy nie zostało zamknięte w 5 s")
}

func (c *Connector) proxyPhase(ctx context.Context) (string, string) {
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://127.0.0.1:%d/api/status", c.adminPort), nil)
	req.SetBasicAuth("connector", c.adminPass)
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return "?", ""
	}
	defer resp.Body.Close()
	var st map[string][]struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Err    string `json:"err"`
	}
	if json.NewDecoder(resp.Body).Decode(&st) != nil {
		return "?", ""
	}
	for _, ps := range st {
		for _, p := range ps {
			if p.Name == c.proxyName {
				return p.Status, p.Err
			}
		}
	}
	return "", ""
}

// pollStatus tłumaczy stan proxy frpc na zdarzenia healthy/unhealthy.
func (c *Connector) pollStatus(ctx context.Context) {
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.mu.Lock()
		draining := c.draining
		c.mu.Unlock()
		if draining {
			return
		}
		ph, perr := c.proxyPhase(ctx)
		if ph == "?" || ph == last {
			continue
		}
		last = ph
		switch ph {
		case "running":
			c.emit(Event{Event: "healthy"})
		case "check failed":
			c.emit(Event{Event: "unhealthy", Reason: "health check nie przechodzi"})
		case "start error":
			c.emit(Event{Event: "rejected", Reason: perr})
			if strings.Contains(perr, "ep:revoked") {
				c.fatalOnce(Event{Event: "revoked", Reason: perr})
			}
		}
	}
}

func (c *Connector) fatalOnce(e Event) {
	select {
	case c.fatal <- e:
	default:
	}
}

// supervise uruchamia frpc i restartuje go z wykładniczym backoffem.
func (c *Connector) supervise(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.runFrpc(ctx)
		if ctx.Err() != nil {
			return
		}
		if c.connected {
			c.emit(Event{Event: "disconnected", Reason: "frpc zakończył działanie"})
		}
		c.connected = false
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		c.logf("frpc zakończony (%v); ponowny start za %s", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

var authFailures = 0

func (c *Connector) runFrpc(ctx context.Context) error {
	cmd := exec.Command(c.o.FrpcPath, "-c", c.cfgPath)
	cmd.Env = frpcEnv(c.o.Bundle.Token)
	setParentDeath(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	c.mu.Lock()
	c.cmd, c.done = cmd, done
	c.mu.Unlock()
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		c.handleLine(sc.Text())
	}
	err = cmd.Wait()
	close(done)
	return err
}

// frpcEnv przekazuje token tylko frpc; pakiet połączenia nie trafia do jego środowiska.
func frpcEnv(tok string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ENTRYPOINT_CONNECT=") || strings.HasPrefix(kv, "EP_FRPC_TOKEN=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "EP_FRPC_TOKEN="+tok)
}

func (c *Connector) handleLine(line string) {
	l := redact.String(line)
	fmt.Fprintln(c.o.Log, "frpc: "+l)
	switch {
	case strings.Contains(line, "login to server success"):
		authFailures = 0
		if !c.connected {
			c.connected = true
			c.emit(Event{Event: "connected"})
		}
	case strings.Contains(line, "ep:revoked"):
		c.fatalOnce(Event{Event: "revoked", Reason: "token został odwołany"})
	case strings.Contains(line, "ep:unknown_token"):
		authFailures++
		if authFailures >= 3 {
			c.fatalOnce(Event{Event: "auth_failed", Reason: "brama nie zna tego tokenu"})
		}
	case strings.Contains(line, "certificate") && (strings.Contains(line, "x509") || strings.Contains(line, "tls")):
		c.emit(Event{Event: "tls_error", Reason: "certyfikat bramy nie jest podpisany przez zaufane CA z pakietu połączenia"})
	case strings.Contains(line, "try to reconnect"), strings.Contains(line, "control writer is closing"), strings.Contains(line, "pong message contains error"):
		if c.connected {
			c.connected = false
			c.emit(Event{Event: "disconnected", Reason: "sesja z bramą zerwana"})
		}
	}
}

func (c *Connector) killFrpc() {
	c.mu.Lock()
	cmd, done := c.cmd, c.done
	c.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	terminate(cmd)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}
