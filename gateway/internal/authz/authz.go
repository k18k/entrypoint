// Pakiet authz to plugin autoryzacji frps i endpoint zgody na certyfikaty.
// Odpowiada wyłącznie ze snapshotu w pamięci, z odjętymi odwołaniami.
package authz

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/k18k/entrypoint/gateway/internal/config"
	"github.com/k18k/entrypoint/gateway/internal/frpsapi"
	"github.com/k18k/entrypoint/gateway/internal/render"
	"github.com/k18k/entrypoint/gateway/internal/token"
)

// Kody odrzuceń przekazywane do frpc. Konektor rozpoznaje je po prefiksie.
const (
	ReasonUnknown      = "ep:unknown_token"
	ReasonRevoked      = "ep:revoked"
	ReasonRateLimited  = "ep:rate_limited"
	ReasonForbidden    = "ep:forbidden"
	ReasonLimit        = "ep:replica_limit"
	ReasonUnavailable  = "ep:unavailable"
	ReasonSessionLimit = "ep:session_limit"
)

type state struct {
	snap    render.AuthzSnapshot
	revoked map[string]bool
	hosts   map[string]bool
}

// LoginRecorder zapisuje informacje o logowaniu (bez sekretów).
type LoginRecorder func(tokenID, clientIP, hostname, version string, caFps []string)

// Server to plugin frps.
type Server struct {
	st       atomic.Pointer[state]
	log      *slog.Logger
	frps     *frpsapi.Client
	limiter  *failLimiter // failed logins per IP
	onDemand *nameLimiter
	// Per-token limits, also for valid tokens: login rate, live sessions, proxy registrations.
	loginRate   *failLimiter
	proxyRate   *failLimiter
	sessions    *sessionTracker
	maxSessions int
	record      LoginRecorder
	mu          sync.Mutex // serializuje podmiany snapshotu
}

// New creates the plugin. sessionWindow is how long a session counts as alive after its last
// Ping (frps drops sessions without Ping after heartbeatTimeout).
func New(log *slog.Logger, frps *frpsapi.Client, l config.Limits, sessionWindow time.Duration, rec LoginRecorder) *Server {
	s := &Server{
		log:         log,
		frps:        frps,
		limiter:     newFailLimiter(l.FailedLoginsPerMinute, time.Minute),
		loginRate:   newFailLimiter(l.LoginsPerTokenPerMinute, time.Minute),
		proxyRate:   newFailLimiter(l.ProxyOpsPerTokenPerMinute, time.Minute),
		sessions:    &sessionTracker{window: sessionWindow, seen: map[string]map[string]time.Time{}},
		maxSessions: l.SessionsPerToken,
		onDemand:    newNameLimiter(l.OnDemandCertsPerHour, time.Hour),
		record:      rec,
	}
	s.st.Store(&state{snap: render.AuthzSnapshot{Services: map[string]render.AuthzService{}, Tokens: map[string]render.AuthzToken{}}, revoked: map[string]bool{}, hosts: map[string]bool{}})
	return s
}

// Swap podmienia snapshot. Odwołane tokeny są zawsze odejmowane.
func (s *Server) Swap(snap render.AuthzSnapshot, revoked map[string]bool, hosts []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rv := map[string]bool{}
	for k := range revoked {
		rv[k] = true
	}
	toks := map[string]render.AuthzToken{}
	for id, t := range snap.Tokens {
		if !rv[id] {
			toks[id] = t
		}
	}
	snap.Tokens = toks
	hs := map[string]bool{}
	for _, h := range hosts {
		hs[h] = true
	}
	s.st.Store(&state{snap: snap, revoked: rv, hosts: hs})
}

// Revoke odejmuje token od razu, bez czekania na rewizję.
func (s *Server) Revoke(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.st.Load()
	rv := map[string]bool{id: true}
	for k := range cur.revoked {
		rv[k] = true
	}
	toks := map[string]render.AuthzToken{}
	for k, t := range cur.snap.Tokens {
		if k != id {
			toks[k] = t
		}
	}
	snap := cur.snap
	snap.Tokens = toks
	s.st.Store(&state{snap: snap, revoked: rv, hosts: cur.hosts})
}

// Revision zwraca numer rewizji obowiązującego snapshotu.
func (s *Server) Revision() int64 { return s.st.Load().snap.Revision }

type request struct {
	Version string          `json:"version"`
	Op      string          `json:"op"`
	Content json.RawMessage `json:"content"`
}

type response struct {
	Reject       bool   `json:"reject"`
	RejectReason string `json:"reject_reason,omitempty"`
	Unchange     bool   `json:"unchange"`
	Content      any    `json:"content,omitempty"`
}

type userInfo struct {
	User  string            `json:"user"`
	Metas map[string]string `json:"metas"`
	RunID string            `json:"run_id"`
}

// Handler zwraca obsługę HTTP pluginu i endpointu zgody TLS.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /frps/handler", s.handlePlugin)
	mux.HandleFunc("GET /tls/ask", s.handleAsk)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func reject(reason string) response { return response{Reject: true, RejectReason: reason} }

func (s *Server) handlePlugin(w http.ResponseWriter, r *http.Request) {
	var req request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, reject(ReasonForbidden+" niepoprawne żądanie"))
		return
	}
	var resp response
	switch req.Op {
	case "Login":
		resp = s.login(req.Content)
	case "NewProxy":
		resp = s.newProxy(r.Context(), req.Content)
	case "Ping":
		resp = s.ping(req.Content)
	default:
		resp = reject(ReasonForbidden + " nieobsługiwana operacja")
	}
	writeJSON(w, resp)
}

// checkToken sprawdza token z metadanych klienta. Zwraca identyfikator (jawny) i wpis.
func (s *Server) checkToken(st *state, metas map[string]string) (string, render.AuthzToken, string) {
	full := metas["token"]
	id, secret, err := token.Parse(full)
	if err != nil {
		return "", render.AuthzToken{}, ReasonUnknown
	}
	if st.revoked[id] {
		return id, render.AuthzToken{}, ReasonRevoked
	}
	t, ok := st.snap.Tokens[id]
	if !ok || !token.Verify(secret, t.Hash) {
		return id, render.AuthzToken{}, ReasonUnknown
	}
	if _, ok := st.snap.Services[t.Service]; !ok {
		return id, render.AuthzToken{}, ReasonUnknown
	}
	return id, t, ""
}

func clientIP(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

func (s *Server) login(raw json.RawMessage) response {
	var c struct {
		User          string            `json:"user"`
		Version       string            `json:"version"`
		Hostname      string            `json:"hostname"`
		Metas         map[string]string `json:"metas"`
		ClientAddress string            `json:"client_address"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return reject(ReasonForbidden + " niepoprawne logowanie")
	}
	ip := clientIP(c.ClientAddress)
	if s.limiter.blocked(ip) {
		s.log.Warn("logowanie tunelu odrzucone: limit nieudanych prób", "ip", ip)
		return reject(ReasonRateLimited + " zbyt wiele nieudanych logowań, spróbuj później")
	}
	st := s.st.Load()
	id, _, why := s.checkToken(st, c.Metas)
	if why == "" && c.User != "" {
		why = ReasonForbidden
	}
	if why != "" {
		s.limiter.fail(ip)
		s.log.Warn("logowanie tunelu odrzucone", "ip", ip, "token_id", id, "powod", why)
		if why == ReasonRevoked {
			return reject(ReasonRevoked + " token został odwołany")
		}
		if why == ReasonForbidden {
			return reject(ReasonForbidden + " pole user nie jest dozwolone")
		}
		return reject(ReasonUnknown + " nieznany token")
	}
	if s.loginRate.blocked(id) {
		s.log.Warn("logowanie tunelu odrzucone: limit logowań tokenu", "ip", ip, "token_id", id)
		return reject(ReasonRateLimited + " zbyt wiele logowań tego tokenu, spróbuj później")
	}
	s.loginRate.fail(id)
	if s.maxSessions > 0 && s.sessions.count(id) >= s.maxSessions {
		s.log.Warn("logowanie tunelu odrzucone: limit sesji tokenu", "ip", ip, "token_id", id)
		return reject(fmt.Sprintf("%s limit %d jednoczesnych sesji tokenu", ReasonSessionLimit, s.maxSessions))
	}
	var fps []string
	if v := c.Metas["ca"]; v != "" {
		fps = strings.Split(v, ",")
	}
	s.log.Info("logowanie tunelu", "ip", ip, "token_id", id, "hostname", c.Hostname)
	if s.record != nil {
		go s.record(id, ip, printable(c.Hostname), printable(c.Version), fps)
	}
	return response{Unchange: true}
}

// Pola NewProxy dozwolone dla proxy tcp. Wszystko inne z niepustą wartością jest odrzucane.
var allowedProxyFields = map[string]bool{
	"user": true, "proxy_name": true, "proxy_type": true,
	"use_encryption": true, "use_compression": true,
	"bandwidth_limit": true, "bandwidth_limit_mode": true,
	"group": true, "group_key": true, "remote_port": true,
	"metas": true, "annotations": true,
}

var replicaNameRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

func isZero(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		return x == ""
	case float64:
		return x == 0
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

func (s *Server) newProxy(ctx context.Context, raw json.RawMessage) response {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return reject(ReasonForbidden + " niepoprawne proxy")
	}
	var c struct {
		User      userInfo `json:"user"`
		ProxyName string   `json:"proxy_name"`
		ProxyType string   `json:"proxy_type"`
	}
	_ = json.Unmarshal(raw, &c)
	st := s.st.Load()
	id, tok, why := s.checkToken(st, c.User.Metas)
	if why != "" {
		s.log.Warn("rejestracja proxy odrzucona", "token_id", id, "proxy", c.ProxyName, "powod", why)
		return reject(why)
	}
	if s.proxyRate.blocked(id) {
		s.log.Warn("rejestracja proxy odrzucona: limit operacji tokenu", "token_id", id, "proxy", c.ProxyName)
		return reject(ReasonRateLimited + " zbyt wiele rejestracji proxy tego tokenu")
	}
	s.proxyRate.fail(id)
	deny := func(msg string) response {
		s.log.Warn("rejestracja proxy odrzucona", "token_id", id, "proxy", c.ProxyName, "powod", msg)
		return reject(ReasonForbidden + " " + msg)
	}
	if c.ProxyType != "tcp" {
		return deny("dozwolony jest tylko typ tcp")
	}
	for k, v := range m {
		if !allowedProxyFields[k] && !isZero(v) {
			return deny("niedozwolone pole " + k)
		}
	}
	// Nazwa: <serwis>.<id tokenu>.<replika>
	parts := strings.Split(c.ProxyName, ".")
	if len(parts) != 3 || parts[0] != tok.Service || parts[1] != id || !replicaNameRe.MatchString(parts[2]) {
		return deny(fmt.Sprintf("nazwa proxy musi mieć postać %s.%s.<replika>", tok.Service, id))
	}
	svc := st.snap.Services[tok.Service]
	if s.frps != nil {
		online, err := s.frps.Online(ctx)
		if err != nil {
			s.log.Error("limit replik: brak stanu frps", "err", err)
			return reject(ReasonUnavailable + " stan replik niedostępny")
		}
		n := 0
		for _, p := range online {
			if strings.HasPrefix(p.Name, tok.Service+"."+id+".") {
				n++
			}
		}
		if n >= tok.MaxReplicas {
			return reject(fmt.Sprintf("%s osiągnięto limit %d replik dla tokenu", ReasonLimit, tok.MaxReplicas))
		}
	}
	// Grupa, klucz grupy i port zawsze z serwisu tokenu.
	m["group"] = svc.Group()
	m["group_key"] = svc.GroupKey
	m["remote_port"] = svc.Port
	s.log.Info("rejestracja repliki", "token_id", id, "proxy", c.ProxyName, "serwis", svc.Name, "port", svc.Port)
	return response{Unchange: false, Content: m}
}

// ping ponownie sprawdza sesję wobec bieżącego snapshotu: odwołanie, rollback, usunięcie serwisu.
func (s *Server) ping(raw json.RawMessage) response {
	var c struct {
		User userInfo `json:"user"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return reject(ReasonForbidden)
	}
	id, _, why := s.checkToken(s.st.Load(), c.User.Metas)
	if why != "" {
		s.log.Warn("sesja tunelu odrzucona przy heartbeat", "token_id", id, "powod", why)
		return reject(why + " sesja nieważna")
	}
	s.sessions.touch(id, c.User.RunID)
	return response{Unchange: true}
}

// handleAsk zgadza się na certyfikat tylko dla hostów z trasą.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	d := strings.ToLower(strings.TrimSuffix(r.URL.Query().Get("domain"), "."))
	st := s.st.Load()
	ok := st.hosts[d]
	viaWildcard := false
	if !ok {
		if i := strings.IndexByte(d, '.'); i > 0 && st.hosts["*"+d[i:]] {
			ok, viaWildcard = true, true
		}
	}
	if ok && viaWildcard && !s.onDemand.allow(d) {
		s.log.Warn("zgoda na certyfikat odrzucona: limit nowych subdomen na godzinę", "domain", d)
		http.Error(w, "limit", http.StatusTooManyRequests)
		return
	}
	if !ok {
		s.log.Info("zgoda na certyfikat odrzucona: brak trasy", "domain", d)
		http.Error(w, "brak trasy", http.StatusForbidden)
		return
	}
	w.WriteHeader(200)
}

// failLimiter liczy nieudane logowania per adres IP w oknie czasowym.
type failLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
}

func newFailLimiter(limit int, window time.Duration) *failLimiter {
	return &failLimiter{limit: limit, window: window, hits: map[string][]time.Time{}}
}

func (l *failLimiter) prune(ip string, now time.Time) []time.Time {
	h := l.hits[ip]
	i := 0
	for i < len(h) && now.Sub(h[i]) > l.window {
		i++
	}
	h = h[i:]
	if len(h) == 0 {
		delete(l.hits, ip)
	} else {
		l.hits[ip] = h
	}
	return h
}

func (l *failLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit > 0 && len(l.prune(ip, time.Now())) >= l.limit
}

func (l *failLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.hits[ip] = append(l.prune(ip, now), now)
	if len(l.hits) > 100000 { // ochrona pamięci
		l.hits = map[string][]time.Time{}
	}
}

// nameLimiter ogranicza liczbę nowych nazw (subdomen w locie) w oknie czasowym.
type nameLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	seen   map[string]time.Time
}

func newNameLimiter(limit int, window time.Duration) *nameLimiter {
	return &nameLimiter{limit: limit, window: window, seen: map[string]time.Time{}}
}

func (l *nameLimiter) allow(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if _, ok := l.seen[name]; ok {
		return true
	}
	n := 0
	for k, t := range l.seen {
		if now.Sub(t) > l.window {
			delete(l.seen, k)
			continue
		}
		n++
	}
	if l.limit > 0 && n >= l.limit {
		return false
	}
	l.seen[name] = now
	return true
}

// sessionTracker counts live sessions per token: a session (frps run id) is alive while it
// keeps sending Ping. shortcut: a session counts only after its first Ping (~10 s), so a burst
// of logins is bounded by the login rate limit, not this one.
type sessionTracker struct {
	mu     sync.Mutex
	window time.Duration
	seen   map[string]map[string]time.Time
}

func (t *sessionTracker) touch(token, run string) {
	if run == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen[token] == nil {
		t.seen[token] = map[string]time.Time{}
	}
	t.seen[token][run] = time.Now()
}

func (t *sessionTracker) count(token string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for run, at := range t.seen[token] {
		if time.Since(at) > t.window {
			delete(t.seen[token], run)
			continue
		}
		n++
	}
	return n
}

// printable keeps client-reported strings safe to store and print in an admin's terminal:
// control characters (including ANSI escapes) are dropped and the length is capped.
func printable(v string) string {
	out := make([]rune, 0, len(v))
	for _, r := range v {
		if unicode.IsPrint(r) {
			out = append(out, r)
		}
		if len(out) == 64 {
			break
		}
	}
	return string(out)
}
