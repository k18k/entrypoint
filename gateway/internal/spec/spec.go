// Package spec defines the configuration document stored in revisions
// and its validation.
package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultMaxBodyBytes          = 50 << 20
	DefaultResponseHeaderTimeout = 60
	DefaultMaxReplicas           = 10
)

// Spec is the full desired configuration of a revision.
type Spec struct {
	Services []Service `json:"services"`
	Routes   []Route   `json:"routes"`
	Tokens   []Token   `json:"tokens"`
}

// Service is a tunnel service with a fixed port.
type Service struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Port   int    `json:"port"`
	Health string `json:"health,omitempty"`
}

// Route sends a host+prefix pair to a service or to an IP:port upstream.
type Route struct {
	ID                    string `json:"id"`
	Host                  string `json:"host"`
	Prefix                string `json:"prefix"`
	Service               string `json:"service,omitempty"`
	Upstream              string `json:"upstream,omitempty"`
	StripPrefix           bool   `json:"stripPrefix,omitempty"`
	MaxBodyBytes          int64  `json:"maxBodyBytes,omitempty"`
	ResponseHeaderTimeout int    `json:"responseHeaderTimeout,omitempty"` // seconds
	ACME                  string `json:"acme,omitempty"`                  // "" (production) or "staging"
	// Wildcard routes only: concrete names allowed to get a certificate (obtained up front),
	// and the explicit opt-in to on-demand certificates for any subdomain (rate-limited).
	TLSNames    []string `json:"tlsNames,omitempty"`
	TLSOnDemand bool     `json:"tlsOnDemand,omitempty"`
}

// Token is a service token entry. Only the secret's hash is stored.
type Token struct {
	ID          string    `json:"id"`
	ServiceID   int64     `json:"serviceId"`
	Hash        string    `json:"hash"`
	Label       string    `json:"label,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	MaxReplicas int       `json:"maxReplicas,omitempty"`
}

// Scope returns the token's permission.
func (t Token) Scope(s *Spec) string {
	if svc := s.ServiceByID(t.ServiceID); svc != nil {
		return "replica:connect " + svc.Name
	}
	return ""
}

func (r Route) EffectiveMaxBody() int64 {
	if r.MaxBodyBytes > 0 {
		return r.MaxBodyBytes
	}
	return DefaultMaxBodyBytes
}

func (r Route) EffectiveHeaderTimeout() int {
	if r.ResponseHeaderTimeout > 0 {
		return r.ResponseHeaderTimeout
	}
	return DefaultResponseHeaderTimeout
}

func (t Token) EffectiveMaxReplicas() int {
	if t.MaxReplicas > 0 {
		return t.MaxReplicas
	}
	return DefaultMaxReplicas
}

// Target describes the route's target in readable form.
func (r Route) Target() string {
	if r.Service != "" {
		return "service " + r.Service
	}
	return r.Upstream
}

// Clone makes a deep copy.
func (s *Spec) Clone() *Spec {
	b, _ := json.Marshal(s)
	var out Spec
	_ = json.Unmarshal(b, &out)
	return &out
}

func (s *Spec) ServiceByName(name string) *Service {
	for i := range s.Services {
		if s.Services[i].Name == name {
			return &s.Services[i]
		}
	}
	return nil
}

func (s *Spec) ServiceByID(id int64) *Service {
	for i := range s.Services {
		if s.Services[i].ID == id {
			return &s.Services[i]
		}
	}
	return nil
}

func (s *Spec) RouteByID(id string) *Route {
	for i := range s.Routes {
		if s.Routes[i].ID == id {
			return &s.Routes[i]
		}
	}
	return nil
}

func (s *Spec) TokenByID(id string) *Token {
	for i := range s.Tokens {
		if s.Tokens[i].ID == id {
			return &s.Tokens[i]
		}
	}
	return nil
}

// Normalize sorts the elements so that revisions are comparable.
func (s *Spec) Normalize() {
	if s.Services == nil {
		s.Services = []Service{}
	}
	if s.Routes == nil {
		s.Routes = []Route{}
	}
	if s.Tokens == nil {
		s.Tokens = []Token{}
	}
	sort.Slice(s.Services, func(i, j int) bool { return s.Services[i].Name < s.Services[j].Name })
	sort.Slice(s.Routes, func(i, j int) bool {
		a, b := s.Routes[i], s.Routes[j]
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		return a.Prefix < b.Prefix
	})
	sort.Slice(s.Tokens, func(i, j int) bool { return s.Tokens[i].ID < s.Tokens[j].ID })
}

var (
	serviceNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	labelRe       = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	prefixRe      = regexp.MustCompile(`^(/[A-Za-z0-9._~!$&'()+,;=:@-]+)+$`)
	healthRe      = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()+,;=:@/?-]*$`)
	routeIDRe     = regexp.MustCompile(`^r[0-9a-f]{6}$`)
)

// ValidServiceName checks a service name.
func ValidServiceName(n string) error {
	if !serviceNameRe.MatchString(n) {
		return fmt.Errorf("invalid service name %q: lowercase letters, digits and '-', starting with a letter, up to 31 characters", n)
	}
	return nil
}

// NormalizeHost lowercases a host name and checks it.
func NormalizeHost(h string) (string, error) {
	h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	if h == "" {
		return "", errors.New("empty host")
	}
	if len(h) > 253 {
		return "", fmt.Errorf("host %q is too long", h)
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("host %q must have at least two labels", h)
	}
	for i, l := range labels {
		if l == "*" && i == 0 {
			if len(labels) < 3 {
				return "", fmt.Errorf("wildcard %q covers a whole top-level domain", h)
			}
			continue
		}
		if !labelRe.MatchString(l) {
			return "", fmt.Errorf("invalid host %q (a wildcard is allowed only as the first label)", h)
		}
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return "", fmt.Errorf("host %q is an IP address; routes need a DNS name", h)
	}
	return h, nil
}

// NormalizePrefix brings a prefix to canonical form: it starts with '/' and has no trailing '/'.
func NormalizePrefix(p string) (string, error) {
	if p == "" || p == "/" {
		return "/", nil
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = strings.TrimRight(p, "/")
	if !prefixRe.MatchString(p) {
		return "", fmt.Errorf("invalid prefix %q", p)
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("prefix %q contains the segment %q", p, seg)
		}
	}
	return p, nil
}

// ValidUpstream checks an IP:port address.
func ValidUpstream(u string) error {
	host, port, err := net.SplitHostPort(u)
	if err != nil {
		return fmt.Errorf("upstream %q: expected IP:port", u)
	}
	if _, err := netip.ParseAddr(host); err != nil {
		return fmt.Errorf("upstream %q: %q is not an IP address", u, host)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("upstream %q: invalid port", u)
	}
	return nil
}

// ValidHealthPath checks a health check path.
func ValidHealthPath(p string) error {
	if p == "" {
		return nil
	}
	if !healthRe.MatchString(p) {
		return fmt.Errorf("invalid health check path %q", p)
	}
	return nil
}

// Validate checks the whole configuration for consistency: schema, route conflicts, existing services.
func (s *Spec) Validate(poolStart, poolEnd int) error {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	names := map[string]bool{}
	ids := map[int64]bool{}
	ports := map[int]bool{}
	for _, svc := range s.Services {
		if err := ValidServiceName(svc.Name); err != nil {
			add("%v", err)
		}
		if names[svc.Name] {
			add("service %q appears twice", svc.Name)
		}
		names[svc.Name] = true
		if svc.ID <= 0 || ids[svc.ID] {
			add("service %q: invalid ID", svc.Name)
		}
		ids[svc.ID] = true
		if svc.Port < poolStart || svc.Port > poolEnd {
			add("service %q: port %d outside the pool %d-%d", svc.Name, svc.Port, poolStart, poolEnd)
		}
		if ports[svc.Port] {
			add("port %d assigned to two services", svc.Port)
		}
		ports[svc.Port] = true
		if err := ValidHealthPath(svc.Health); err != nil {
			add("service %q: %v", svc.Name, err)
		}
	}

	type key struct{ host, prefix string }
	seen := map[key]string{}
	routeIDs := map[string]bool{}
	hostACME := map[string]string{}
	for _, r := range s.Routes {
		if !routeIDRe.MatchString(r.ID) || routeIDs[r.ID] {
			add("route %q: invalid or duplicate ID", r.ID)
		}
		routeIDs[r.ID] = true
		h, err := NormalizeHost(r.Host)
		if err != nil {
			add("route %s: %v", r.ID, err)
		} else if h != r.Host {
			add("route %s: host is not normalized", r.ID)
		}
		p, err := NormalizePrefix(r.Prefix)
		if err != nil {
			add("route %s: %v", r.ID, err)
		} else if p != r.Prefix {
			add("route %s: prefix is not normalized", r.ID)
		}
		k := key{r.Host, strings.ToLower(r.Prefix)} // Caddy matches paths case-insensitively
		if other, ok := seen[k]; ok {
			add("route conflict: %s%s is already taken by route %s", r.Host, r.Prefix, other)
		}
		seen[k] = r.ID
		switch {
		case r.Service != "" && r.Upstream != "":
			add("route %s: points to both a service and an upstream", r.ID)
		case r.Service == "" && r.Upstream == "":
			add("route %s: no target", r.ID)
		case r.Service != "":
			if !names[r.Service] {
				add("route %s: service %q does not exist", r.ID, r.Service)
			}
		default:
			if err := ValidUpstream(r.Upstream); err != nil {
				add("route %s: %v", r.ID, err)
			}
		}
		if r.StripPrefix && r.Prefix == "/" {
			add("route %s: stripPrefix makes no sense for the / prefix", r.ID)
		}
		if r.MaxBodyBytes < 0 || r.ResponseHeaderTimeout < 0 {
			add("route %s: negative limit", r.ID)
		}
		if r.ACME != "" && r.ACME != "staging" {
			add("route %s: unknown ACME mode %q", r.ID, r.ACME)
		}
		wild := strings.HasPrefix(r.Host, "*.")
		if !wild && (len(r.TLSNames) > 0 || r.TLSOnDemand) {
			add("route %s: tlsNames and tlsOnDemand apply only to wildcard routes", r.ID)
		}
		for _, n := range r.TLSNames {
			if h, err := NormalizeHost(n); err != nil || h != n || strings.HasPrefix(n, "*.") || !MatchesWildcard(r.Host, n) {
				add("route %s: TLS name %q is not a single subdomain of %s", r.ID, n, r.Host)
			}
		}
		if prev, ok := hostACME[r.Host]; ok && prev != r.ACME {
			add("host %s: routes have different ACME modes", r.Host)
		}
		hostACME[r.Host] = r.ACME
	}

	tokIDs := map[string]bool{}
	for _, t := range s.Tokens {
		if tokIDs[t.ID] {
			add("token %s appears twice", t.ID)
		}
		tokIDs[t.ID] = true
		if !ids[t.ServiceID] {
			add("token %s: service does not exist", t.ID)
		}
		if len(t.Hash) != 64 {
			add("token %s: invalid hash", t.ID)
		}
		if t.MaxReplicas < 0 {
			add("token %s: negative replica limit", t.ID)
		}
	}
	if len(errs) > 0 {
		return &ValidationError{Problems: errs}
	}
	return nil
}

// ValidationError collects all validation problems.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "validation failed: " + strings.Join(e.Problems, "; ")
}

// Diff describes the changes between two configurations. It never contains token hashes.
func Diff(a, b *Spec) []string {
	var out []string
	as := map[string]Service{}
	for _, s := range a.Services {
		as[s.Name] = s
	}
	bs := map[string]Service{}
	for _, s := range b.Services {
		bs[s.Name] = s
		if old, ok := as[s.Name]; !ok {
			out = append(out, fmt.Sprintf("+ service %s (port %d)", s.Name, s.Port))
		} else if !reflect.DeepEqual(old, s) {
			out = append(out, fmt.Sprintf("~ service %s", s.Name))
		}
	}
	for _, s := range a.Services {
		if _, ok := bs[s.Name]; !ok {
			out = append(out, fmt.Sprintf("- service %s (port %d stays reserved)", s.Name, s.Port))
		}
	}
	ar := map[string]Route{}
	for _, r := range a.Routes {
		ar[r.ID] = r
	}
	br := map[string]bool{}
	for _, r := range b.Routes {
		br[r.ID] = true
		if old, ok := ar[r.ID]; !ok {
			out = append(out, fmt.Sprintf("+ route %s %s%s -> %s", r.ID, r.Host, r.Prefix, r.Target()))
		} else if !reflect.DeepEqual(old, r) {
			out = append(out, fmt.Sprintf("~ route %s %s%s -> %s", r.ID, r.Host, r.Prefix, r.Target()))
		}
	}
	for _, r := range a.Routes {
		if !br[r.ID] {
			out = append(out, fmt.Sprintf("- route %s %s%s", r.ID, r.Host, r.Prefix))
		}
	}
	at := map[string]bool{}
	for _, t := range a.Tokens {
		at[t.ID] = true
	}
	bt := map[string]bool{}
	for _, t := range b.Tokens {
		bt[t.ID] = true
		if !at[t.ID] {
			name := "?"
			if s := b.ServiceByID(t.ServiceID); s != nil {
				name = s.Name
			}
			out = append(out, fmt.Sprintf("+ token %s for service %s", t.ID, name))
		}
	}
	for _, t := range a.Tokens {
		if !bt[t.ID] {
			out = append(out, fmt.Sprintf("- token %s", t.ID))
		}
	}
	return out
}

// MatchesWildcard reports whether name is exactly one label under the wildcard host (*.example.com).
func MatchesWildcard(wildcard, name string) bool {
	i := strings.IndexByte(name, '.')
	return strings.HasPrefix(wildcard, "*.") && i > 0 && name[i:] == wildcard[1:]
}

// CertHosts returns names allowed to get certificates: exact route hosts, explicit TLS names,
// and wildcard hosts only when the route opted in to on-demand certificates.
func (s *Spec) CertHosts() []string {
	var hs []string
	seen := map[string]bool{}
	add := func(h string) {
		if !seen[h] {
			seen[h] = true
			hs = append(hs, h)
		}
	}
	for _, r := range s.Routes {
		if !strings.HasPrefix(r.Host, "*.") || r.TLSOnDemand {
			add(r.Host)
		}
		for _, n := range r.TLSNames {
			add(n)
		}
	}
	return hs
}
