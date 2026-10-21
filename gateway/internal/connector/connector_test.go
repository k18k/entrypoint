package connector

import (
	"strings"
	"testing"

	"github.com/k18k/entrypoint/gateway/internal/token"
)

// Strix vuln-0004: frpc renders its config as a Go template, so template delimiters are refused.
func TestNewRejectsTemplateDelimiters(t *testing.T) {
	full, _, _, _ := token.New()
	b := token.Bundle{V: 1, Server: "tunnel.example.com", Port: 55555, CAs: []string{"x"}, Service: "api", Token: full}
	o := Options{Bundle: b, Service: "api", Upstream: "http://127.0.0.1:3000", Health: "/{{ .Envs.EP_FRPC_TOKEN }}", FrpcPath: "/bin/true"}
	if _, err := New(o); err == nil || !strings.Contains(err.Error(), "template") {
		t.Fatalf("template in --health accepted: %v", err)
	}
	o.Health = "/health"
	if _, err := New(o); err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
}
