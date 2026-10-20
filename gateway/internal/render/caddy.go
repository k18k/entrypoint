// Pakiet render tworzy artefakty rewizji: konfigurację Caddy (JSON) i snapshot autoryzacji.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/k18k/entrypoint/gateway/internal/config"
	"github.com/k18k/entrypoint/gateway/internal/spec"
)

type obj = map[string]any

// CaddyOptions to parametry renderowania niezależne od rewizji.
type CaddyOptions struct {
	AdminSocket string
	StorageDir  string
	AskEndpoint string
	ACME        config.ACME
	Testing     config.Testing
}

func OptionsFrom(c config.Config) CaddyOptions {
	return CaddyOptions{
		AdminSocket: c.CaddyAdminSocket,
		StorageDir:  c.CaddyStorage(),
		AskEndpoint: "http://" + c.PluginListen + "/tls/ask",
		ACME:        c.ACME,
		Testing:     c.Testing,
	}
}

func issuer(o CaddyOptions, staging bool) obj {
	ca := o.ACME.CA
	if staging {
		ca = o.ACME.StagingCA
	}
	is := obj{"module": "acme", "ca": ca}
	if o.ACME.Email != "" {
		is["email"] = o.ACME.Email
	}
	if len(o.ACME.TrustedRoots) > 0 {
		is["trusted_roots_pem_files"] = o.ACME.TrustedRoots
	}
	if o.Testing.ACMEProfile != "" {
		is["profile"] = o.Testing.ACMEProfile
	}
	return is
}

// matchPrefix buduje matcher ścieżki na granicy segmentu: /api obejmuje /api i /api/x, ale nie /apix.
func matchPrefix(p string) []string {
	return []string{p, p + "/*"}
}

func routeHandlers(r spec.Route, s *spec.Spec) []any {
	hs := []any{obj{"handler": "request_body", "max_size": r.EffectiveMaxBody()}}
	if r.StripPrefix {
		hs = append(hs, obj{"handler": "rewrite", "strip_path_prefix": r.Prefix})
	}
	dial := r.Upstream
	if r.Service != "" {
		svc := s.ServiceByName(r.Service)
		dial = fmt.Sprintf("127.0.0.1:%d", svc.Port)
	}
	rp := obj{
		"handler":   "reverse_proxy",
		"upstreams": []any{obj{"dial": dial}},
		"transport": obj{
			"protocol":                "http",
			"dial_timeout":            "5s",
			"response_header_timeout": fmt.Sprintf("%ds", r.EffectiveHeaderTimeout()),
			"keep_alive":              obj{"idle_timeout": "4s"},
		},
		// Przeładowanie konfiguracji nie zrywa WebSocketów od razu.
		"stream_close_delay": "1h",
		// Przed Caddy nie stoi zaufane proxy: nagłówki adresu klienta od klienta są usuwane.
		// X-Forwarded-For/Proto/Host Caddy ustawia sam.
		"headers": obj{"request": obj{"delete": []string{"X-Real-IP", "Forwarded", "X-Client-IP", "True-Client-IP", "CF-Connecting-IP"}}},
	}
	return append(hs, rp)
}

// SortHosts ustawia dokładne hosty przed wildcardami, dalej alfabetycznie.
func SortHosts(hosts []string) {
	sort.Slice(hosts, func(i, j int) bool {
		wi, wj := strings.HasPrefix(hosts[i], "*."), strings.HasPrefix(hosts[j], "*.")
		if wi != wj {
			return !wi
		}
		return hosts[i] < hosts[j]
	})
}

// Caddy renderuje pełną konfigurację Caddy dla danej konfiguracji.
func Caddy(s *spec.Spec, o CaddyOptions) ([]byte, error) {
	byHost := map[string][]spec.Route{}
	for _, r := range s.Routes {
		byHost[r.Host] = append(byHost[r.Host], r)
	}
	hosts := make([]string, 0, len(byHost))
	for h := range byHost {
		hosts = append(hosts, h)
	}
	SortHosts(hosts)

	routes := []any{}
	var exactProd, exactStaging, wildProd, wildStaging, automate, skip []string
	for _, h := range hosts {
		rs := byHost[h]
		// Najdłuższy prefiks wygrywa: sortujemy malejąco po długości.
		sort.Slice(rs, func(i, j int) bool {
			if len(rs[i].Prefix) != len(rs[j].Prefix) {
				return len(rs[i].Prefix) > len(rs[j].Prefix)
			}
			return rs[i].Prefix < rs[j].Prefix
		})
		sub := []any{}
		for _, r := range rs {
			rt := obj{"handle": routeHandlers(r, s), "terminal": true}
			if r.Prefix != "/" {
				rt["match"] = []any{obj{"path": matchPrefix(r.Prefix)}}
			}
			sub = append(sub, rt)
		}
		sub = append(sub, obj{"handle": []any{obj{"handler": "static_response", "status_code": 404}}})
		routes = append(routes, obj{
			"match":    []any{obj{"host": []string{h}}},
			"handle":   []any{obj{"handler": "subroute", "routes": sub}},
			"terminal": true,
		})
		staging := rs[0].ACME == "staging"
		wild := strings.HasPrefix(h, "*.")
		names := []string{h}
		if wild {
			names = nil
			onDemand := false
			for _, r := range rs {
				names = append(names, r.TLSNames...)
				onDemand = onDemand || r.TLSOnDemand
			}
			automate = append(automate, names...)
			skip = append(skip, h)
			if onDemand {
				if staging {
					wildStaging = append(wildStaging, h)
				} else {
					wildProd = append(wildProd, h)
				}
			}
		}
		if staging {
			exactStaging = append(exactStaging, names...)
		} else {
			exactProd = append(exactProd, names...)
		}
	}
	// Host bez trasy: 404 (dotyczy tylko żądań, dla których TLS w ogóle się zestawił).
	routes = append(routes, obj{"handle": []any{obj{"handler": "static_response", "status_code": 404}}})

	server := obj{
		"listen":    []string{":443"},
		"protocols": []string{"h1", "h2"},
		"routes":    routes,
		"logs":      obj{},
	}
	if len(skip) > 0 {
		// No wildcard certificates (they need DNS-01, v1); subdomains get certificates only as
		// explicit TLS names, or on demand when the route opted in.
		server["automatic_https"] = obj{"skip_certificates": skip}
	}

	policies := []any{}
	if len(exactProd) > 0 {
		policies = append(policies, obj{"subjects": exactProd, "issuers": []any{issuer(o, false)}})
	}
	if len(exactStaging) > 0 {
		policies = append(policies, obj{"subjects": exactStaging, "issuers": []any{issuer(o, true)}})
	}
	if len(wildProd) > 0 {
		policies = append(policies, obj{"subjects": wildProd, "issuers": []any{issuer(o, false)}, "on_demand": true})
	}
	if len(wildStaging) > 0 {
		policies = append(policies, obj{"subjects": wildStaging, "issuers": []any{issuer(o, true)}, "on_demand": true})
	}
	// Polityka domyślna (bez subjects) też korzysta z naszego CA, nigdy z domyślnych Caddy.
	policies = append(policies, obj{"issuers": []any{issuer(o, false)}})

	automation := obj{
		"policies": policies,
		"on_demand": obj{
			"permission": obj{"module": "http", "endpoint": o.AskEndpoint},
		},
	}
	if d := o.Testing.RenewInterval.Duration; d > 0 {
		automation["renew_interval"] = d.String()
	}
	tlsApp := obj{"automation": automation}
	if len(automate) > 0 {
		sort.Strings(automate)
		tlsApp["certificates"] = obj{"automate": automate}
	}

	cfg := obj{
		"admin": obj{
			"listen": "unix/" + o.AdminSocket + "|0660",
			"config": obj{"persist": false},
		},
		"storage": obj{"module": "file_system", "root": o.StorageDir},
		"logging": obj{"logs": obj{"default": obj{"level": "INFO"}}},
		"apps": obj{
			"http": obj{"servers": obj{"entrypoint": server}},
			"tls":  tlsApp,
		},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// Hash liczy hash konfiguracji w postaci kanonicznej (klucze posortowane, bez formatowania).
func Hash(cfg []byte) (string, error) {
	var v any
	if err := json.Unmarshal(cfg, &v); err != nil {
		return "", err
	}
	canon, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(canon)
	return hex.EncodeToString(h[:]), nil
}

func shortHash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:12])
}
