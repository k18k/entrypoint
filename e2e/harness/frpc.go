package harness

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Bundle to zdekodowany pakiet połączenia (testy budują z niego konfigurację zmodyfikowanego frpc).
type Bundle struct {
	Server  string   `json:"server"`
	Port    int      `json:"port"`
	CAs     []string `json:"ca"`
	Service string   `json:"service"`
	Token   string   `json:"token"`
}

func DecodeBundle(s string) (Bundle, error) {
	var b Bundle
	raw, err := decodeB64(strings.TrimPrefix(s, "epc1."))
	if err != nil {
		return b, err
	}
	err = json.Unmarshal(raw, &b)
	return b, err
}

// TokenID zwraca identyfikator tokenu z pakietu.
func (b Bundle) TokenID() string {
	parts := strings.Split(b.Token, "_")
	if len(parts) < 3 {
		return ""
	}
	return parts[1]
}

// RawFrpc uruchamia frpc z własną konfiguracją (bez konektora), jak zmodyfikowany klient.
// proxies to fragment JSON listy proxies; heartbeat < 0 wyłącza heartbeat.
func (e *Env) RawFrpc(container, name string, b Bundle, proxies string, heartbeat int) {
	e.T.Helper()
	ca := strings.Join(b.CAs, "\n")
	e.WriteFile(container, "/run/ep/"+name+"-ca.pem", ca, "600")
	cfg := fmt.Sprintf(`{
  "serverAddr": %q, "serverPort": %d, "loginFailExit": false,
  "metadatas": {"token": "{{ .Envs.EP_TOK }}"},
  "transport": {"tls": {"enable": true, "trustedCaFile": "/run/ep/%s-ca.pem", "serverName": %q},
                "heartbeatInterval": %d, "heartbeatTimeout": 90},
  "log": {"to": "console", "level": "info", "disablePrintColor": true},
  "proxies": %s
}`, b.Server, b.Port, name, b.Server, heartbeat, proxies)
	e.WriteFile(container, "/run/ep/"+name+".json", cfg, "600")
	e.WriteFile(container, "/run/ep/"+name+".tok", b.Token, "600")
	e.MustSh(container, fmt.Sprintf(`EP_TOK="$(cat /run/ep/%[1]s.tok)" nohup frpc -c /run/ep/%[1]s.json > /tmp/frpc-%[1]s.log 2>&1 & echo $! > /tmp/frpc-%[1]s.pid`, name))
}

func (e *Env) KillRawFrpc(container, name string) {
	_, _ = e.Sh(container, fmt.Sprintf("kill -9 $(cat /tmp/frpc-%s.pid) 2>/dev/null; true", name))
}

func (e *Env) RawFrpcLog(container, name string) string {
	out, _ := e.Sh(container, fmt.Sprintf("cat /tmp/frpc-%s.log", name))
	return out
}
