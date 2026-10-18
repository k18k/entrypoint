package render

import (
	"os"
	"os/exec"
	"path/filepath"
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
		t.Skip("brak binarki caddy")
	}
	s := sample()
	if err := s.Validate(20000, 29999); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	o := CaddyOptions{AdminSocket: filepath.Join(dir, "a.sock"), StorageDir: filepath.Join(dir, "st"), AskEndpoint: "http://127.0.0.1:7400/tls/ask",
		ACME: config.ACME{Email: "a@example.com", CA: config.LetsEncryptProd, StagingCA: config.LetsEncryptStaging}}
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
		t.Fatal("hash zależy od kolejności kluczy")
	}
}
