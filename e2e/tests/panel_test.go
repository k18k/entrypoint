package tests

// e2e suite 10: the web panel, through Caddy over HTTPS like a browser.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"regexp"
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

// ceremony runs a WebAuthn registration or sign-in with the software passkey, as the frontend does.
func (p *panelClient) ceremony(kind string, begin any, key *h.Passkey) (int, map[string]any) {
	p.t.Helper()
	code, out := p.do("POST", "/api/"+kind+"/begin", begin, nil)
	if code != 200 {
		return code, out
	}
	opts, _ := json.Marshal(out["options"])
	var body []byte
	var err error
	if kind == "register" {
		body, err = key.Create(opts)
	} else {
		body, err = key.Get(opts)
	}
	if err != nil {
		p.t.Fatal(err)
	}
	return p.do("POST", "/api/"+kind+"/finish?ceremony="+out["ceremony"].(string), json.RawMessage(body), nil)
}

var registerLink = regexp.MustCompile(`https://` + regexp.QuoteMeta(panelName) + `/register#(\S+)`)

func newPanelClient(e *h.Env) *panelClient {
	jar, _ := cookiejar.New(nil)
	c := e.HTTPClient()
	c.Jar = jar
	return &panelClient{t: e.T, c: c}
}

// Passkey registration with a one-time CLI link, sign-in, cookie attributes, CSRF and Origin
// checks, then the whole app flow from the panel: create an app with its host, connect a replica
// with the issued bundle, issue a VPN key, revoke the token; finally key removal and the per-IP limit.
func TestPanel_LoginAppFlowAndProtection(t *testing.T) {
	e := h.New(t)
	e.StopAllApps()
	t.Cleanup(e.StopAllApps)
	e.SetDNS(map[string]string{panelName: h.GatewayIP, vpnName: h.GatewayIP, "web.ep.test": h.GatewayIP})
	e.ResetGateway(h.Gateway)
	e.Install(h.InstallOptions{Extra: []string{"--panel-name", panelName, "--vpn-name", vpnName}})
	e.WaitHTTPS("https://"+panelName+"/api/session", 200)
	m := registerLink.FindStringSubmatch(e.MustEP("panel", "register"))
	if m == nil {
		t.Fatal("panel register: no link")
	}
	link := m[1]
	e.Secrets = append(e.Secrets, link)
	key := &h.Passkey{Origin: "https://" + panelName}

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
	foreign, _ := p.do("POST", "/api/register/begin", map[string]string{"token": link}, map[string]string{"Origin": "https://evil.example"})
	wrong, _ := p.ceremony("login", nil, &h.Passkey{Origin: "https://" + panelName})
	if c, out := p.ceremony("register", map[string]string{"token": link}, key); c != 200 {
		t.Fatalf("registration: %d %v", c, out)
	}
	reused, _ := p.do("POST", "/api/register/begin", map[string]string{"token": link}, nil)
	p = newPanelClient(e) // a fresh browser signs in with the passkey
	code, login := p.ceremony("login", nil, key)
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
	_, vpnKey := p.do("POST", "/api/v1/vpn/keys", map[string]any{"reusable": false, "expiration": "1h"}, nil)
	bundle, _ := vpnKey["bundle"].(string)
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

	// Removing the passkey with the CLI ends sessions.
	p.ceremony("login", nil, key)
	keyList := e.MustEP("panel", "keys")
	keyID := strings.Fields(strings.Split(keyList, "\n")[1])[0]
	e.MustEP("panel", "keys", "rm", keyID)
	afterRemove, _ := p.do("GET", "/api/v1/status", nil, nil)

	// Per-IP limit: 10 failures (unknown passkeys), then sign-in is refused.
	q := newPanelClient(e)
	for i := 0; i < 10; i++ {
		q.ceremony("login", nil, &h.Passkey{Origin: "https://" + panelName})
	}
	limited, _ := q.do("POST", "/api/login/begin", nil, nil)

	// The backend is not reachable from outside, only through Caddy.
	_, directErr := e.Sh(h.Attacker, "curl -s --max-time 3 http://"+h.GatewayIP+":7700/api/session")

	var cookieDesc string
	for _, c := range cookies {
		cookieDesc += c.Name + " "
	}
	result(t, "UI %d bytes, CSP %q; no session %d, registration from a foreign origin %d, unregistered passkey %d, link reused %d, passkey sign-in %d (cookie %s); status %d, change without CSRF %d; app created, replica served; VPN bundle issued: %v; revoke %d -> web %d; revisions by panel: %v; logout %d -> %d; key removed -> %d; 11th sign-in from one IP %d; backend from outside: err=%v",
		len(page), csp, noSession, foreign, wrong, reused, code, strings.TrimSpace(cookieDesc), statusCode, noCSRF, strings.HasPrefix(bundle, "epv1."), revokeCode, after,
		strings.Contains(author, "panel"), logoutCode, afterLogout, afterRemove, limited, directErr != nil)
	if !strings.Contains(string(page), "<div id=\"root\">") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatal("no frontend or no CSP")
	}
	if noSession != 401 || foreign != 403 || wrong != 401 || reused != 403 || statusCode != 200 || noCSRF != 403 {
		t.Fatal("session or CSRF protection failed")
	}
	if !strings.HasPrefix(bundle, "epv1.") || revokeCode != 200 || after == 200 || !strings.Contains(author, "panel") {
		t.Fatalf("panel actions: revisions %v", revs)
	}
	if afterLogout != 401 || afterRemove != 401 || limited != 429 || directErr == nil {
		t.Fatal("logout, login limit or backend exposure")
	}
}
