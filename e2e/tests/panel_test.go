package tests

// e2e suite 10: the web panel, through Caddy over HTTPS like a browser.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	h "github.com/k18k/entrypoint/e2e/harness"
)

const panelName = "panel.ep.test"

type panelClient struct {
	t    *testing.T
	c    *http.Client
	csrf string
}

func (p *panelClient) do(method, path string, body any, hdr map[string]string) (int, map[string]any) {
	p.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "https://"+panelName+path, r)
	req.Header.Set("Origin", "https://"+panelName)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.csrf != "" {
		req.Header.Set("X-CSRF-Token", p.csrf)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := p.c.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		out = map[string]any{"raw": string(raw)}
	}
	return resp.StatusCode, out
}

func newPanelClient(e *h.Env) *panelClient {
	jar, _ := cookiejar.New(nil)
	c := e.HTTPClient()
	c.Jar = jar
	return &panelClient{t: e.T, c: c}
}

// Login, cookie attributes, CSRF and Origin checks, then the whole app flow from the panel:
// create an app with its host, connect a replica with the issued bundle, issue a VPN key,
// revoke the token; finally the per-IP login limit.
func TestPanel_LoginAppFlowAndProtection(t *testing.T) {
	e := h.New(t)
	e.StopAllApps()
	t.Cleanup(e.StopAllApps)
	e.SetDNS(map[string]string{panelName: h.GatewayIP, vpnName: h.GatewayIP, "web.ep.test": h.GatewayIP})
	e.ResetGateway(h.Gateway)
	e.Install(h.InstallOptions{Extra: []string{"--panel-name", panelName, "--vpn-name", vpnName}})
	pw := "panel-password-e2e-123"
	e.Secrets = append(e.Secrets, pw)
	e.MustSh(h.Gateway, "echo "+pw+" | entrypoint panel password")
	e.WaitHTTPS("https://"+panelName+"/api/session", 200)

	p := newPanelClient(e)
	// The frontend with its security headers.
	resp, err := p.c.Get("https://" + panelName + "/apps")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")

	noSession, _ := p.do("GET", "/api/v1/status", nil, nil)
	foreign, _ := p.do("POST", "/api/login", map[string]string{"password": pw}, map[string]string{"Origin": "https://evil.example"})
	wrong, _ := p.do("POST", "/api/login", map[string]string{"password": "wrong-password"}, nil)
	code, login := p.do("POST", "/api/login", map[string]string{"password": pw}, nil)
	if code != 200 {
		t.Fatalf("login: %d %v", code, login)
	}
	u, _ := http.NewRequest("GET", "https://"+panelName+"/", nil)
	cookies := p.c.Jar.Cookies(u.URL)
	csrf, _ := login["csrf"].(string)

	statusCode, _ := p.do("GET", "/api/v1/status", nil, nil)
	noCSRF, _ := p.do("POST", "/api/v1/services", map[string]string{"name": "web"}, nil)

	// The app flow, as the frontend does it.
	p.csrf = csrf
	if c, out := p.do("POST", "/api/v1/services", map[string]string{"name": "web", "health": "/health"}, nil); c != 200 {
		t.Fatalf("service: %d %v", c, out)
	}
	if c, out := p.do("POST", "/api/v1/routes", map[string]any{"host": "web.ep.test", "prefix": "/", "service": "web"}, nil); c != 200 {
		t.Fatalf("route: %d %v", c, out)
	}
	c, tok := p.do("POST", "/api/v1/tokens", map[string]any{"service": "web", "label": "panel"}, nil)
	if c != 200 {
		t.Fatalf("token: %d %v", c, tok)
	}
	tk := tok["token"].(map[string]any)
	connect, tokenID := tk["connect"].(string), tk["id"].(string)
	e.Secrets = append(e.Secrets, connect, h.TokenFromBundle(connect))
	e.StartApp(h.AppOptions{Container: h.ClientA, Connect: connect, Service: "web", Replica: "a", Wait: true})
	e.WaitHTTPS("https://web.ep.test/health", 200)
	_, revs := p.do("GET", "/api/v1/revisions", nil, nil)
	_, key := p.do("POST", "/api/v1/vpn/keys", map[string]any{"reusable": false, "expiration": "1h"}, nil)
	bundle, _ := key["bundle"].(string)
	revokeCode, _ := p.do("POST", "/api/v1/tokens/"+tokenID+"/revoke", nil, nil)
	var after int
	e.Eventually(30*time.Second, time.Second, "replica gone after revocation", func() error {
		after, _, _ = get(e.HTTPClient(), "https://web.ep.test/health")
		if after == 200 {
			return fmt.Errorf("still 200")
		}
		return nil
	})
	author := e.MustEP("revision", "list")
	logoutCode, _ := p.do("POST", "/api/logout", nil, nil)
	afterLogout, _ := p.do("GET", "/api/v1/status", nil, nil)

	// Per-IP limit: 10 failures, then even the right password is refused.
	q := newPanelClient(e)
	for i := 0; i < 10; i++ {
		q.do("POST", "/api/login", map[string]string{"password": "wrong-password"}, nil)
	}
	limited, _ := q.do("POST", "/api/login", map[string]string{"password": pw}, nil)

	// The backend is not reachable from outside, only through Caddy.
	_, directErr := e.Sh(h.Attacker, "curl -s --max-time 3 http://"+h.GatewayIP+":7700/api/session")

	var cookieDesc string
	for _, c := range cookies {
		cookieDesc += c.Name + " "
	}
	result(t, "UI %d bytes, CSP %q; no session %d, foreign origin %d, wrong password %d, login %d (cookie %s); status %d, change without CSRF %d; app created, replica served; VPN bundle issued: %v; revoke %d -> web %d; revisions by panel: %v; logout %d -> %d; 11th login from one IP %d; backend from outside: err=%v",
		len(page), csp, noSession, foreign, wrong, code, strings.TrimSpace(cookieDesc), statusCode, noCSRF, strings.HasPrefix(bundle, "epv1."), revokeCode, after,
		strings.Contains(author, "panel"), logoutCode, afterLogout, limited, directErr != nil)
	if !strings.Contains(string(page), "<div id=\"root\">") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatal("no frontend or no CSP")
	}
	if noSession != 401 || foreign != 403 || wrong != 401 || statusCode != 200 || noCSRF != 403 {
		t.Fatal("session or CSRF protection failed")
	}
	if !strings.HasPrefix(bundle, "epv1.") || revokeCode != 200 || after == 200 || !strings.Contains(author, "panel") {
		t.Fatalf("panel actions: revisions %v", revs)
	}
	if afterLogout != 401 || limited != 429 || directErr == nil {
		t.Fatal("logout, login limit or backend exposure")
	}
}
