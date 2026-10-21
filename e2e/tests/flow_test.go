package tests

import (
	"io"
	"strings"
	"testing"
	"time"

	h "github.com/k18k/entrypoint/e2e/harness"
)

// Basic flow: install, service, route, token, replica with the node:http adapter, HTTPS request.
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
		t.Fatalf("response: %s", b)
	}
	_ = time.Second
}
