// Package authz is the frps authorization plugin and the certificate consent endpoint.
// It answers only from the in-memory snapshot, minus revocations.
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

// Rejection codes passed to frpc. The connector recognizes them by prefix.
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

// LoginRecorder records login information (without secrets).
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
	mu          sync.Mutex // serializes snapshot swaps
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

// Swap replaces the snapshot. Revoked tokens are always subtracted.
func (s *Server) Swap(snap render.AuthzSnapshot, revoked map[string]bool, hosts []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Revocations only grow: keep those already in memory, so a snapshot built from a journal
	// read that raced with a revoke cannot re-authorize the token (Strix vuln-0008).
	rv := map[string]bool{}
	for k := range s.st.Load().revoked {
		rv[k] = true
	}
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

// Revoke subtracts a token right away, without waiting for a revision.
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

// Revision returns the revision number of the current snapshot.
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

// Handler returns the HTTP handler of the plugin and the TLS consent endpoint.
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
		writeJSON(w, reject(ReasonForbidden+" invalid request"))
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
		resp = reject(ReasonForbidden + " unsupported operation")
	}
	writeJSON(w, resp)
}

// checkToken checks the token from the client metadata. It returns the (public) ID and the entry.
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
		return reject(ReasonForbidden + " invalid login")
	}
	ip := clientIP(c.ClientAddress)
	if s.limiter.blocked(ip) {
		s.log.Warn("tunnel login rejected: failed attempts limit", "ip", ip)
		return reject(ReasonRateLimited + " too many failed logins, try again later")
	}
	st := s.st.Load()
	id, _, why := s.checkToken(st, c.Metas)
	if why == "" && c.User != "" {
		why = ReasonForbidden
	}
	if why != "" {
		s.limiter.fail(ip)
		s.log.Warn("tunnel login rejected", "ip", ip, "token_id", id, "reason", why)
		if why == ReasonRevoked {
			return reject(ReasonRevoked + " the token was revoked")
		}
		if why == ReasonForbidden {
			return reject(ReasonForbidden + " the user field is not allowed")
		}
		return reject(ReasonUnknown + " unknown token")
	}
	if s.loginRate.blocked(id) {
		s.log.Warn("tunnel login rejected: token login limit", "ip", ip, "token_id", id)
		return reject(ReasonRateLimited + " too many logins with this token, try again later")
	}
	s.loginRate.fail(id)
	if s.maxSessions > 0 && s.sessions.count(id) >= s.maxSessions {
		s.log.Warn("tunnel login rejected: token session limit", "ip", ip, "token_id", id)
		return reject(fmt.Sprintf("%s limit of %d concurrent sessions per token", ReasonSessionLimit, s.maxSessions))
	}
	var fps []string
	if v := c.Metas["ca"]; v != "" {
		fps = strings.Split(v, ",")
	}
	s.log.Info("tunnel login", "ip", ip, "token_id", id, "hostname", c.Hostname)
	if s.record != nil {
		go s.record(id, ip, printable(c.Hostname), printable(c.Version), fps)
	}
	return response{Unchange: true}
}

// NewProxy fields allowed for tcp proxies. Anything else with a non-empty value is rejected.
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
		return reject(ReasonForbidden + " invalid proxy")
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
		s.log.Warn("proxy registration rejected", "token_id", id, "proxy", c.ProxyName, "reason", why)
		return reject(why)
	}
	if s.proxyRate.blocked(id) {
		s.log.Warn("proxy registration rejected: token operation limit", "token_id", id, "proxy", c.ProxyName)
		return reject(ReasonRateLimited + " too many proxy registrations with this token")
	}
	s.proxyRate.fail(id)
	deny := func(msg string) response {
		s.log.Warn("proxy registration rejected", "token_id", id, "proxy", c.ProxyName, "reason", msg)
		return reject(ReasonForbidden + " " + msg)
	}
	if c.ProxyType != "tcp" {
		return deny("only the tcp type is allowed")
	}
	for k, v := range m {
		if !allowedProxyFields[k] && !isZero(v) {
			return deny("field not allowed: " + k)
		}
	}
	// Name: <service>.<token ID>.<replica>
	parts := strings.Split(c.ProxyName, ".")
	if len(parts) != 3 || parts[0] != tok.Service || parts[1] != id || !replicaNameRe.MatchString(parts[2]) {
		return deny(fmt.Sprintf("the proxy name must look like %s.%s.<replica>", tok.Service, id))
	}
	svc := st.snap.Services[tok.Service]
	if s.frps != nil {
		online, err := s.frps.Online(ctx)
		if err != nil {
			s.log.Error("replica limit: no frps state", "err", err)
			return reject(ReasonUnavailable + " replica state unavailable")
		}
		n := 0
		for _, p := range online {
			if strings.HasPrefix(p.Name, tok.Service+"."+id+".") {
				n++
			}
		}
		if n >= tok.MaxReplicas {
			return reject(fmt.Sprintf("%s reached the limit of %d replicas per token", ReasonLimit, tok.MaxReplicas))
		}
	}
	// The group, group key and port always come from the token's service.
	m["group"] = svc.Group()
	m["group_key"] = svc.GroupKey
	m["remote_port"] = svc.Port
	s.log.Info("replica registered", "token_id", id, "proxy", c.ProxyName, "service", svc.Name, "port", svc.Port)
	return response{Unchange: false, Content: m}
}

// ping rechecks the session against the current snapshot: revocation, rollback, removed service.
func (s *Server) ping(raw json.RawMessage) response {
	var c struct {
		User userInfo `json:"user"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return reject(ReasonForbidden)
	}
	id, _, why := s.checkToken(s.st.Load(), c.User.Metas)
	if why != "" {
		s.log.Warn("tunnel session rejected at heartbeat", "token_id", id, "reason", why)
		return reject(why + " session invalid")
	}
	s.sessions.touch(id, c.User.RunID)
	return response{Unchange: true}
}

// handleAsk consents to a certificate only for hosts with a route.
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
		s.log.Warn("certificate consent rejected: new subdomains per hour limit", "domain", d)
		http.Error(w, "limit", http.StatusTooManyRequests)
		return
	}
	if !ok {
		s.log.Info("certificate consent rejected: no route", "domain", d)
		http.Error(w, "no route", http.StatusForbidden)
		return
	}
	w.WriteHeader(200)
}

// failLimiter counts failed logins per IP address in a time window.
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
	if len(l.hits) > 100000 { // memory guard
		l.hits = map[string][]time.Time{}
	}
}

// nameLimiter limits the number of new names (on-demand subdomains) in a time window.
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
