// Package panel is the web panel's backend: a single administrator who signs in only with
// passkeys (WebAuthn), sessions, CSRF protection and a proxy to the entrypointd API. Its only
// state on disk are the passkeys; everything else goes through entrypointd.
package panel

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/k18k/entrypoint/gateway/internal/config"
)

const (
	cookieName    = "__Host-ep_panel"
	sessionTTL    = 12 * time.Hour
	sessionIdle   = 2 * time.Hour
	failWindow    = 15 * time.Minute
	maxFails      = 10
	ceremonyTTL   = 3 * time.Minute
	maxCeremonies = 256
)

type session struct {
	csrf       string
	created    time.Time
	lastSeen   time.Time
	generation int // of the passkeys at login: adding or removing a key ends every session
}

// ceremony is a WebAuthn registration or login in progress.
type ceremony struct {
	data    webauthn.SessionData
	login   bool
	expires time.Time
}

// Server serves the panel.
type Server struct {
	cfg      config.Config
	ui       fs.FS
	log      *slog.Logger
	proxy    *httputil.ReverseProxy
	wa       *webauthn.WebAuthn
	mu       sync.Mutex
	sessions map[string]*session
	fails    map[string][]time.Time
	cers     map[string]*ceremony
	keysMu   sync.Mutex // serializes changes to the passkeys file
	now      func() time.Time
}

// New creates the panel server. ui holds the built frontend (index.html and assets).
func New(cfg config.Config, ui fs.FS, log *slog.Logger) (*Server, error) {
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          cfg.Panel.Name,
		RPDisplayName: "Entrypoint " + cfg.GatewayName,
		RPOrigins:     []string{"https://" + cfg.Panel.Name},
	})
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, ui: ui, log: log, wa: wa, sessions: map[string]*session{}, fails: map[string][]time.Time{}, cers: map[string]*ceremony{}, now: time.Now}
	sock := cfg.APISocket
	s.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme, r.Out.URL.Host = "http", "entrypointd"
			r.Out.URL.Path = strings.TrimPrefix(r.In.URL.Path, "/api")
			r.Out.URL.RawPath = ""
			r.Out.Header.Del("Cookie")
			r.Out.Header.Del("X-Csrf-Token")
		},
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			reply(w, 502, map[string]string{"error": "entrypointd unavailable"})
		},
	}
	return s, nil
}

// Handler returns the panel's HTTP handler.
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/session", s.hSession)
	m.HandleFunc("POST /api/register/begin", s.hRegisterBegin)
	m.HandleFunc("GET /api/passkeys", s.hPasskeys)
	m.HandleFunc("POST /api/passkeys/begin", s.hPasskeyAdd)
	m.HandleFunc("DELETE /api/passkeys/{id}", s.hPasskeyRemove)
	m.HandleFunc("POST /api/register/finish", s.hRegisterFinish)
	m.HandleFunc("POST /api/login/begin", s.hLoginBegin)
	m.HandleFunc("POST /api/login/finish", s.hLoginFinish)
	m.HandleFunc("POST /api/logout", s.hLogout)
	m.HandleFunc("/api/v1/", s.hAPI)
	m.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { reply(w, 404, map[string]string{"error": "not found"}) })
	m.HandleFunc("/", s.hUI)
	return secureHeaders(m)
}

func secureHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Strict-Transport-Security", "max-age=31536000")
		h.ServeHTTP(w, r)
	})
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// clientIP is the address Caddy saw. The panel listens on loopback only, and Caddy replaces
// X-Forwarded-For from untrusted clients with the real address.
func clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if a, err := netip.ParseAddr(host); err == nil && a.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			return strings.TrimSpace(first)
		}
	}
	return host
}

// sameOrigin requires the Origin of the panel's own site on state-changing requests.
func (s *Server) sameOrigin(r *http.Request) bool {
	return r.Header.Get("Origin") == "https://"+s.cfg.Panel.Name
}

func randomString() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// current returns the request's valid session.
func (s *Server) current(r *http.Request) (string, *session) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", nil
	}
	keys, err := LoadKeys(s.cfg)
	if err != nil {
		return "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[c.Value]
	now := s.now()
	if ss == nil {
		return "", nil
	}
	if now.Sub(ss.created) > sessionTTL || now.Sub(ss.lastSeen) > sessionIdle || ss.generation != keys.Generation || len(keys.Keys) == 0 {
		delete(s.sessions, c.Value)
		return "", nil
	}
	ss.lastSeen = now
	return c.Value, ss
}

func (s *Server) hSession(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"authenticated": false, "gateway": s.cfg.GatewayName}
	if keys, err := LoadKeys(s.cfg); err == nil {
		out["registered"] = len(keys.Keys) > 0
	}
	if _, ss := s.current(r); ss != nil {
		out["authenticated"] = true
		out["csrf"] = ss.csrf
	}
	reply(w, 200, out)
}

// limited reports whether the address has used up its failed logins.
func (s *Server) limited(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cut := s.now().Add(-failWindow)
	var keep []time.Time
	for _, t := range s.fails[ip] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(s.fails, ip)
	} else {
		s.fails[ip] = keep
	}
	return len(keep) >= maxFails
}

func (s *Server) failed(ip string) {
	s.mu.Lock()
	s.fails[ip] = append(s.fails[ip], s.now())
	s.mu.Unlock()
}

// startCeremony keeps the WebAuthn session data server-side under a random ID.
func (s *Server) startCeremony(data *webauthn.SessionData, login bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for id, c := range s.cers {
		if now.After(c.expires) {
			delete(s.cers, id)
		}
	}
	if len(s.cers) >= maxCeremonies {
		return "", errors.New("too many sign-ins in progress; try again in a few minutes")
	}
	id := randomString()
	s.cers[id] = &ceremony{data: *data, login: login, expires: now.Add(ceremonyTTL)}
	return id, nil
}

// takeCeremony returns a ceremony once.
func (s *Server) takeCeremony(id string, login bool) (*ceremony, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cers[id]
	delete(s.cers, id)
	if c == nil || c.login != login || s.now().After(c.expires) {
		return nil, false
	}
	return c, true
}

// startSession signs the browser in.
func (s *Server) startSession(w http.ResponseWriter, generation int) {
	id, ss := randomString(), &session{csrf: randomString(), created: s.now(), lastSeen: s.now(), generation: generation}
	s.mu.Lock()
	s.sessions[id] = ss
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: id, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds())})
	reply(w, 200, map[string]any{"authenticated": true, "registered": true, "csrf": ss.csrf, "gateway": s.cfg.GatewayName})
}

var userVerification = protocol.AuthenticatorSelection{
	ResidentKey:      protocol.ResidentKeyRequirementRequired,
	UserVerification: protocol.VerificationRequired,
}

// hRegisterBegin starts adding a passkey with a one-time link from `entrypoint panel register`.
func (s *Server) hRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		reply(w, 403, map[string]string{"error": "wrong origin"})
		return
	}
	ip := clientIP(r)
	if s.limited(ip) {
		reply(w, 429, map[string]string{"error": "too many failed attempts; try again in 15 minutes"})
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		reply(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	if err := consumeRegistration(s.cfg, req.Token, s.now()); err != nil {
		s.failed(ip)
		s.log.Warn("panel passkey registration refused", "ip", ip, "error", err)
		reply(w, 403, map[string]string{"error": err.Error()})
		return
	}
	s.beginRegistration(w)
}

// beginRegistration starts adding a passkey (keysMu held); hRegisterFinish completes it.
func (s *Server) beginRegistration(w http.ResponseWriter) {
	keys, err := LoadKeys(s.cfg)
	if err != nil {
		reply(w, 500, map[string]string{"error": "cannot read passkeys"})
		return
	}
	if len(keys.UserID) == 0 {
		keys.UserID = make([]byte, 32)
		if _, err := rand.Read(keys.UserID); err != nil {
			reply(w, 500, map[string]string{"error": err.Error()})
			return
		}
		if err := SaveKeys(s.cfg, keys); err != nil {
			reply(w, 500, map[string]string{"error": "cannot save passkeys"})
			return
		}
	}
	var exclude []protocol.CredentialDescriptor
	for _, k := range keys.Keys {
		exclude = append(exclude, k.Credential.Descriptor())
	}
	opts, data, err := s.wa.BeginRegistration(keys.admin(),
		webauthn.WithAuthenticatorSelection(userVerification),
		webauthn.WithExclusions(exclude),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation))
	if err != nil {
		reply(w, 500, map[string]string{"error": err.Error()})
		return
	}
	id, err := s.startCeremony(data, false)
	if err != nil {
		reply(w, 503, map[string]string{"error": err.Error()})
		return
	}
	reply(w, 200, map[string]any{"ceremony": id, "options": opts.Response})
}

func (s *Server) hRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		reply(w, 403, map[string]string{"error": "wrong origin"})
		return
	}
	c, ok := s.takeCeremony(r.URL.Query().Get("ceremony"), false)
	if !ok {
		reply(w, 400, map[string]string{"error": "the registration expired; run sudo entrypoint panel register again"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	keys, err := LoadKeys(s.cfg)
	if err != nil {
		reply(w, 500, map[string]string{"error": "cannot read passkeys"})
		return
	}
	cred, err := s.wa.FinishRegistration(keys.admin(), c.data, r)
	if err != nil {
		s.failed(clientIP(r))
		reply(w, 400, map[string]string{"error": "the passkey was not accepted: " + protocolMessage(err)})
		return
	}
	name := "passkey"
	if ua := r.Header.Get("User-Agent"); ua != "" {
		name = "added from " + browserName(ua)
	}
	keys.Keys = append(keys.Keys, Key{ID: shortID(cred.ID), Name: name, CreatedAt: s.now().UTC(), Credential: *cred})
	keys.Generation++
	if err := SaveKeys(s.cfg, keys); err != nil {
		reply(w, 500, map[string]string{"error": "cannot save passkeys"})
		return
	}
	s.log.Info("panel passkey registered", "key", shortID(cred.ID), "ip", clientIP(r))
	s.startSession(w, keys.Generation)
}

func (s *Server) hLoginBegin(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		reply(w, 403, map[string]string{"error": "wrong origin"})
		return
	}
	if s.limited(clientIP(r)) {
		reply(w, 429, map[string]string{"error": "too many failed sign-ins; try again in 15 minutes"})
		return
	}
	opts, data, err := s.wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		reply(w, 500, map[string]string{"error": err.Error()})
		return
	}
	id, err := s.startCeremony(data, true)
	if err != nil {
		reply(w, 503, map[string]string{"error": err.Error()})
		return
	}
	reply(w, 200, map[string]any{"ceremony": id, "options": opts.Response})
}

func (s *Server) hLoginFinish(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		reply(w, 403, map[string]string{"error": "wrong origin"})
		return
	}
	ip := clientIP(r)
	if s.limited(ip) {
		reply(w, 429, map[string]string{"error": "too many failed sign-ins; try again in 15 minutes"})
		return
	}
	c, ok := s.takeCeremony(r.URL.Query().Get("ceremony"), true)
	if !ok {
		reply(w, 400, map[string]string{"error": "the sign-in expired; try again"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	keys, err := LoadKeys(s.cfg)
	if err != nil {
		reply(w, 500, map[string]string{"error": "cannot read passkeys"})
		return
	}
	adm := keys.admin()
	handler := func(rawID, userHandle []byte) (webauthn.User, error) {
		if len(keys.UserID) == 0 || subtle.ConstantTimeCompare(userHandle, keys.UserID) != 1 {
			return nil, errors.New("unknown passkey")
		}
		return adm, nil
	}
	_, cred, err := s.wa.FinishPasskeyLogin(handler, c.data, r)
	if err != nil {
		s.failed(ip)
		s.log.Warn("panel sign-in failed", "ip", ip, "error", protocolMessage(err))
		reply(w, 401, map[string]string{"error": "this passkey is not registered for the panel"})
		return
	}
	now := s.now().UTC()
	for i := range keys.Keys {
		if bytes.Equal(keys.Keys[i].Credential.ID, cred.ID) {
			keys.Keys[i].Credential.Authenticator = cred.Authenticator
			keys.Keys[i].LastUsed = &now
		}
	}
	if err := SaveKeys(s.cfg, keys); err != nil {
		reply(w, 500, map[string]string{"error": "cannot save passkeys"})
		return
	}
	if cred.Authenticator.CloneWarning {
		s.log.Warn("panel passkey signature counter went back; the key may be cloned", "key", shortID(cred.ID))
	}
	s.mu.Lock()
	delete(s.fails, ip)
	s.mu.Unlock()
	s.log.Info("panel sign-in", "key", shortID(cred.ID), "ip", ip)
	s.startSession(w, keys.Generation)
}

// protocolMessage gives the detail of a WebAuthn error for logs and the registering administrator.
func protocolMessage(err error) string {
	var pe *protocol.Error
	if errors.As(err, &pe) && pe.DevInfo != "" {
		return pe.Details + ": " + pe.DevInfo
	}
	return err.Error()
}

func browserName(ua string) string {
	for _, b := range []string{"Edg/", "Firefox/", "Chrome/", "Safari/"} {
		if strings.Contains(ua, b) {
			name := strings.TrimSuffix(b, "/")
			if name == "Edg" {
				name = "Edge"
			}
			return name
		}
	}
	return "a browser"
}

func (s *Server) hLogout(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		reply(w, 403, map[string]string{"error": "wrong origin"})
		return
	}
	if id, ss := s.current(r); ss != nil && s.csrfOK(r, ss) {
		s.mu.Lock()
		delete(s.sessions, id)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	reply(w, 200, map[string]bool{"authenticated": false})
}

func (s *Server) csrfOK(r *http.Request, ss *session) bool {
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(ss.csrf)) == 1
}

// hAPI forwards authenticated requests to entrypointd; changes need the CSRF token and the panel's Origin.
func (s *Server) hAPI(w http.ResponseWriter, r *http.Request) {
	_, ss := s.current(r)
	if ss == nil {
		reply(w, 401, map[string]string{"error": "not logged in"})
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if !s.sameOrigin(r) || !s.csrfOK(r, ss) {
			reply(w, 403, map[string]string{"error": "missing or wrong CSRF token"})
			return
		}
	}
	if u, err := url.PathUnescape(r.URL.Path); err != nil || strings.Contains(u, "..") {
		reply(w, 400, map[string]string{"error": "invalid path"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.proxy.ServeHTTP(w, r)
}

// hUI serves the frontend; unknown paths get index.html (client-side routing).
func (s *Server) hUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", 405)
		return
	}
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p != "" {
		if f, err := fs.Stat(s.ui, p); err == nil && !f.IsDir() {
			if strings.HasPrefix(p, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeFileFS(w, r, s.ui, p)
			return
		}
	}
	idx, err := fs.ReadFile(s.ui, "index.html")
	if errors.Is(err, fs.ErrNotExist) {
		http.Error(w, "the panel frontend is not built (make panel)", 503)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(idx)
}

// signedIn checks the session and, for changes, the CSRF token and Origin.
func (s *Server) signedIn(w http.ResponseWriter, r *http.Request) bool {
	_, ss := s.current(r)
	if ss == nil {
		reply(w, 401, map[string]string{"error": "not logged in"})
		return false
	}
	if r.Method != http.MethodGet && (!s.sameOrigin(r) || !s.csrfOK(r, ss)) {
		reply(w, 403, map[string]string{"error": "missing or wrong CSRF token"})
		return false
	}
	return true
}

func (s *Server) hPasskeys(w http.ResponseWriter, r *http.Request) {
	if !s.signedIn(w, r) {
		return
	}
	keys, err := LoadKeys(s.cfg)
	if err != nil {
		reply(w, 500, map[string]string{"error": "cannot read passkeys"})
		return
	}
	type row struct {
		ID        string     `json:"id"`
		Name      string     `json:"name"`
		CreatedAt time.Time  `json:"createdAt"`
		LastUsed  *time.Time `json:"lastUsed,omitempty"`
	}
	out := []row{}
	for _, k := range keys.Keys {
		out = append(out, row{k.ID, k.Name, k.CreatedAt, k.LastUsed})
	}
	reply(w, 200, out)
}

// hPasskeyAdd lets a signed-in administrator add another passkey (e.g. a backup security key)
// without a CLI link; it finishes through /api/register/finish.
func (s *Server) hPasskeyAdd(w http.ResponseWriter, r *http.Request) {
	if !s.signedIn(w, r) {
		return
	}
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	s.beginRegistration(w)
}

// hPasskeyRemove revokes a passkey. The last one stays, so the panel cannot lock itself out.
// Every session ends; the caller gets a new one.
func (s *Server) hPasskeyRemove(w http.ResponseWriter, r *http.Request) {
	if !s.signedIn(w, r) {
		return
	}
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	keys, err := LoadKeys(s.cfg)
	if err != nil {
		reply(w, 500, map[string]string{"error": "cannot read passkeys"})
		return
	}
	if len(keys.Keys) <= 1 {
		reply(w, 400, map[string]string{"error": "this is the only passkey; add another one first"})
		return
	}
	if err := RemoveKey(s.cfg, r.PathValue("id")); err != nil {
		reply(w, 404, map[string]string{"error": err.Error()})
		return
	}
	keys, _ = LoadKeys(s.cfg)
	s.log.Info("panel passkey removed", "key", r.PathValue("id"), "ip", clientIP(r))
	s.startSession(w, keys.Generation)
}
