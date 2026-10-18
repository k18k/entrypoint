package authz

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/k18k/entrypoint/gateway/internal/render"
	"github.com/k18k/entrypoint/gateway/internal/spec"
	"github.com/k18k/entrypoint/gateway/internal/token"
)

func setup(t *testing.T) (*Server, string, string) {
	full, id, hash, _ := token.New()
	s := &spec.Spec{
		Services: []spec.Service{{ID: 1, Name: "api", Port: 20000}, {ID: 2, Name: "other", Port: 20001}},
		Tokens:   []spec.Token{{ID: id, ServiceID: 1, Hash: hash}},
		Routes:   []spec.Route{{ID: "r000001", Host: "a.example.com", Prefix: "/", Service: "api"}, {ID: "r000002", Host: "*.w.example.com", Prefix: "/", Service: "api"}},
	}
	srv := New(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, 3, 2, nil)
	srv.Swap(render.Authz(1, s), map[string]bool{}, []string{"a.example.com", "*.w.example.com"})
	return srv, full, id
}

func call(t *testing.T, s *Server, op string, content any) response {
	b, _ := json.Marshal(map[string]any{"op": op, "content": content})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/frps/handler", bytes.NewReader(b)))
	var r response
	json.Unmarshal(w.Body.Bytes(), &r)
	return r
}

func TestLoginPingRevoke(t *testing.T) {
	s, full, id := setup(t)
	login := map[string]any{"metas": map[string]string{"token": full}, "client_address": "1.2.3.4:5"}
	if r := call(t, s, "Login", login); r.Reject {
		t.Fatalf("poprawny token odrzucony: %v", r.RejectReason)
	}
	ping := map[string]any{"user": map[string]any{"metas": map[string]string{"token": full}}}
	if r := call(t, s, "Ping", ping); r.Reject {
		t.Fatal("ping odrzucony")
	}
	s.Revoke(id)
	if r := call(t, s, "Ping", ping); !r.Reject {
		t.Fatal("ping odwołanego tokenu przyjęty")
	}
	if r := call(t, s, "Login", login); !r.Reject {
		t.Fatal("login odwołanego tokenu przyjęty")
	}
	// Ponowna podmiana snapshotu nie przywraca odwołanego tokenu, jeśli zbiór odwołań go zawiera.
	s.Swap(render.Authz(2, &spec.Spec{}), map[string]bool{id: true}, nil)
	if r := call(t, s, "Login", login); !r.Reject {
		t.Fatal("token po podmianie")
	}
}

func TestFailLimit(t *testing.T) {
	s, full, _ := setup(t)
	bad := map[string]any{"metas": map[string]string{"token": "ept_000000000000_zle"}, "client_address": "9.9.9.9:1"}
	for i := 0; i < 3; i++ {
		call(t, s, "Login", bad)
	}
	good := map[string]any{"metas": map[string]string{"token": full}, "client_address": "9.9.9.9:2"}
	if r := call(t, s, "Login", good); !r.Reject {
		t.Fatal("limit nieudanych logowań nie działa")
	}
	other := map[string]any{"metas": map[string]string{"token": full}, "client_address": "8.8.8.8:2"}
	if r := call(t, s, "Login", other); r.Reject {
		t.Fatal("limit dotyka innego adresu")
	}
}

func TestAsk(t *testing.T) {
	s, _, _ := setup(t)
	code := func(d string) int {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/tls/ask?domain="+d, nil))
		return w.Code
	}
	if code("a.example.com") != 200 || code("x.w.example.com") != 200 || code("b.example.com") != 403 || code("a.b.w.example.com") != 403 {
		t.Fatal("zgody TLS")
	}
	code("y.w.example.com")
	if code("z.w.example.com") != 429 {
		t.Fatal("limit nowych subdomen")
	}
}
