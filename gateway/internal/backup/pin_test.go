package backup

import (
	"testing"

	"github.com/k18k/entrypoint/gateway/internal/config"
)

// A restored archive must not change host-specific settings that end up in systemd units.
func TestPinHostFields(t *testing.T) {
	cur := config.Default()
	b := cur
	b.PluginListen = "127.0.0.1:9999"
	b.CaddyBin = "/tmp/evil"
	b.APISocket, b.CaddyAdminSocket, b.FrpsAPI, b.DataDir = "/tmp/a", "/tmp/b", "127.0.0.1:1", "/tmp/d"
	b.GatewayName = "tunnel.example.com"
	pinHostFields(&b, cur)
	if b.PluginListen != cur.PluginListen || b.CaddyBin != cur.CaddyBin || b.APISocket != cur.APISocket ||
		b.CaddyAdminSocket != cur.CaddyAdminSocket || b.FrpsAPI != cur.FrpsAPI || b.DataDir != cur.DataDir {
		t.Fatalf("host fields taken from archive: %+v", b)
	}
	if b.GatewayName != "tunnel.example.com" {
		t.Fatal("non-host field overwritten")
	}
}
