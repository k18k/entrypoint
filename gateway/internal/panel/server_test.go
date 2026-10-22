package panel

import (
	"encoding/json"
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
	"time"

	"github.com/k18k/entrypoint/gateway/internal/config"
)

const origin = "https://panel.example.com"

type env struct {
	t    *testing.T
	s    *Server
	h    http.Handler
	seen *[]string
}

func setup(t *testing.T) *env {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.GatewayName = "tunnel.example.com"
	cfg.Panel.Name = "panel.example.com"
	cfg.DataDir = dir
	cfg.APISocket = filepath.Join(dir, "api.sock")
	os.MkdirAll(cfg.PanelDir(), 0o700)
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
	s, err := New(cfg, fstest.MapFS{"index.html": {Data: []byte("<html>panel</html>")}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return &env{t, s, s.Handler(), &seen}
}

func (e *env) do(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	r.Header.Set("Origin", origin)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

// ceremony runs begin and finish with the authenticator; it returns the finish response.
func (e *env) ceremony(kind string, begin string, k *softKey) *httptest.ResponseRecorder {
	e.t.Helper()
	w := e.do("POST", "/api/"+kind+"/begin", begin, nil)
	if w.Code != 200 {
		return w
	}
	var b struct {
		Ceremony string          `json:"ceremony"`
		Options  json.RawMessage `json:"options"`
	}
	json.Unmarshal(w.Body.Bytes(), &b)
	var body []byte
	var err error
	if kind == "register" {
		body, err = k.Create(b.Options)
	} else {
		body, err = k.Get(b.Options)
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return e.do("POST", "/api/"+kind+"/finish?ceremony="+b.Ceremony, string(body), nil)
}

func (e *env) register(k *softKey) *httptest.ResponseRecorder {
	tok, _, err := NewRegistration(e.s.cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.ceremony("register", `{"token":"`+tok+`"}`, k)
}

func cookieOf(w *httptest.ResponseRecorder) (map[string]string, string) {
	c := w.Result().Cookies()[0]
	var out struct {
		CSRF string `json:"csrf"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	return map[string]string{"Cookie": c.Name + "=" + c.Value}, out.CSRF
}

func TestPasskeyRegistrationAndLogin(t *testing.T) {
	e := setup(t)
	key := &softKey{Origin: origin}

	if w := e.do("GET", "/api/v1/status", "", nil); w.Code != 401 {
		t.Fatalf("API without a session: %d", w.Code)
	}
	// No passkey yet: sign-in cannot work.
	if w := e.ceremony("login", "", key); w.Code == 200 {
		t.Fatal("login without a registered passkey")
	}
	// A wrong or reused registration token is refused.
	if w := e.do("POST", "/api/register/begin", `{"token":"guess"}`, nil); w.Code != 403 {
		t.Fatalf("registration without a link: %d", w.Code)
	}
	w := e.register(key)
	if w.Code != 200 {
		t.Fatalf("registration: %d %s", w.Code, w.Body)
	}
	c := w.Result().Cookies()[0]
	if c.Name != "__Host-ep_panel" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie attributes: %+v", c)
	}
	keys, _ := LoadKeys(e.s.cfg)
	if len(keys.Keys) != 1 || keys.Generation != 1 {
		t.Fatalf("stored keys: %+v", keys)
	}
	if fi, _ := os.Stat(keysPath(e.s.cfg)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("passkeys file mode %v", fi.Mode())
	}

	// Sign in with the passkey, then use the API.
	w = e.ceremony("login", "", key)
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	cookie, csrf := cookieOf(w)
	if w := e.do("GET", "/api/v1/status", "", cookie); w.Code != 200 {
		t.Fatalf("API with a session: %d", w.Code)
	}
	if w := e.do("POST", "/api/v1/services", `{}`, cookie); w.Code != 403 {
		t.Fatalf("change without CSRF: %d", w.Code)
	}
	bad := map[string]string{"Cookie": cookie["Cookie"], "X-CSRF-Token": csrf, "Origin": "https://evil.example"}
	if w := e.do("POST", "/api/v1/services", `{}`, bad); w.Code != 403 {
		t.Fatalf("change from a foreign origin: %d", w.Code)
	}
	ok := map[string]string{"Cookie": cookie["Cookie"], "X-CSRF-Token": csrf}
	if w := e.do("POST", "/api/v1/services", `{}`, ok); w.Code != 200 {
		t.Fatalf("change with CSRF: %d", w.Code)
	}
	for _, s := range *e.seen {
		if strings.Contains(s, "cookie=__Host") {
			t.Fatalf("session cookie forwarded to entrypointd: %s", s)
		}
	}
	keys, _ = LoadKeys(e.s.cfg)
	if keys.Keys[0].LastUsed == nil || keys.Keys[0].Credential.Authenticator.SignCount == 0 {
		t.Fatalf("sign-in not recorded: %+v", keys.Keys[0])
	}

	// A passkey that was never registered is refused.
	if w := e.ceremony("login", "", &softKey{Origin: origin}); w.Code == 200 {
		t.Fatal("unregistered passkey accepted")
	}
	// Removing the key ends the session.
	if err := RemoveKey(e.s.cfg, keys.Keys[0].ID); err != nil {
		t.Fatal(err)
	}
	if w := e.do("GET", "/api/v1/status", "", cookie); w.Code != 401 {
		t.Fatalf("session survived removing the key: %d", w.Code)
	}
	// The SPA gets index.html for client-side routes, with a CSP.
	if w := e.do("GET", "/register", "", nil); w.Code != 200 || w.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("UI: %d", w.Code)
	}
}

func TestRegistrationLinkIsOneTimeAndExpires(t *testing.T) {
	e := setup(t)
	tok, _, _ := NewRegistration(e.s.cfg)
	if w := e.ceremony("register", `{"token":"`+tok+`"}`, &softKey{Origin: origin}); w.Code != 200 {
		t.Fatalf("first use: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/register/begin", `{"token":"`+tok+`"}`, nil); w.Code != 403 {
		t.Fatalf("second use: %d", w.Code)
	}
	tok, _, _ = NewRegistration(e.s.cfg)
	e.s.now = func() time.Time { return time.Now().Add(RegistrationTTL + time.Second) }
	if w := e.do("POST", "/api/register/begin", `{"token":"`+tok+`"}`, nil); w.Code != 403 || !strings.Contains(w.Body.String(), "expired") {
		t.Fatalf("expired link: %d %s", w.Code, w.Body)
	}
	// A key made for another site (wrong origin in clientData) is rejected.
	e.s.now = time.Now
	if w := e.register(&softKey{Origin: "https://evil.example"}); w.Code == 200 {
		t.Fatal("passkey for a foreign origin accepted")
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := setup(t)
	e.register(&softKey{Origin: origin})
	for i := 0; i < maxFails; i++ {
		e.ceremony("login", "", &softKey{Origin: origin})
	}
	if w := e.do("POST", "/api/login/begin", "", nil); w.Code != 429 {
		t.Fatalf("after %d failures: %d", maxFails, w.Code)
	}
}

// A signed-in administrator adds a second passkey (no CLI link), signs in with it, removes the
// first one; the last passkey cannot be removed.
func TestMultiplePasskeys(t *testing.T) {
	e := setup(t)
	first, second := &softKey{Origin: origin}, &softKey{Origin: origin}
	cookie, csrf := cookieOf(e.register(first))
	auth := map[string]string{"Cookie": cookie["Cookie"], "X-CSRF-Token": csrf}

	if w := e.do("POST", "/api/passkeys/begin", "", cookie); w.Code != 403 {
		t.Fatalf("add without CSRF: %d", w.Code)
	}
	w := e.do("POST", "/api/passkeys/begin", "", auth)
	var b struct {
		Ceremony string          `json:"ceremony"`
		Options  json.RawMessage `json:"options"`
	}
	json.Unmarshal(w.Body.Bytes(), &b)
	body, _ := second.Create(b.Options)
	if w := e.do("POST", "/api/register/finish?ceremony="+b.Ceremony, string(body), nil); w.Code != 200 {
		t.Fatalf("second passkey: %d %s", w.Code, w.Body)
	}
	w = e.ceremony("login", "", second)
	if w.Code != 200 {
		t.Fatalf("login with the second passkey: %d", w.Code)
	}
	cookie, csrf = cookieOf(w)
	auth = map[string]string{"Cookie": cookie["Cookie"], "X-CSRF-Token": csrf}
	var keys []struct{ ID string }
	json.Unmarshal(e.do("GET", "/api/passkeys", "", cookie).Body.Bytes(), &keys)
	if len(keys) != 2 {
		t.Fatalf("passkeys: %+v", keys)
	}
	w = e.do("DELETE", "/api/passkeys/"+keys[0].ID, "", auth)
	if w.Code != 200 {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/v1/status", "", cookie); w.Code != 401 {
		t.Fatalf("old session after removing a key: %d", w.Code)
	}
	cookie, csrf = cookieOf(w)
	if c := e.ceremony("login", "", first); c.Code == 200 {
		t.Fatal("the removed passkey still signs in")
	}
	auth = map[string]string{"Cookie": cookie["Cookie"], "X-CSRF-Token": csrf}
	if w := e.do("DELETE", "/api/passkeys/"+keys[1].ID, "", auth); w.Code != 400 {
		t.Fatalf("removing the last passkey: %d", w.Code)
	}
}
