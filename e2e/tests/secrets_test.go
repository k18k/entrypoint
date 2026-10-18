package tests

// Zestaw e2e 12: ochrona sekretów.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	h "github.com/k18k/entrypoint/e2e/harness"
)

// Pełny cykl z błędami i odwołaniem, potem przeszukanie wszystkich logów pod kątem sekretów.
func TestSecrets_NoSecretsInLogs(t *testing.T) {
	e := apiSetup(t)
	id, ca := e.Token("api")
	_, cb := e.Token("api")
	e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	// Błędy: zniekształcony pakiet, token w złym formacie, zły serwis, odwołany token.
	broken := strings.Replace(cb, "epc1.", "epc1.X", 1)
	e.WriteFile(h.ClientB, "/run/ep/broken", broken, "600")
	out1, _ := e.Sh(h.ClientB, `ENTRYPOINT_CONNECT="$(cat /run/ep/broken)" /app/node_modules/@k18k/entrypoint-linux-x64/bin/entrypoint-connector tunnel --upstream http://127.0.0.1:1 2>&1`)
	e.WriteFile(h.ClientB, "/run/ep/b", cb, "600")
	out2, _ := e.Sh(h.ClientB, `ENTRYPOINT_CONNECT="$(cat /run/ep/b)" timeout 5 /app/node_modules/@k18k/entrypoint-linux-x64/bin/entrypoint-connector tunnel --service inny --upstream http://127.0.0.1:1 2>&1`)
	tokB := h.TokenFromBundle(cb)
	e.WriteFile(h.ClientB, "/run/ep/badtok", strings.Replace(tokB, "_", "-", 1), "600")
	out3, _ := e.Sh(h.ClientB, `ENTRYPOINT_CONNECT="$(cat /run/ep/badtok)" /app/node_modules/@k18k/entrypoint-linux-x64/bin/entrypoint-connector tunnel --upstream http://127.0.0.1:1 2>&1`)
	e.MustEP("token", "revoke", id)
	time.Sleep(15 * time.Second)
	// Procesy klienta: token nie występuje w argumentach.
	ps := e.MustSh(h.ClientA, "ps -eo args")
	// Zbierz wszystkie logi: CLI, konektor, frpc, journald bramy, docker logs.
	cliOut := out1 + out2 + out3
	var all strings.Builder
	all.WriteString(cliOut + ps)
	for _, u := range []string{"entrypointd", "entrypoint-caddy", "entrypoint-frps"} {
		all.WriteString(e.Journal(h.Gateway, u))
	}
	for _, c := range []string{h.ClientA, h.ClientB} {
		s, _ := e.Sh(c, "cat /tmp/*.log 2>/dev/null")
		all.WriteString(s)
	}
	for _, c := range []string{h.Gateway, h.ClientA, h.ClientB, h.ACME, h.DNS} {
		s, _ := h.Run(nil, "docker", "logs", c)
		all.WriteString(s)
	}
	text := all.String()
	caKey := strings.TrimSpace(e.MustExec(h.Gateway, "cat", "/var/lib/entrypoint/ca/ca.key"))
	frpsPw := strings.TrimSpace(e.MustExec(h.Gateway, "cat", "/var/lib/entrypoint/secrets/frps-api"))
	secrets := append([]string{}, e.Secrets...)
	keyBody := strings.Split(caKey, "\n")
	if len(keyBody) > 2 {
		secrets = append(secrets, keyBody[1])
	}
	secrets = append(secrets, frpsPw)
	found := 0
	for _, s := range secrets {
		if s == "" {
			continue
		}
		// Sam sekret tokenu (bez jawnego identyfikatora) też nie może wystąpić.
		if strings.HasPrefix(s, "ept_") {
			parts := strings.SplitN(s, "_", 3)
			s = parts[2]
		}
		if strings.Contains(text, s) {
			found++
			t.Errorf("sekret znaleziony w logach (długość %d)", len(s))
		}
	}
	_ = os.WriteFile(filepath.Join(h.Repo(), "e2e", "artifacts", "secrets-scan.txt"), []byte(text), 0o600)
	result(t, "przeszukano %d KB logów (CLI, konektor, frpc, journald, docker); znalezione sekrety: %d; komunikaty błędów: %q | %q | %q",
		len(text)/1024, found, firstLine(out1), firstLine(out2), firstLine(out3))
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

func TestSecrets_FilePermissions(t *testing.T) {
	e := apiSetup(t)
	e.Token("api")
	out := e.MustSh(h.Gateway, `stat -c '%n %a %U:%G' /var/lib/entrypoint/ca/ca.key /var/lib/entrypoint/ca /var/lib/entrypoint/tunnel/server.key /var/lib/entrypoint/frps/frps.json /var/lib/entrypoint/secrets/frps-api /var/lib/entrypoint/db/entrypoint.db /var/lib/entrypoint/db /var/lib/entrypoint/state/authz.json /var/lib/entrypoint/caddy/caddy.json /var/lib/entrypoint/caddy/storage /etc/entrypoint/entrypoint.json`)
	want := map[string]string{
		"/var/lib/entrypoint/ca/ca.key":         "600 entrypoint:entrypoint",
		"/var/lib/entrypoint/ca":                "700 entrypoint:entrypoint",
		"/var/lib/entrypoint/tunnel/server.key": "640 entrypoint:entrypoint-frps",
		"/var/lib/entrypoint/frps/frps.json":    "640 root:entrypoint-frps",
		"/var/lib/entrypoint/secrets/frps-api":  "640 root:entrypoint",
		"/var/lib/entrypoint/db/entrypoint.db":  "600 entrypoint:entrypoint",
		"/var/lib/entrypoint/db":                "700 entrypoint:entrypoint",
		"/var/lib/entrypoint/state/authz.json":  "600 entrypoint:entrypoint",
		"/var/lib/entrypoint/caddy/caddy.json":  "640 entrypoint:entrypoint-caddy",
		"/var/lib/entrypoint/caddy/storage":     "700 entrypoint-caddy:entrypoint-caddy",
		"/etc/entrypoint/entrypoint.json":       "644 root:root",
	}
	got := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.SplitN(l, " ", 2)
		got[f[0]] = f[1]
	}
	for p, w := range want {
		if got[p] != w {
			t.Errorf("%s: %s, oczekiwano %s", p, got[p], w)
		}
	}
	// Konfiguracja startowa nie zawiera sekretów.
	cfg := e.MustExec(h.Gateway, "cat", "/etc/entrypoint/entrypoint.json")
	if strings.Contains(cfg, "password") || strings.Contains(cfg, "ept_") {
		t.Error("sekret w /etc/entrypoint/entrypoint.json")
	}
	// Procesy działają jako osobni użytkownicy.
	users := e.MustSh(h.Gateway, "ps -eo user:20,comm | grep -E 'entrypointd|caddy|frps' | sort -u | tr '\\n' ';'")
	result(t, "prawa plików: %s; procesy: %s", strings.ReplaceAll(strings.TrimSpace(out), "\n", "; "), users)
	for _, u := range []string{"entrypoint ", "entrypoint-caddy", "entrypoint-frps"} {
		if !strings.Contains(users, u) {
			t.Errorf("brak procesu użytkownika %q", u)
		}
	}
}
