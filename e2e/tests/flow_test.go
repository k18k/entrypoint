package tests

import (
	"io"
	"strings"
	"testing"
	"time"

	h "github.com/k18k/entrypoint/e2e/harness"
)

// Podstawowy przepływ: instalacja, serwis, trasa, token, replika z adapterem node:http, żądanie HTTPS.
func TestTunnels_BasicFlow(t *testing.T) {
	e := h.New(t)
	e.FreshGateway()
	e.SetDNS(map[string]string{"api.ep.test": h.GatewayIP})
	e.Service("api", "--health", "/health")
	e.Route("api.ep.test", "--service", "api")
	_, connect := e.Token("api")
	e.StartApp(h.AppOptions{Container: h.ClientA, Connect: connect, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	resp, err := e.HTTPClient().Get("https://api.ep.test/whoami")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"replica":"a"`) {
		t.Fatalf("odpowiedź: %s", b)
	}
	_ = time.Second
}
