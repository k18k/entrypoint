package tests

// e2e suite 3: HTTP routing.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	h "github.com/k18k/entrypoint/e2e/harness"
)

type echo struct {
	Name       string              `json:"name"`
	Host       string              `json:"host"`
	Path       string              `json:"path"`
	RequestURI string              `json:"requestURI"`
	Proto      string              `json:"proto"`
	Headers    map[string][]string `json:"headers"`
}

// A second upstream in the private network (same container, different port and name).
func secondUpstream(e *h.Env) {
	e.MustSh(h.Upstream, "pgrep -f 'addr :8081' >/dev/null || (nohup testsrv -addr :8081 -name second > /tmp/second.log 2>&1 &)")
}

func routingSetup(t *testing.T) *h.Env {
	e := h.New(t)
	e.StopAllApps()
	e.FreshGateway()
	secondUpstream(e)
	e.SetDNS(map[string]string{"r.ep.test": h.GatewayIP, "x.w.ep.test": h.GatewayIP, "y.w.ep.test": h.GatewayIP, "lim.ep.test": h.GatewayIP})
	e.Route("r.ep.test", "--to", h.UpstreamVPN)
	e.Route("r.ep.test/api", "--to", h.UpstreamIP+":8081")
	e.Route("r.ep.test/api/v2", "--to", h.UpstreamVPN, "--strip-prefix")
	e.Route("*.w.ep.test", "--to", h.UpstreamVPN, "--tls-name", "y.w.ep.test")
	e.Route("x.w.ep.test", "--to", h.UpstreamIP+":8081")
	e.Route("lim.ep.test", "--to", h.UpstreamVPN, "--max-body", "1MB", "--header-timeout", "2s")
	e.WaitHTTPS("https://r.ep.test/", 200)
	return e
}

func doEcho(t *testing.T, c *http.Client, req *http.Request) echo {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var e echo
	_ = json.NewDecoder(resp.Body).Decode(&e)
	e.Proto = fmt.Sprintf("%s (client: %s)", e.Proto, resp.Proto)
	return e
}

func getEcho(t *testing.T, c *http.Client, url string) echo {
	req, _ := http.NewRequest("GET", url, nil)
	return doEcho(t, c, req)
}

func TestRouting_HostPrefixAndBoundaries(t *testing.T) {
	e := routingSetup(t)
	c := e.HTTPClient()
	cases := []struct{ url, name, path string }{
		{"https://r.ep.test/x", "vpn-upstream", "/x"},
		{"https://r.ep.test/api", "second", "/api"},
		{"https://r.ep.test/api/x", "second", "/api/x"},      // prefix passed through unchanged
		{"https://r.ep.test/apix", "vpn-upstream", "/apix"},  // segment boundary
		{"https://r.ep.test/api/v2/x", "vpn-upstream", "/x"}, // longest prefix + stripPrefix
		{"https://r.ep.test/api/v2", "vpn-upstream", "/"},    // stripPrefix of the whole path
		{"https://r.ep.test/api/v2x", "second", "/api/v2x"},  // segment boundary of a longer prefix
		{"https://x.w.ep.test/a", "second", "/a"},            // an exact host beats a wildcard
		{"https://y.w.ep.test/a", "vpn-upstream", "/a"},      // wildcard, on-demand certificate
	}
	var lines []string
	for _, tc := range cases {
		var got echo
		e.Eventually(60*time.Second, time.Second, tc.url, func() error {
			resp, err := c.Get(tc.url)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			got = echo{}
			return json.NewDecoder(resp.Body).Decode(&got)
		})
		lines = append(lines, fmt.Sprintf("%s -> %s %s", strings.TrimPrefix(tc.url, "https://"), got.Name, got.Path))
		if got.Name != tc.name || got.Path != tc.path {
			t.Errorf("%s: %s %q, expected %s %q", tc.url, got.Name, got.Path, tc.name, tc.path)
		}
		if got.Host != strings.Split(strings.TrimPrefix(tc.url, "https://"), "/")[0] {
			t.Errorf("%s: Host %q was not passed through unchanged", tc.url, got.Host)
		}
	}
	result(t, "%s", strings.Join(lines, "; "))
}

func TestRouting_Conflicts(t *testing.T) {
	e := routingSetup(t)
	var lines []string
	for _, spec := range [][]string{
		{"r.ep.test/api", "--to", h.UpstreamVPN},
		{"R.EP.TEST/api/", "--to", h.UpstreamVPN},
		{"r.ep.test/api", "--service", "missing"},
		{"r.ep.test/b", "--to", "not-an-ip:80"},
		{"r.ep.test/../x", "--to", h.UpstreamVPN},
		{"*.test", "--to", h.UpstreamVPN},
		{"*.*.ep.test", "--to", h.UpstreamVPN},
		{h.GatewayIP, "--to", h.UpstreamVPN},
		{"r.ep.test/c", "--to", h.UpstreamVPN, "--acme-staging"},
	} {
		out, err := e.EP(append([]string{"route", "add"}, spec...)...)
		lines = append(lines, fmt.Sprintf("%s: rejected=%v", spec[0], err != nil))
		if err == nil {
			t.Errorf("route %v accepted: %s", spec, out)
		}
	}
	// baseRev conflict.
	var res struct{ Revision struct{ Num int64 } }
	e.EPJSON(&res, "route", "add", "r.ep.test/d", "--to", h.UpstreamVPN)
	_, err := e.EP("route", "add", "r.ep.test/e", "--to", h.UpstreamVPN, "--base-rev", fmt.Sprint(res.Revision.Num-1))
	lines = append(lines, fmt.Sprintf("stale baseRev: rejected=%v", err != nil))
	if err == nil || !strings.Contains(err.Error(), "base revision conflict") {
		t.Errorf("baseRev: %v", err)
	}
	_, err = e.EP("route", "add", "r.ep.test/e", "--to", h.UpstreamVPN, "--base-rev", fmt.Sprint(res.Revision.Num))
	if err != nil {
		t.Errorf("current baseRev rejected: %v", err)
	}
	result(t, "%s", strings.Join(lines, "; "))
}

// A raw HTTP/1.1 request over TLS (paths the Go client would not send unchanged).
func rawRequest(t *testing.T, e *h.Env, host, target string, headers ...string) (int, echo) {
	t.Helper()
	pool, _ := h.PebbleRoots()
	conn, err := tls.Dial("tcp", net.JoinHostPort(host, "443"), &tls.Config{RootCAs: pool, ServerName: host, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var b bytes.Buffer
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n", target, host)
	for _, hd := range headers {
		b.WriteString(hd + "\r\n")
	}
	b.WriteString("\r\n")
	conn.Write(b.Bytes())
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var ec echo
	_ = json.NewDecoder(resp.Body).Decode(&ec)
	return resp.StatusCode, ec
}

func TestRouting_PathNormalization(t *testing.T) {
	h.Experiment(t)
	e := routingSetup(t)
	cases := []struct{ target, wantName, wantPath string }{
		{"/api/../x", "", ""},
		{"/x/../api/y", "", ""},
		{"//api/x", "", ""},
		{"/api//x", "", ""},
		{"/api%2Fx", "", ""},
		{"/%61pi/x", "", ""},
		{"/API/x", "", ""},
		{"/api/%2e%2e/x", "", ""},
	}
	var lines []string
	for _, tc := range cases {
		code, ec := rawRequest(t, e, "r.ep.test", tc.target)
		lines = append(lines, fmt.Sprintf("%s -> %d %s path=%q uri=%q", tc.target, code, ec.Name, ec.Path, ec.RequestURI))
		// Security property: the /api route (upstream "second") only gets paths
		// that lie under /api after normalization.
		if ec.Name == "second" && !(strings.HasPrefix(strings.ToLower(ec.Path), "/api/") || strings.ToLower(ec.Path) == "/api") {
			t.Errorf("%s reached the /api route with path %q", tc.target, ec.Path)
		}
	}
	result(t, "%s", strings.Join(lines, "; "))
}

func TestRouting_ForwardedHeaders(t *testing.T) {
	e := routingSetup(t)
	_, ec := rawRequest(t, e, "r.ep.test", "/h", "X-Forwarded-For: 1.2.3.4", "X-Forwarded-Host: evil.example", "X-Forwarded-Proto: http", "X-Real-IP: 1.2.3.4", "Forwarded: for=1.2.3.4")
	xff := strings.Join(ec.Headers["X-Forwarded-For"], ",")
	xfh := strings.Join(ec.Headers["X-Forwarded-Host"], ",")
	xfp := strings.Join(ec.Headers["X-Forwarded-Proto"], ",")
	result(t, "upstream sees X-Forwarded-For=%q X-Forwarded-Host=%q X-Forwarded-Proto=%q X-Real-IP=%q Forwarded=%q Host=%q",
		xff, xfh, xfp, strings.Join(ec.Headers["X-Real-Ip"], ","), strings.Join(ec.Headers["Forwarded"], ","), ec.Host)
	if xff != h.RunnerIP || xfh != "r.ep.test" || xfp != "https" || ec.Host != "r.ep.test" ||
		len(ec.Headers["X-Real-Ip"]) > 0 || len(ec.Headers["Forwarded"]) > 0 {
		t.Fatal("client-supplied X-Forwarded-* headers were not replaced")
	}
}

func TestRouting_LimitsAndTimeouts(t *testing.T) {
	e := routingSetup(t)
	e.WaitHTTPS("https://lim.ep.test/", 200)
	c := e.HTTPClient()
	post := func(n int) int {
		resp, err := c.Post("https://lim.ep.test/upload", "application/octet-stream", bytes.NewReader(make([]byte, n)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	small, big := post(512<<10), post(2<<20)
	code, _, _ := get(c, "https://lim.ep.test/slow?ms=4000")
	codeOK, _, _ := get(c, "https://lim.ep.test/slow?ms=500")
	// The default 50 MB limit on another route.
	r51 := func() int {
		resp, err := c.Post("https://r.ep.test/upload", "application/octet-stream", bytes.NewReader(make([]byte, 51<<20)))
		if err != nil {
			return 0
		}
		resp.Body.Close()
		return resp.StatusCode
	}()
	result(t, "1 MB limit: 512 KB -> %d, 2 MB -> %d; default 50 MB: 51 MB -> %d; header-timeout 2 s: response after 4 s -> %d, after 0.5 s -> %d", small, big, r51, code, codeOK)
	if small != 200 || big != 413 || r51 != 413 || code != 504 || codeOK != 200 {
		t.Fatal("route limits do not work")
	}
}

func TestRouting_UpstreamWebSocketSSEAndHTTP2(t *testing.T) {
	e := routingSetup(t)
	c := e.HTTPClient()
	ec := getEcho(t, c, "https://r.ep.test/proto")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "wss://r.ep.test/ws", &websocket.DialOptions{HTTPClient: c})
	if err != nil {
		t.Fatal(err)
	}
	ws.Write(ctx, websocket.MessageText, []byte("ping"))
	_, m, err := ws.Read(ctx)
	if err != nil || string(m) != "ping" {
		t.Fatalf("WebSocket through an IP:port upstream: %q %v", m, err)
	}
	ws.Close(websocket.StatusNormalClosure, "")
	lines, done := readSSE(ctx, e, "https://r.ep.test/sse")
	events := 0
	for events < 3 {
		select {
		case l := <-lines:
			events += strings.Count(l, "data:")
		case err := <-done:
			t.Fatalf("SSE: %v", err)
		case <-ctx.Done():
			t.Fatal("SSE: no events")
		}
	}
	// Upstream through a tunnel: WebSocket works too (node:http adapter).
	result(t, "HTTP to the client: %s; WebSocket echo through an upstream in the private network: ok; SSE: %d events", ec.Proto, events)
	if !strings.Contains(ec.Proto, "client: HTTP/2.0") {
		t.Fatal("no HTTP/2 to the client")
	}
}

func TestRouting_HTTPRedirect(t *testing.T) {
	h.Experiment(t)
	routingSetup(t)
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get("http://r.ep.test/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	result(t, "HTTP -> %d %s", resp.StatusCode, loc)
	if resp.StatusCode != 308 || loc != "https://r.ep.test/x" {
		t.Fatal("no redirect to HTTPS")
	}
}
