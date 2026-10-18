// Pakiet harness steruje środowiskiem e2e z kontenera runner.
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

// Kopia środowiska (shard): każda ma własny projekt compose, podsieci i nazwy kontenerów,
// więc kilka kopii działa równolegle. EP_SHARD wybiera kopię, EP_SHARDS to ich liczba.
var (
	Shard  = envInt("EP_SHARD", 0)
	Shards = envInt("EP_SHARDS", 1)
	Net    = fmt.Sprintf("10.77.%d", Shard) // sieć publiczna środowiska
	Priv   = fmt.Sprintf("10.78.%d", Shard) // sieć prywatna („VPN”)

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
	// Sieć publiczna środowiska w Dockerze (dla kontenerów uruchamianych przez testy).
	Network = fmt.Sprintf("ep-e2e-%d_epnet", Shard)

	Containers = []string{Gateway, Gateway2, ClientA, ClientB, ClientN22, Attacker, Upstream, ACME, DNS}
)

const (
	TunnelName = "tunnel.ep.test"
	Version    = "0.1.0"
)

func name(s string) string { return fmt.Sprintf("ep%d-%s", Shard, s) }

// IP zwraca adres w sieci publicznej środowiska.
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

// Env to kontekst jednego testu.
type Env struct {
	T       *testing.T
	start   time.Time
	Secrets []string // wartości, których nie może być w żadnych logach
}

var experiments sync.Map

// Experiment oznacza test jako eksperyment: sprawdza zachowanie Caddy albo frp, na którym
// opiera się specyfikacja, a nie nasz kod. Uruchamia go `make e2e-experiments`, nie `make e2e`.
// Wywołanie musi poprzedzać h.New.
func Experiment(t *testing.T) { experiments.Store(t.Name(), true) }

func New(t *testing.T) *Env {
	if os.Getenv("EP_E2E") == "" {
		t.Skip("testy e2e uruchamia `make e2e`")
	}
	_, exp := experiments.Load(t.Name())
	only := os.Getenv("EP_EXPERIMENTS") != ""
	switch {
	case exp && !only:
		t.Skip("eksperyment: uruchamia go make e2e-experiments")
	case !exp && only:
		t.Skip("nie jest eksperymentem")
	}
	if Shards > 1 && shardOf(t.Name(), exp) != Shard {
		t.Skip("test przydzielony innej kopii środowiska")
	}
	e := &Env{T: t, start: time.Now().Add(-time.Second)}
	kind := "e2e"
	if exp {
		kind = "experiment"
	}
	t.Cleanup(func() { e.collect(kind) })
	return e
}

// Przydział testów do kopii środowiska: najdłuższe testy (czasy z poprzedniego przebiegu,
// e2e/durations.json) trafiają kolejno do najmniej obciążonej kopii. Nieznane testy: hash nazwy.
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

// Run wykonuje polecenie i zwraca stdout; stderr trafia do błędu.
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

// Exec uruchamia polecenie w kontenerze.
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

// Sh uruchamia skrypt powłoki w kontenerze.
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

// WriteFile zapisuje plik w kontenerze przez stdin (bez śladu w argumentach).
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

// Eventually ponawia cond, aż zwróci nil albo minie timeout.
func (e *Env) Eventually(timeout, every time.Duration, what string, cond func() error) {
	e.T.Helper()
	if err := Wait(timeout, every, cond); err != nil {
		e.T.Fatalf("%s: nie nastąpiło w %s: %v", what, timeout, err)
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

// ResetGateway tworzy kontener bramy od nowa (czysta maszyna z systemd).
func (e *Env) ResetGateway(name string) {
	e.T.Helper()
	svc := map[string]string{Gateway: "gateway", Gateway2: "gateway2"}[name]
	if _, err := compose("up", "-d", "--force-recreate", "--no-deps", svc); err != nil {
		e.T.Fatal(err)
	}
	e.Eventually(60*time.Second, 500*time.Millisecond, "systemd w kontenerze "+name, func() error {
		out, _ := e.Exec(name, "systemctl", "is-system-running")
		s := strings.TrimSpace(out)
		if s == "running" || s == "degraded" {
			return nil
		}
		return fmt.Errorf("stan %q", s)
	})
}

// StopContainer zatrzymuje kontener (np. utrata maszyny).
func (e *Env) StopContainer(name string) {
	e.T.Helper()
	if _, err := Run(nil, "docker", "rm", "-f", name); err != nil {
		e.T.Fatal(err)
	}
}

// InstallOptions to parametry instalacji bramy w testach.
type InstallOptions struct {
	Container string
	PublicIP  string
	Extra     []string
}

// InstallPrograms uruchamia prawdziwy install.sh z lokalną paczką wydania.
func (e *Env) InstallPrograms(container string) {
	e.T.Helper()
	tarball := fmt.Sprintf("/dist/release/entrypoint_%s_linux_amd64.tar.gz", Version)
	e.MustSh(container, "ENTRYPOINT_TARBALL="+tarball+" sh /scripts/install.sh")
}

// Install instaluje bramę: install.sh, potem entrypoint install z Pebble jako CA.
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

// FreshGateway to najczęstszy początek testu: czysta brama, DNS tylko z nazwą bramy.
func (e *Env) FreshGateway(extra ...string) {
	e.T.Helper()
	e.SetDNS(map[string]string{})
	e.ResetGateway(Gateway)
	e.Install(InstallOptions{Extra: extra})
}

// EP uruchamia CLI na bramie.
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

// EPJSON uruchamia polecenie z --json i dekoduje wynik.
func (e *Env) EPJSON(out any, args ...string) {
	e.T.Helper()
	s := e.MustEP(append(args, "--json")...)
	if err := json.Unmarshal([]byte(s), out); err != nil {
		e.T.Fatalf("entrypoint %v: %v\n%s", args, err, s)
	}
}

// Service tworzy serwis i zwraca jego port.
func (e *Env) Service(name string, extra ...string) int {
	e.T.Helper()
	var res struct {
		Service struct{ Port int }
	}
	e.EPJSON(&res, append([]string{"service", "add", name}, extra...)...)
	return res.Service.Port
}

// Route dodaje trasę i zwraca jej identyfikator.
func (e *Env) Route(hostPrefix string, extra ...string) string {
	e.T.Helper()
	var res struct {
		Route struct{ ID string }
	}
	e.EPJSON(&res, append([]string{"route", "add", hostPrefix}, extra...)...)
	return res.Route.ID
}

// Token wydaje token i zwraca (id, ENTRYPOINT_CONNECT). Pakiet trafia na listę sekretów.
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
		e.T.Fatal("brak ENTRYPOINT_CONNECT w odpowiedzi")
	}
	e.Secrets = append(e.Secrets, res.Token.Connect, TokenFromBundle(res.Token.Connect))
	return res.Token.ID, res.Token.Connect
}

var serialCounter atomic.Int64

func init() { serialCounter.Store(time.Now().Unix() % 1_000_000_000) }

// SetDNS ustawia rekordy A strefy ep.test (oprócz ns i nazwy bramy).
func (e *Env) SetDNS(records map[string]string) {
	e.SetDNSWithGateway(GatewayIP, records)
}

func (e *Env) SetDNSWithGateway(gatewayIP string, records map[string]string) {
	e.T.Helper()
	serial := serialCounter.Add(1)
	var b strings.Builder
	fmt.Fprintf(&b, "$ORIGIN ep.test.\n$TTL 1\n@ IN SOA ns.ep.test. admin.ep.test. %d 60 60 600 1\n@ IN NS ns.ep.test.\nns IN A %s\ntunnel IN A %s\n", serial, DNSIP, gatewayIP)
	// Rekord kontrolny: jego pojawienie się oznacza, że CoreDNS wczytał tę wersję strefy.
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
	e.Eventually(15*time.Second, 100*time.Millisecond, "przeładowanie strefy DNS", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := r.LookupHost(ctx, canary)
		return err
	})
}

// RunDir zwraca katalog roboczy tej kopii środowiska (e2e/.run/<shard>/<nazwa>).
func RunDir(name string) string {
	return filepath.Join(Repo(), "e2e", ".run", strconv.Itoa(Shard), name)
}

// PebbleRoots zwraca bieżące korzenie Pebble (zmieniają się przy każdym starcie Pebble).
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

// HTTPClient zwraca klienta ufającego wyłącznie Pebble (jak przeglądarka ufa Let's Encrypt).
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

// WaitHTTPS czeka, aż host odpowie przez HTTPS z ważnym certyfikatem.
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
			return fmt.Errorf("kod %d", resp.StatusCode)
		}
		return nil
	})
}

var bundleTokenRe = regexp.MustCompile(`"token":"(ept_[^"]+)"`)

// TokenFromBundle wyciąga token z pakietu (do wyszukiwania wycieków w logach).
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

// collect zapisuje artefakty testu: logi kontenerów i usług, konfiguracje Caddy i frps, stan bazy, wynik.
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
			out = regexp.MustCompile(`"password": "[^"]*"`).ReplaceAllString(out, `"password": "[ukryte]"`)
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
