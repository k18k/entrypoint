// Pakiet spec definiuje dokument konfiguracji zapisywany w rewizjach
// oraz jego walidację.
package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
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

// Spec to pełna konfiguracja oczekiwana w danej rewizji.
type Spec struct {
	Services []Service `json:"services"`
	Routes   []Route   `json:"routes"`
	Tokens   []Token   `json:"tokens"`
}

// Service to serwis tunelu ze stałym portem.
type Service struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Port   int    `json:"port"`
	Health string `json:"health,omitempty"`
}

// Route kieruje parę host+prefiks do serwisu albo upstreamu IP:port.
type Route struct {
	ID                    string `json:"id"`
	Host                  string `json:"host"`
	Prefix                string `json:"prefix"`
	Service               string `json:"service,omitempty"`
	Upstream              string `json:"upstream,omitempty"`
	StripPrefix           bool   `json:"stripPrefix,omitempty"`
	MaxBodyBytes          int64  `json:"maxBodyBytes,omitempty"`
	ResponseHeaderTimeout int    `json:"responseHeaderTimeout,omitempty"` // sekundy
	ACME                  string `json:"acme,omitempty"`                  // "" (produkcja) albo "staging"
}

// Token to wpis tokenu serwisu. Przechowywany jest wyłącznie hash sekretu.
type Token struct {
	ID          string    `json:"id"`
	ServiceID   int64     `json:"serviceId"`
	Hash        string    `json:"hash"`
	Label       string    `json:"label,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	MaxReplicas int       `json:"maxReplicas,omitempty"`
}

// Scope zwraca uprawnienie tokenu.
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

// Target opisuje cel trasy w czytelnej formie.
func (r Route) Target() string {
	if r.Service != "" {
		return "serwis " + r.Service
	}
	return r.Upstream
}

// Clone tworzy głęboką kopię.
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

// Normalize porządkuje kolejność elementów, żeby rewizje były porównywalne.
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

// ValidServiceName sprawdza nazwę serwisu.
func ValidServiceName(n string) error {
	if !serviceNameRe.MatchString(n) {
		return fmt.Errorf("niepoprawna nazwa serwisu %q: dozwolone małe litery, cyfry i '-', start literą, do 31 znaków", n)
	}
	return nil
}

// NormalizeHost sprowadza nazwę hosta do małych liter i ją sprawdza.
func NormalizeHost(h string) (string, error) {
	h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	if h == "" {
		return "", errors.New("pusty host")
	}
	if len(h) > 253 {
		return "", fmt.Errorf("host %q jest za długi", h)
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("host %q musi mieć co najmniej dwie etykiety", h)
	}
	for i, l := range labels {
		if l == "*" && i == 0 {
			if len(labels) < 3 {
				return "", fmt.Errorf("wildcard %q obejmuje całą domenę najwyższego poziomu", h)
			}
			continue
		}
		if !labelRe.MatchString(l) {
			return "", fmt.Errorf("niepoprawny host %q (wildcard dozwolony tylko jako pierwsza etykieta)", h)
		}
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return "", fmt.Errorf("host %q jest adresem IP; trasy wymagają nazwy DNS", h)
	}
	return h, nil
}

// NormalizePrefix sprowadza prefiks do postaci kanonicznej: zaczyna się od '/', bez końcowego '/'.
func NormalizePrefix(p string) (string, error) {
	if p == "" || p == "/" {
		return "/", nil
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = strings.TrimRight(p, "/")
	if !prefixRe.MatchString(p) {
		return "", fmt.Errorf("niepoprawny prefiks %q", p)
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("prefiks %q zawiera segment %q", p, seg)
		}
	}
	return p, nil
}

// ValidUpstream sprawdza adres IP:port.
func ValidUpstream(u string) error {
	host, port, err := net.SplitHostPort(u)
	if err != nil {
		return fmt.Errorf("upstream %q: oczekiwano IP:port", u)
	}
	if _, err := netip.ParseAddr(host); err != nil {
		return fmt.Errorf("upstream %q: %q nie jest adresem IP", u, host)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("upstream %q: niepoprawny port", u)
	}
	return nil
}

// ValidHealthPath sprawdza ścieżkę health checku.
func ValidHealthPath(p string) error {
	if p == "" {
		return nil
	}
	if !healthRe.MatchString(p) {
		return fmt.Errorf("niepoprawna ścieżka health checku %q", p)
	}
	return nil
}

// Validate sprawdza spójność całej konfiguracji: schemat, konflikty tras, istnienie serwisów.
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
			add("serwis %q występuje dwa razy", svc.Name)
		}
		names[svc.Name] = true
		if svc.ID <= 0 || ids[svc.ID] {
			add("serwis %q: niepoprawny identyfikator", svc.Name)
		}
		ids[svc.ID] = true
		if svc.Port < poolStart || svc.Port > poolEnd {
			add("serwis %q: port %d spoza puli %d-%d", svc.Name, svc.Port, poolStart, poolEnd)
		}
		if ports[svc.Port] {
			add("port %d przypisany do dwóch serwisów", svc.Port)
		}
		ports[svc.Port] = true
		if err := ValidHealthPath(svc.Health); err != nil {
			add("serwis %q: %v", svc.Name, err)
		}
	}

	type key struct{ host, prefix string }
	seen := map[key]string{}
	routeIDs := map[string]bool{}
	hostACME := map[string]string{}
	for _, r := range s.Routes {
		if !routeIDRe.MatchString(r.ID) || routeIDs[r.ID] {
			add("trasa %q: niepoprawny lub powtórzony identyfikator", r.ID)
		}
		routeIDs[r.ID] = true
		h, err := NormalizeHost(r.Host)
		if err != nil {
			add("trasa %s: %v", r.ID, err)
		} else if h != r.Host {
			add("trasa %s: host nie jest znormalizowany", r.ID)
		}
		p, err := NormalizePrefix(r.Prefix)
		if err != nil {
			add("trasa %s: %v", r.ID, err)
		} else if p != r.Prefix {
			add("trasa %s: prefiks nie jest znormalizowany", r.ID)
		}
		k := key{r.Host, r.Prefix}
		if other, ok := seen[k]; ok {
			add("konflikt tras: %s%s jest już zajęte przez trasę %s", r.Host, r.Prefix, other)
		}
		seen[k] = r.ID
		switch {
		case r.Service != "" && r.Upstream != "":
			add("trasa %s: wskazuje jednocześnie serwis i upstream", r.ID)
		case r.Service == "" && r.Upstream == "":
			add("trasa %s: brak celu", r.ID)
		case r.Service != "":
			if !names[r.Service] {
				add("trasa %s: serwis %q nie istnieje", r.ID, r.Service)
			}
		default:
			if err := ValidUpstream(r.Upstream); err != nil {
				add("trasa %s: %v", r.ID, err)
			}
		}
		if r.StripPrefix && r.Prefix == "/" {
			add("trasa %s: stripPrefix nie ma sensu dla prefiksu /", r.ID)
		}
		if r.MaxBodyBytes < 0 || r.ResponseHeaderTimeout < 0 {
			add("trasa %s: ujemny limit", r.ID)
		}
		if r.ACME != "" && r.ACME != "staging" {
			add("trasa %s: nieznany tryb ACME %q", r.ID, r.ACME)
		}
		if prev, ok := hostACME[r.Host]; ok && prev != r.ACME {
			add("host %s: trasy mają różne tryby ACME", r.Host)
		}
		hostACME[r.Host] = r.ACME
	}

	tokIDs := map[string]bool{}
	for _, t := range s.Tokens {
		if tokIDs[t.ID] {
			add("token %s występuje dwa razy", t.ID)
		}
		tokIDs[t.ID] = true
		if !ids[t.ServiceID] {
			add("token %s: serwis nie istnieje", t.ID)
		}
		if len(t.Hash) != 64 {
			add("token %s: niepoprawny hash", t.ID)
		}
		if t.MaxReplicas < 0 {
			add("token %s: ujemny limit replik", t.ID)
		}
	}
	if len(errs) > 0 {
		return &ValidationError{Problems: errs}
	}
	return nil
}

// ValidationError zbiera wszystkie problemy walidacji.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "walidacja nie powiodła się: " + strings.Join(e.Problems, "; ")
}

// Diff opisuje zmiany między dwiema konfiguracjami. Nie zawiera hashy tokenów.
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
			out = append(out, fmt.Sprintf("+ serwis %s (port %d)", s.Name, s.Port))
		} else if old != s {
			out = append(out, fmt.Sprintf("~ serwis %s", s.Name))
		}
	}
	for _, s := range a.Services {
		if _, ok := bs[s.Name]; !ok {
			out = append(out, fmt.Sprintf("- serwis %s (port %d zostaje zarezerwowany)", s.Name, s.Port))
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
			out = append(out, fmt.Sprintf("+ trasa %s %s%s -> %s", r.ID, r.Host, r.Prefix, r.Target()))
		} else if old != r {
			out = append(out, fmt.Sprintf("~ trasa %s %s%s -> %s", r.ID, r.Host, r.Prefix, r.Target()))
		}
	}
	for _, r := range a.Routes {
		if !br[r.ID] {
			out = append(out, fmt.Sprintf("- trasa %s %s%s", r.ID, r.Host, r.Prefix))
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
			out = append(out, fmt.Sprintf("+ token %s dla serwisu %s", t.ID, name))
		}
	}
	for _, t := range a.Tokens {
		if !bt[t.ID] {
			out = append(out, fmt.Sprintf("- token %s", t.ID))
		}
	}
	return out
}
