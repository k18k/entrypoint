package panel

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/k18k/entrypoint/gateway/internal/config"
)

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := VerifyPassword("correct horse battery", h); !ok {
		t.Fatal("right password rejected")
	}
	if ok, _ := VerifyPassword("wrong", h); ok {
		t.Fatal("wrong password accepted")
	}
}

func setup(t *testing.T) (*Server, http.Handler, *[]string) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.GatewayName = "tunnel.example.com"
	cfg.Panel.Name = "panel.example.com"
	cfg.DataDir = dir
	cfg.APISocket = filepath.Join(dir, "api.sock")
	os.MkdirAll(cfg.PanelDir(), 0o700)
	h, _ := HashPassword("correct horse battery")
	os.WriteFile(cfg.PanelPasswordFile(), []byte(h+"\n"), 0o600)
	// A stand-in for entrypointd that records what reaches it.
	var seen []string
	ln, err := net.Listen("unix", cfg.APISocket)
	if err != nil {
		t.Fatal(err)
	}
	api := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" cookie="+r.Header.Get("Cookie"))
		io.WriteString(w, `{"ok":true}`)
	})}
	go api.Serve(ln)
	t.Cleanup(func() { api.Close() })
	s := New(cfg, fstest.MapFS{"index.html": {Data: []byte("<html>panel</html>")}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return s, s.Handler(), &seen
}

func do(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestLoginSessionAndCSRF(t *testing.T) {
	_, h, seen := setup(t)
	origin := map[string]string{"Origin": "https://panel.example.com"}

	if w := do(h, "GET", "/api/v1/status", "", nil); w.Code != 401 {
		t.Fatalf("API without a session: %d", w.Code)
	}
	if w := do(h, "POST", "/api/login", `{"password":"correct horse battery"}`, map[string]string{"Origin": "https://evil.example"}); w.Code != 403 {
		t.Fatalf("login from a foreign origin: %d", w.Code)
	}
	if w := do(h, "POST", "/api/login", `{"password":"nope"}`, origin); w.Code != 401 {
		t.Fatalf("wrong password: %d", w.Code)
	}
	w := do(h, "POST", "/api/login", `{"password":"correct horse battery"}`, origin)
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	c := w.Result().Cookies()[0]
	if c.Name != "__Host-ep_panel" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie attributes: %+v", c)
	}
	csrf := strings.Split(strings.Split(w.Body.String(), `"csrf":"`)[1], `"`)[0]
	cookie := map[string]string{"Cookie": c.Name + "=" + c.Value}

	if w := do(h, "GET", "/api/v1/status", "", cookie); w.Code != 200 {
		t.Fatalf("API with a session: %d", w.Code)
	}
	noCSRF := map[string]string{"Cookie": cookie["Cookie"], "Origin": origin["Origin"]}
	if w := do(h, "POST", "/api/v1/services", `{}`, noCSRF); w.Code != 403 {
		t.Fatalf("change without CSRF: %d", w.Code)
	}
	badOrigin := map[string]string{"Cookie": cookie["Cookie"], "Origin": "https://evil.example", "X-CSRF-Token": csrf}
	if w := do(h, "POST", "/api/v1/services", `{}`, badOrigin); w.Code != 403 {
		t.Fatalf("change from a foreign origin: %d", w.Code)
	}
	ok := map[string]string{"Cookie": cookie["Cookie"], "Origin": origin["Origin"], "X-CSRF-Token": csrf}
	if w := do(h, "POST", "/api/v1/services", `{}`, ok); w.Code != 200 {
		t.Fatalf("change with CSRF: %d", w.Code)
	}
	for _, s := range *seen {
		if strings.Contains(s, "cookie=__Host") {
			t.Fatalf("session cookie forwarded to entrypointd: %s", s)
		}
	}
	if got := strings.Join(*seen, ","); !strings.Contains(got, "GET /v1/status") || !strings.Contains(got, "POST /v1/services") {
		t.Fatalf("proxied: %s", got)
	}
	// Logout ends the session.
	do(h, "POST", "/api/logout", "", ok)
	if w := do(h, "GET", "/api/v1/status", "", cookie); w.Code != 401 {
		t.Fatalf("API after logout: %d", w.Code)
	}
	// The SPA gets index.html for client-side routes.
	if w := do(h, "GET", "/vpn", "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "panel") || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("UI: %d %v", w.Code, w.Header())
	}
}

func TestLoginRateLimit(t *testing.T) {
	_, h, _ := setup(t)
	origin := map[string]string{"Origin": "https://panel.example.com"}
	for i := 0; i < maxFails; i++ {
		do(h, "POST", "/api/login", `{"password":"nope"}`, origin)
	}
	if w := do(h, "POST", "/api/login", `{"password":"correct horse battery"}`, origin); w.Code != 429 {
		t.Fatalf("after %d failures: %d", maxFails, w.Code)
	}
}

func TestPasswordChangeEndsSessions(t *testing.T) {
	s, h, _ := setup(t)
	w := do(h, "POST", "/api/login", `{"password":"correct horse battery"}`, map[string]string{"Origin": "https://panel.example.com"})
	c := w.Result().Cookies()[0]
	nh, _ := HashPassword("another long password")
	os.WriteFile(s.cfg.PanelPasswordFile(), []byte(nh), 0o600)
	if w := do(h, "GET", "/api/v1/status", "", map[string]string{"Cookie": c.Name + "=" + c.Value}); w.Code != 401 {
		t.Fatalf("session survived a password change: %d", w.Code)
	}
}
