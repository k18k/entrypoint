package spec

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{"API.Example.com.": "api.example.com", "*.example.com": "*.example.com"} {
		got, err := NormalizeHost(in)
		if err != nil || got != want {
			t.Errorf("%q -> %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "localhost", "*.com", "a.*.com", "10.0.0.1", "exa mple.com", "-a.com"} {
		if _, err := NormalizeHost(bad); err == nil {
			t.Errorf("host %q przyjęty", bad)
		}
	}
	for in, want := range map[string]string{"": "/", "/": "/", "api": "/api", "/api/": "/api", "/a/b": "/a/b"} {
		got, err := NormalizePrefix(in)
		if err != nil || got != want {
			t.Errorf("prefiks %q -> %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"/a/../b", "/a/./b", "/a%2fb", "/a b", "/*"} {
		if _, err := NormalizePrefix(bad); err == nil {
			t.Errorf("prefiks %q przyjęty", bad)
		}
	}
}

func TestValidate(t *testing.T) {
	s := &Spec{
		Services: []Service{{ID: 1, Name: "api", Port: 20000}},
		Routes: []Route{
			{ID: "r000001", Host: "a.example.com", Prefix: "/", Service: "api"},
			{ID: "r000002", Host: "a.example.com", Prefix: "/", Upstream: "10.0.0.1:80"},
			{ID: "r000003", Host: "b.example.com", Prefix: "/", Service: "brak"},
			{ID: "r000004", Host: "c.example.com", Prefix: "/", Upstream: "host:80"},
		},
		Tokens: []Token{{ID: "t1", ServiceID: 9, Hash: strings.Repeat("a", 64)}},
	}
	err := s.Validate(20000, 29999)
	if err == nil {
		t.Fatal("brak błędów walidacji")
	}
	for _, want := range []string{"konflikt tras", "serwis \"brak\"", "nie jest adresem IP", "token t1: serwis nie istnieje"} {
		if !strings.Contains(err.Error(), strings.ReplaceAll(want, "\\\\", "")) {
			t.Errorf("brak %q w: %v", want, err)
		}
	}
}
