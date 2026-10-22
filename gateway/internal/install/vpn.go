package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/k18k/entrypoint/gateway/internal/config"
)

// VPNUser is the Headscale user that owns every node, the gateway included.
const VPNUser = "entrypoint"

// STUNPort is the embedded DERP server's STUN port (UDP, public).
const STUNPort = 3478

// HeadscaleConfig renders Headscale's configuration. JSON is valid YAML, which Headscale reads.
func HeadscaleConfig(cfg config.Config) ([]byte, error) {
	hd := cfg.HeadscaleDir()
	derp := map[string]any{
		"enabled":                                true,
		"region_id":                              999,
		"region_code":                            "entrypoint",
		"region_name":                            "Entrypoint " + cfg.VPN.Name,
		"verify_clients":                         true,
		"stun_listen_addr":                       fmt.Sprintf("0.0.0.0:%d", STUNPort),
		"private_key_path":                       filepath.Join(hd, "derp_server_private.key"),
		"automatically_add_embedded_derp_region": true,
	}
	for _, s := range cfg.PublicIPs {
		if a, err := netip.ParseAddr(s); err == nil {
			k := "ipv6"
			if a.Is4() {
				k = "ipv4"
			}
			if derp[k] == nil {
				derp[k] = a.String()
			}
		}
	}
	c := map[string]any{
		"server_url":          "https://" + cfg.VPN.Name,
		"listen_addr":         cfg.VPN.Listen,
		"metrics_listen_addr": cfg.VPNMetrics(),
		"trusted_proxies":     []string{"127.0.0.1/32", "::1/128"},
		"noise":               map[string]any{"private_key_path": filepath.Join(hd, "noise_private.key")},
		"prefixes":            map[string]any{"v4": "100.64.0.0/10", "v6": "fd7a:115c:a1e0::/48", "allocation": "sequential"},
		// Only the embedded relay: no traffic or DERP map lookups through Tailscale Inc.
		"derp": map[string]any{
			"server":              derp,
			"urls":                []string{},
			"paths":               []string{},
			"auto_update_enabled": false,
		},
		"disable_check_updates": true,
		"database": map[string]any{
			"type":   "sqlite",
			"sqlite": map[string]any{"path": filepath.Join(hd, "db.sqlite"), "write_ahead_log": true},
		},
		"log": map[string]any{"level": "info", "format": "text"},
		// No policy file: every node may reach every other node.
		"policy": map[string]any{"mode": "file", "path": ""},
		"dns":                    map[string]any{"magic_dns": false, "override_local_dns": false, "nameservers": map[string]any{"global": []string{}}},
		"unix_socket":            config.HeadscaleSocket,
		"unix_socket_permission": "0770",
		"logtail":                map[string]any{"enabled": false},
		"taildrop":               map[string]any{"enabled": false},
	}
	return json.MarshalIndent(c, "", "  ")
}

// Headscale runs the Headscale CLI against the local server.
func Headscale(cfg config.Config, args ...string) ([]byte, error) {
	cmd := exec.Command(filepath.Join(config.OptDir, "current", "bin", "headscale"), append([]string{"-c", cfg.HeadscaleConfig(), "--force"}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if ee, ok := err.(*exec.ExitError); ok && msg == "" {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		return nil, fmt.Errorf("headscale %s: %v: %s", args[0], err, msg)
	}
	return out, nil
}

// Tailscale runs the Tailscale CLI against the gateway's tailscaled.
func Tailscale(args ...string) ([]byte, error) {
	cmd := exec.Command(filepath.Join(config.OptDir, "current", "bin", "tailscale"), append([]string{"--socket", config.TailscaleSocket}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("tailscale %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// VPNUserID returns the ID of the Headscale user for nodes, creating it on first use.
func VPNUserID(cfg config.Config) (uint64, error) {
	out, err := Headscale(cfg, "users", "list", "-o", "json")
	if err != nil {
		return 0, err
	}
	var users []struct {
		ID   uint64 `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &users); err != nil {
		return 0, fmt.Errorf("headscale users list: %w", err)
	}
	for _, u := range users {
		if u.Name == VPNUser {
			return u.ID, nil
		}
	}
	out, err = Headscale(cfg, "users", "create", VPNUser, "-o", "json")
	if err != nil {
		return 0, err
	}
	var u struct {
		ID uint64 `json:"id"`
	}
	if err := json.Unmarshal(out, &u); err != nil || u.ID == 0 {
		return 0, fmt.Errorf("headscale users create: unexpected output")
	}
	return u.ID, nil
}

// PreAuthKey issues a Headscale pre-auth key for a new node.
func PreAuthKey(cfg config.Config, expiration string, reusable bool) (string, error) {
	uid, err := VPNUserID(cfg)
	if err != nil {
		return "", err
	}
	args := []string{"preauthkeys", "create", "-u", strconv.FormatUint(uid, 10), "-e", expiration, "-o", "json"}
	if reusable {
		args = append(args, "--reusable")
	}
	out, err := Headscale(cfg, args...)
	if err != nil {
		return "", err
	}
	var k struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(out, &k); err != nil || k.Key == "" {
		return "", errors.New("headscale preauthkeys create: unexpected output")
	}
	return k.Key, nil
}

// joinVPN makes the gateway a node of its own VPN, so routes can point at node addresses.
// It logs in over loopback, so it doesn't wait for the control server's certificate.
func joinVPN(cfg config.Config, out io.Writer) error {
	var err error
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var st []byte
		if st, err = Tailscale("status", "--json"); err == nil {
			var s struct{ BackendState string }
			if json.Unmarshal(st, &s) == nil && s.BackendState == "Running" {
				return nil
			}
			if _, err = VPNUserID(cfg); err == nil {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("VPN: %w", err)
	}
	key, err := PreAuthKey(cfg, "10m", false)
	if err != nil {
		return err
	}
	kf := filepath.Join(cfg.TailscaleDir(), "join-key")
	if err := os.WriteFile(kf, []byte(key), 0o600); err != nil {
		return err
	}
	defer os.Remove(kf)
	// The key goes through a file, never the command line.
	if _, err := Tailscale("up", "--login-server", "http://"+cfg.VPN.Listen, "--auth-key", "file:"+kf,
		"--hostname", "gateway", "--accept-dns=false", "--timeout", "60s"); err != nil {
		return err
	}
	ip, _ := Tailscale("ip", "-4")
	fmt.Fprintf(out, "The gateway joined the VPN as %s.\n", strings.TrimSpace(string(ip)))
	return nil
}
