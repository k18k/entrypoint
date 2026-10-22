package render

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/k18k/entrypoint/gateway/internal/config"
	"github.com/k18k/entrypoint/gateway/internal/spec"
)

func sample() *spec.Spec {
	s := &spec.Spec{
		Services: []spec.Service{{ID: 1, Name: "api", Port: 20000, Health: "/health"}},
		Routes: []spec.Route{
			{ID: "r000001", Host: "app.example.com", Prefix: "/", Upstream: "10.0.0.5:8080"},
			{ID: "r000002", Host: "app.example.com", Prefix: "/api", Service: "api", StripPrefix: true},
			{ID: "r000003", Host: "*.example.com", Prefix: "/", Service: "api"},
			{ID: "r000004", Host: "test.example.com", Prefix: "/", Upstream: "10.0.0.5:8080", ACME: "staging"},
		},
	}
	s.Normalize()
	return s
}

func TestCaddyValidates(t *testing.T) {
	bin := os.Getenv("CADDY_BIN")
	if bin == "" {
		bin = "../../../dist/deps/linux_amd64/caddy"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip("no caddy binary")
	}
	s := sample()
	if err := s.Validate(20000, 29999); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	o := CaddyOptions{AdminSocket: filepath.Join(dir, "a.sock"), StorageDir: filepath.Join(dir, "st"), AskEndpoint: "http://127.0.0.1:7400/tls/ask",
		ACME: config.ACME{Email: "a@example.com", CA: config.LetsEncryptProd, StagingCA: config.LetsEncryptStaging},
		VPN:  config.VPN{Name: "vpn.example.com", Listen: "127.0.0.1:7600"}, ServiceConns: 1024}
	b, err := Caddy(s, o)
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "c.json")
	os.WriteFile(f, b, 0o600)
	cmd := exec.Command(bin, "validate", "--config", f)
	cmd.Env = []string{"HOME=" + dir}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("caddy validate: %v\n%s\n%s", err, out, b)
	}
}

func TestHashCanonical(t *testing.T) {
	a, _ := Hash([]byte(`{"b":1,"a":{"y":2,"x":1}}`))
	b, _ := Hash([]byte("{\n \"a\": {\"x\":1,\"y\":2}, \"b\": 1}"))
	if a != b {
		t.Fatal("hash depends on key order")
	}
}

// Issue #3: a wildcard route alone authorizes no certificates; explicit names are automated,
// on-demand only when opted in.
func TestWildcardCertConsent(t *testing.T) {
	s := &spec.Spec{Services: []spec.Service{{ID: 1, Name: "api", Port: 20000}}, Routes: []spec.Route{
		{ID: "r000001", Host: "*.a.example.com", Prefix: "/", Service: "api"},
		{ID: "r000002", Host: "*.b.example.com", Prefix: "/", Service: "api", TLSNames: []string{"x.b.example.com"}},
		{ID: "r000003", Host: "*.c.example.com", Prefix: "/", Service: "api", TLSOnDemand: true},
		{ID: "r000004", Host: "d.example.com", Prefix: "/", Service: "api"},
	}}
	if err := s.Validate(20000, 29999); err != nil {
		t.Fatal(err)
	}
	hosts := s.CertHosts()
	sort.Strings(hosts)
	if got := strings.Join(hosts, ","); got != "*.c.example.com,d.example.com,x.b.example.com" {
		t.Fatalf("CertHosts = %v", hosts)
	}
	b, _ := Caddy(s, CaddyOptions{ACME: config.ACME{CA: "https://ca"}})
	cfg := string(b)
	if !strings.Contains(cfg, `"automate": [`) || !strings.Contains(cfg, `"x.b.example.com"`) || strings.Count(cfg, `"on_demand": true`) != 1 {
		t.Fatalf("render:\n%s", cfg)
	}
	bad := &spec.Spec{Services: s.Services, Routes: []spec.Route{
		{ID: "r000005", Host: "*.b.example.com", Prefix: "/", Service: "api", TLSNames: []string{"y.z.b.example.com"}},
		{ID: "r000006", Host: "e.example.com", Prefix: "/", Service: "api", TLSOnDemand: true},
	}}
	if err := bad.Validate(20000, 29999); err == nil {
		t.Fatal("invalid TLS names accepted")
	}
}

func TestVPNHostReserved(t *testing.T) {
	o := CaddyOptions{ACME: config.ACME{CA: "https://ca"}, VPN: config.VPN{Name: "app.example.com", Listen: "127.0.0.1:7600"}}
	if _, err := Caddy(sample(), o); err == nil || !strings.Contains(err.Error(), "reserved for the VPN") {
		t.Fatalf("route on the VPN host accepted: %v", err)
	}
	o.VPN.Name = "vpn.example.com"
	b, err := Caddy(sample(), o)
	if err != nil {
		t.Fatal(err)
	}
	// The control server comes before the *.example.com wildcard.
	cfg := string(b)
	if i, j := strings.Index(cfg, `"127.0.0.1:7600"`), strings.LastIndex(cfg, `"*.example.com"`); i < 0 || i > j {
		t.Fatalf("VPN route missing or after the wildcard:\n%s", cfg)
	}
}
