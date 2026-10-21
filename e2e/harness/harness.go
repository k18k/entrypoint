// Package harness drives the e2e environment from the runner container.
package harness

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Environment copy (shard): each has its own compose project, subnets and container names,
// so several copies run in parallel. EP_SHARD picks the copy, EP_SHARDS is their number.
var (
	Shard  = envInt("EP_SHARD", 0)
	Shards = envInt("EP_SHARDS", 1)
	Net    = fmt.Sprintf("10.77.%d", Shard) // the environment's public network
	Priv   = fmt.Sprintf("10.78.%d", Shard) // private network ("VPN")

	Gateway     = name("gateway")
	Gateway2    = name("gateway2")
	ClientA     = name("client-a")
	ClientB     = name("client-b")
	ClientN22   = name("client-n22")
	Attacker    = name("attacker")
	Upstream    = name("upstream")
	ACME        = name("acme")
	DNS         = name("dns")
	DNSIP       = Net + ".2"
	GatewayIP   = Net + ".10"
	Gateway2IP  = Net + ".11"
	RunnerIP    = Net + ".30"
	UpstreamIP  = Priv + ".20"
	UpstreamVPN = UpstreamIP + ":8080"
	// The environment's public Docker network (for containers started by tests).
	Network = fmt.Sprintf("ep-e2e-%d_epnet", Shard)

	Containers = []string{Gateway, Gateway2, ClientA, ClientB, ClientN22, Attacker, Upstream, ACME, DNS}
)

const TunnelName = "tunnel.ep.test"

// Version is the version from versions.json (the release package name installed in tests).
var Version = func() string {
	var v struct {
		Entrypoint string `json:"entrypoint"`
	}
	b, _ := os.ReadFile(filepath.Join(Repo(), "versions.json"))
	_ = json.Unmarshal(b, &v)
	return v.Entrypoint
}()

func name(s string) string { return fmt.Sprintf("ep%d-%s", Shard, s) }

// IP returns an address in the environment's public network.
func IP(last int) string { return fmt.Sprintf("%s.%d", Net, last) }

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func Repo() string {
	if r := os.Getenv("REPO"); r != "" {
		return r
	}
	wd, _ := os.Getwd()
	return filepath.Dir(wd)
}

// Env is the context of one test.
type Env struct {
	T       *testing.T
	start   time.Time
	Secrets []string // values that must not appear in any log
}

var experiments sync.Map

// Experiment marks a test as an experiment: it checks Caddy or frp behavior the specification
// relies on, not our code. `make e2e-experiments` runs it, `make e2e` does not.
// The call must come before h.New.
func Experiment(t *testing.T) { experiments.Store(t.Name(), true) }

func New(t *testing.T) *Env {
	if os.Getenv("EP_E2E") == "" {
		t.Skip("e2e tests run via `make e2e`")
	}
	_, exp := experiments.Load(t.Name())
	only := os.Getenv("EP_EXPERIMENTS") != ""
	switch {
	case exp && !only:
		t.Skip("experiment: run it with make e2e-experiments")
	case !exp && only:
		t.Skip("not an experiment")
	}
	if Shards > 1 && shardOf(t.Name(), exp) != Shard {
		t.Skip("test assigned to another environment copy")
	}
	e := &Env{T: t, start: time.Now().Add(-time.Second)}
	kind := "e2e"
	if exp {
		kind = "experiment"
	}
	t.Cleanup(func() { e.collect(kind) })
	return e
}

// Assigning tests to environment copies: the longest tests (times from the previous run,
// e2e/durations.json) go one by one to the least loaded copy. Unknown tests: name hash.
type durationEntry struct {
	Seconds    float64 `json:"seconds"`
	Experiment bool    `json:"experiment"`
}

var (
	assignOnce sync.Once
	assignment map[string]int
)

func shardOf(name string, exp bool) int {
	assignOnce.Do(func() {
		assignment = map[string]int{}
		var d map[string]durationEntry
		b, err := os.ReadFile(filepath.Join(Repo(), "e2e", "durations.json"))
		if err == nil {
			_ = json.Unmarshal(b, &d)
		}
		for _, wantExp := range []bool{false, true} {
			type item struct {
				name string
				sec  float64
			}
			var items []item
			for n, de := range d {
				if de.Experiment == wantExp {
					items = append(items, item{n, de.Seconds})
				}
			}
			sort.Slice(items, func(i, j int) bool {
				if items[i].sec != items[j].sec {
					return items[i].sec > items[j].sec
				}
				return items[i].name < items[j].name
			})
			load := make([]float64, Shards)
			for _, it := range items {
				best := 0
				for i := range load {
					if load[i] < load[best] {
						best = i
					}
				}
				load[best] += it.sec
				assignment[it.name] = best
			}
		}
	})
	if s, ok := assignment[name]; ok {
		return s
	}
	h := fnv.New32a()
	h.Write([]byte(name))
	return int(h.Sum32() % uint32(Shards))
}

// Run runs a command and returns stdout; stderr goes into the error.
func Run(stdin io.Reader, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr, cmd.Stdin = &out, &errb, stdin
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s %s: %v: %s%s", name, strings.Join(args, " "), err, errb.String(), out.String())
	}
	return out.String(), nil
}

// Exec runs a command in a container.
func (e *Env) Exec(container string, args ...string) (string, error) {
	return Run(nil, "docker", append([]string{"exec", container}, args...)...)
}

func (e *Env) MustExec(container string, args ...string) string {
	e.T.Helper()
	out, err := e.Exec(container, args...)
	if err != nil {
		e.T.Fatal(err)
	}
	return out
}

// Sh runs a shell script in a container.
func (e *Env) Sh(container, script string) (string, error) {
	return e.Exec(container, "sh", "-c", script)
}

func (e *Env) MustSh(container, script string) string {
	e.T.Helper()
	out, err := e.Sh(container, script)
	if err != nil {
		e.T.Fatal(err)
	}
	return out
}

// WriteFile writes a file in a container through stdin (no trace in arguments).
func (e *Env) WriteFile(container, path, content string, mode string) {
	e.T.Helper()
	_, err := Run(strings.NewReader(content), "docker", "exec", "-i", container, "sh", "-c",
		fmt.Sprintf("umask 077; mkdir -p %q && cat > %q && chmod %s %q", filepath.Dir(path), path, mode, path))
	if err != nil {
		e.T.Fatal(err)
	}
}

func compose(args ...string) (string, error) {
	base := []string{"compose", "-f", filepath.Join(Repo(), "e2e", "docker-compose.yml"), "--profile", "restore"}
	return Run(nil, "docker", append(base, args...)...)
}

// Eventually retries cond until it returns nil or the timeout passes.
func (e *Env) Eventually(timeout, every time.Duration, what string, cond func() error) {
	e.T.Helper()
	if err := Wait(timeout, every, cond); err != nil {
		e.T.Fatalf("%s: did not happen within %s: %v", what, timeout, err)
	}
}

func Wait(timeout, every time.Duration, cond func() error) error {
	deadline := time.Now().Add(timeout)
	var err error
	for {
		if err = cond(); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(every)
	}
}

// ResetGateway recreates the gateway container (a clean machine with systemd).
func (e *Env) ResetGateway(name string) {
	e.T.Helper()
	svc := map[string]string{Gateway: "gateway", Gateway2: "gateway2"}[name]
	if _, err := compose("up", "-d", "--force-recreate", "--no-deps", svc); err != nil {
		e.T.Fatal(err)
	}
	e.Eventually(60*time.Second, 500*time.Millisecond, "systemd in container "+name, func() error {
		out, _ := e.Exec(name, "systemctl", "is-system-running")
		s := strings.TrimSpace(out)
		if s == "running" || s == "degraded" {
			return nil
		}
		return fmt.Errorf("stan %q", s)
	})
}

// StopContainer stops a container (e.g. a lost machine).
func (e *Env) StopContainer(name string) {
	e.T.Helper()
	if _, err := Run(nil, "docker", "rm", "-f", name); err != nil {
		e.T.Fatal(err)
	}
}

// InstallOptions are the gateway install parameters in tests.
type InstallOptions struct {
	Container string
	PublicIP  string
	Extra     []string
}

// InstallPrograms runs the real install.sh with the local release package.
func (e *Env) InstallPrograms(container string) {
	e.T.Helper()
	tarball := fmt.Sprintf("/dist/release/entrypoint_%s_linux_amd64.tar.gz", Version)
	e.MustSh(container, "ENTRYPOINT_TARBALL="+tarball+" sh /scripts/install.sh")
}

// Install installs the gateway: install.sh, then entrypoint install with Pebble as the CA.
func (e *Env) Install(o InstallOptions) {
	e.T.Helper()
	if o.Container == "" {
		o.Container = Gateway
	}
	if o.PublicIP == "" {
		o.PublicIP = GatewayIP
	}
	e.InstallPrograms(o.Container)
	args := []string{"entrypoint", "install",
		"--gateway-name", TunnelName, "--email", "admin@ep.test",
		"--acme-ca", "https://pebble:14000/dir", "--acme-staging-ca", "https://pebble:14000/dir",
		"--acme-ca-root", "/pebble/pebble.minica.pem", "--public-ip", o.PublicIP,
		"--drift-interval", "5s"}
	args = append(args, o.Extra...)
	e.MustExec(o.Container, args...)
}

// FreshGateway is the most common test start: a clean gateway, DNS with only the gateway name.
func (e *Env) FreshGateway(extra ...string) {
	e.T.Helper()
	e.SetDNS(map[string]string{})
	e.ResetGateway(Gateway)
	e.Install(InstallOptions{Extra: extra})
}

// EP runs the CLI on the gateway.
func (e *Env) EP(args ...string) (string, error) {
	return e.EPOn(Gateway, args...)
}

func (e *Env) EPOn(container string, args ...string) (string, error) {
	return e.Exec(container, append([]string{"entrypoint"}, args...)...)
}

func (e *Env) MustEP(args ...string) string {
	e.T.Helper()
	out, err := e.EP(args...)
	if err != nil {
		e.T.Fatal(err)
	}
	return out
}

// EPJSON runs a command with --json and decodes the result.
func (e *Env) EPJSON(out any, args ...string) {
	e.T.Helper()
	s := e.MustEP(append(args, "--json")...)
	if err := json.Unmarshal([]byte(s), out); err != nil {
		e.T.Fatalf("entrypoint %v: %v\n%s", args, err, s)
	}
}

// Service creates a service and returns its port.
func (e *Env) Service(name string, extra ...string) int {
	e.T.Helper()
	var res struct {
		Service struct{ Port int }
	}
	e.EPJSON(&res, append([]string{"service", "add", name}, extra...)...)
	return res.Service.Port
}

// Route adds a route and returns its ID.
func (e *Env) Route(hostPrefix string, extra ...string) string {
	e.T.Helper()
	var res struct {
		Route struct{ ID string }
	}
	e.EPJSON(&res, append([]string{"route", "add", hostPrefix}, extra...)...)
	return res.Route.ID
}

// Token issues a token and returns (id, ENTRYPOINT_CONNECT). The bundle goes on the secrets list.
func (e *Env) Token(service string) (string, string) {
	e.T.Helper()
	var res struct {
		Token struct {
			ID      string
			Connect string
		}
	}
	e.EPJSON(&res, "token", "create", "--service", service)
	if res.Token.Connect == "" {
		e.T.Fatal("no ENTRYPOINT_CONNECT in the response")
	}
	e.Secrets = append(e.Secrets, res.Token.Connect, TokenFromBundle(res.Token.Connect))
	return res.Token.ID, res.Token.Connect
}

var serialCounter atomic.Int64

func init() { serialCounter.Store(time.Now().Unix() % 1_000_000_000) }

// SetDNS sets the A records of the ep.test zone (besides ns and the gateway name).
func (e *Env) SetDNS(records map[string]string) {
	e.SetDNSWithGateway(GatewayIP, records)
}

func (e *Env) SetDNSWithGateway(gatewayIP string, records map[string]string) {
	e.T.Helper()
	serial := serialCounter.Add(1)
	var b strings.Builder
	fmt.Fprintf(&b, "$ORIGIN ep.test.\n$TTL 1\n@ IN SOA ns.ep.test. admin.ep.test. %d 60 60 600 1\n@ IN NS ns.ep.test.\nns IN A %s\ntunnel IN A %s\n", serial, DNSIP, gatewayIP)
	// Canary record: once it shows up, CoreDNS has loaded this zone version.
	fmt.Fprintf(&b, "serial-%d IN A 127.0.0.1\n", serial)
	names := make([]string, 0, len(records))
	for n := range records {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		rel := strings.TrimSuffix(strings.TrimSuffix(n, "."), ".ep.test")
		fmt.Fprintf(&b, "%s IN A %s\n", rel, records[n])
	}
	dir := RunDir("zones")
	tmp := filepath.Join(dir, ".ep.test.db.tmp")
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		e.T.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, "ep.test.db")); err != nil {
		e.T.Fatal(err)
	}
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", DNSIP+":53")
	}}
	canary := fmt.Sprintf("serial-%d.ep.test", serial)
	e.Eventually(15*time.Second, 100*time.Millisecond, "DNS zone reload", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := r.LookupHost(ctx, canary)
		return err
	})
}

// RunDir returns this environment copy's working directory (e2e/.run/<shard>/<name>).
func RunDir(name string) string {
	return filepath.Join(Repo(), "e2e", ".run", strconv.Itoa(Shard), name)
}

// PebbleRoots returns Pebble's current roots (they change on every Pebble start).
func PebbleRoots() (*x509.CertPool, error) {
	minica, err := os.ReadFile(filepath.Join(Repo(), "e2e", "pebble", "pebble.minica.pem"))
	if err != nil {
		return nil, err
	}
	mp := x509.NewCertPool()
	mp.AppendCertsFromPEM(minica)
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: mp}}}
	pool := x509.NewCertPool()
	for i := 0; i < 3; i++ {
		resp, err := c.Get(fmt.Sprintf("https://pebble:15000/roots/%d", i))
		if err != nil {
			return nil, err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			break
		}
		pool.AppendCertsFromPEM(b)
	}
	return pool, nil
}

// HTTPClient returns a client that trusts only Pebble (like a browser trusts Let's Encrypt).
func (e *Env) HTTPClient() *http.Client {
	e.T.Helper()
	pool, err := PebbleRoots()
	if err != nil {
		e.T.Fatal(err)
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: pool},
			ForceAttemptHTTP2: true,
			IdleConnTimeout:   2 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// WaitHTTPS waits until the host answers over HTTPS with a valid certificate.
func (e *Env) WaitHTTPS(url string, wantStatus int) {
	e.T.Helper()
	c := e.HTTPClient()
	e.Eventually(90*time.Second, time.Second, "HTTPS "+url, func() error {
		resp, err := c.Get(url)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != wantStatus {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return nil
	})
}

var bundleTokenRe = regexp.MustCompile(`"token":"(ept_[^"]+)"`)

// TokenFromBundle extracts the token from a bundle (to search logs for leaks).
func TokenFromBundle(b string) string {
	raw := strings.TrimPrefix(b, "epc1.")
	dec, err := decodeB64(raw)
	if err != nil {
		return ""
	}
	m := bundleTokenRe.FindStringSubmatch(string(dec))
	if m == nil {
		return ""
	}
	return m[1]
}

func safeName(s string) string {
	return regexp.MustCompile(`[^A-Za-z0-9_.-]+`).ReplaceAllString(s, "_")
}

// collect saves the test's artifacts: container and service logs, Caddy and frps configs, database state, result.
func (e *Env) collect(kind string) {
	dir := filepath.Join(Repo(), "e2e", "artifacts", safeName(e.T.Name()))
	_ = os.RemoveAll(dir)
	_ = os.MkdirAll(dir, 0o755)
	since := e.start.UTC().Format(time.RFC3339)
	running := map[string]bool{}
	if out, err := Run(nil, "docker", "ps", "--format", "{{.Names}}"); err == nil {
		for _, n := range strings.Fields(out) {
			running[n] = true
		}
	}
	for _, c := range Containers {
		if !running[c] {
			continue
		}
		out, _ := Run(nil, "docker", "logs", "--since", since, c)
		cmd := exec.Command("docker", "logs", "--since", since, c)
		b, _ := cmd.CombinedOutput()
		_ = out
		_ = os.WriteFile(filepath.Join(dir, c+".log"), b, 0o644)
	}
	for _, gw := range []string{Gateway, Gateway2} {
		if !running[gw] {
			continue
		}
		for _, u := range []string{"entrypointd", "entrypoint-caddy", "entrypoint-frps"} {
			out, _ := e.Exec(gw, "journalctl", "-u", u+".service", "--no-pager", "-o", "short-iso", "--since", "@"+fmt.Sprint(e.start.Unix()))
			_ = os.WriteFile(filepath.Join(dir, gw+"-"+u+".journal"), []byte(out), 0o644)
		}
		if out, err := e.Exec(gw, "cat", "/var/lib/entrypoint/caddy/caddy.json"); err == nil {
			_ = os.WriteFile(filepath.Join(dir, gw+"-caddy.json"), []byte(out), 0o644)
		}
		if out, err := e.Exec(gw, "cat", "/var/lib/entrypoint/frps/frps.json"); err == nil {
			out = regexp.MustCompile(`"password": "[^"]*"`).ReplaceAllString(out, `"password": "[redacted]"`)
			_ = os.WriteFile(filepath.Join(dir, gw+"-frps.json"), []byte(out), 0o644)
		}
		if out, err := e.Exec(gw, "sqlite3", "-readonly", "/var/lib/entrypoint/db/entrypoint.db",
			".mode line", "SELECT num, status, author, summary, error FROM revisions ORDER BY num;",
			"SELECT * FROM revocations;", "SELECT * FROM ports;", "SELECT * FROM components;"); err == nil {
			_ = os.WriteFile(filepath.Join(dir, gw+"-db.txt"), []byte(out), 0o644)
		}
	}
	for _, c := range []string{ClientA, ClientB, ClientN22, Attacker} {
		if !running[c] {
			continue
		}
		if out, err := e.Sh(c, "cat /tmp/*.log 2>/dev/null"); err == nil && out != "" {
			_ = os.WriteFile(filepath.Join(dir, c+"-app.log"), []byte(out), 0o644)
		}
	}
	result := "PASS"
	if e.T.Failed() {
		result = "FAIL"
	} else if e.T.Skipped() {
		result = "SKIP"
	}
	_ = os.WriteFile(filepath.Join(dir, "result.txt"), []byte(result+"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "kind.txt"), []byte(kind+"\n"), 0o644)
}
