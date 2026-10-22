// Package panel is the web panel's backend: a single administrator login, sessions,
// CSRF protection and a proxy to the entrypointd API. It keeps no state of its own on disk
// besides the password hash; everything else goes through entrypointd.
package panel

import (
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
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/k18k/entrypoint/gateway/internal/config"
)

const (
	cookieName   = "__Host-ep_panel"
	sessionTTL   = 12 * time.Hour
	sessionIdle  = 2 * time.Hour
	failWindow   = 15 * time.Minute
	maxFails     = 10
	passwordFile = "admin.hash"
)

type session struct {
	csrf     string
	created  time.Time
	lastSeen time.Time
	hash     string // the password hash at login: changing the password ends every session
}

// Server serves the panel.
type Server struct {
	cfg      config.Config
	ui       fs.FS
	log      *slog.Logger
	proxy    *httputil.ReverseProxy
	mu       sync.Mutex
	sessions map[string]*session
	fails    map[string][]time.Time
	now      func() time.Time
}

// New creates the panel server. ui holds the built frontend (index.html and assets).
func New(cfg config.Config, ui fs.FS, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, ui: ui, log: log, sessions: map[string]*session{}, fails: map[string][]time.Time{}, now: time.Now}
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
	return s
}

// Handler returns the panel's HTTP handler.
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/session", s.hSession)
	m.HandleFunc("POST /api/login", s.hLogin)
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

func (s *Server) passwordHash() (string, error) {
	b, err := os.ReadFile(s.cfg.PanelPasswordFile())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// current returns the request's valid session.
func (s *Server) current(r *http.Request) (string, *session) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", nil
	}
	hash, err := s.passwordHash()
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
	if now.Sub(ss.created) > sessionTTL || now.Sub(ss.lastSeen) > sessionIdle || ss.hash != hash {
		delete(s.sessions, c.Value)
		return "", nil
	}
	ss.lastSeen = now
	return c.Value, ss
}

func (s *Server) hSession(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"authenticated": false, "gateway": s.cfg.GatewayName}
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

func (s *Server) hLogin(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		reply(w, 403, map[string]string{"error": "wrong origin"})
		return
	}
	ip := clientIP(r)
	if s.limited(ip) {
		reply(w, 429, map[string]string{"error": "too many failed logins; try again in 15 minutes"})
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		reply(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	hash, err := s.passwordHash()
	if err != nil {
		s.log.Error("panel password not readable", "error", err)
		reply(w, 503, map[string]string{"error": "no panel password set; run: sudo entrypoint panel password"})
		return
	}
	ok, err := VerifyPassword(req.Password, hash)
	if err != nil || !ok {
		s.mu.Lock()
		s.fails[ip] = append(s.fails[ip], s.now())
		s.mu.Unlock()
		s.log.Warn("panel login failed", "ip", ip)
		reply(w, 401, map[string]string{"error": "wrong password"})
		return
	}
	id, ss := randomString(), &session{csrf: randomString(), created: s.now(), lastSeen: s.now(), hash: hash}
	s.mu.Lock()
	s.sessions[id] = ss
	delete(s.fails, ip)
	s.mu.Unlock()
	s.log.Info("panel login", "ip", ip)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: id, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds())})
	reply(w, 200, map[string]any{"authenticated": true, "csrf": ss.csrf, "gateway": s.cfg.GatewayName})
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
