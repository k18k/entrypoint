package tests

// Zestaw e2e 6: stan konfiguracji (rewizje, rollback, awaria w trakcie stosowania, dryf).

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	h "github.com/k18k/entrypoint/e2e/harness"
)

type revision struct {
	Num      int64
	Status   string
	Error    string
	Attempts int
	Summary  string
}

func revisions(e *h.Env) []revision {
	var r []revision
	e.EPJSON(&r, "revision", "list")
	return r
}

// tryRevisions toleruje chwilową niedostępność API (restart demona).
func tryRevisions(e *h.Env) ([]revision, error) {
	out, err := e.EP("revision", "list", "--json")
	if err != nil {
		return nil, err
	}
	var r []revision
	if err := jsonUnmarshal(out, &r); err != nil || len(r) == 0 {
		return nil, fmt.Errorf("brak rewizji: %v", err)
	}
	return r, nil
}

func caddyConfigHash(e *h.Env) string {
	return strings.TrimSpace(e.MustSh(h.Gateway, "curl -s --unix-socket /run/entrypoint-caddy/admin.sock http://caddy/config/ | sha256sum | cut -c1-16"))
}

// Błąd Caddy po kroku autoryzacji: rewizja failed, snapshot i Caddy wracają do poprzedniej.
func TestState_CaddyFailureAfterAuthz(t *testing.T) {
	e := h.New(t)
	e.FreshGateway()
	e.SetDNS(map[string]string{"s.ep.test": h.GatewayIP})
	e.Route("s.ep.test", "--to", h.UpstreamVPN)
	e.WaitHTTPS("https://s.ep.test/", 200)
	before := revisions(e)[0]
	e.MustExec(h.Gateway, "systemctl", "stop", "entrypoint-caddy")
	_, err := e.EP("service", "add", "nowy")
	e.MustExec(h.Gateway, "systemctl", "start", "entrypoint-caddy")
	r := revisions(e)[0]
	st := e.Status()
	e.WaitHTTPS("https://s.ep.test/", 200)
	// Następna zmiana opiera się na ostatniej udanej rewizji i się udaje.
	_, err2 := e.EP("service", "add", "nowy")
	r2 := revisions(e)[0]
	result(t, "zmiana przy zatrzymanym Caddy: CLI błąd=%v, rewizja %d status %s; snapshot = rewizja %d; ostrzeżenia: %v; ponowna zmiana: rewizja %d %s",
		err != nil, r.Num, r.Status, st.Status.AuthzRevision, st.Status.Warnings, r2.Num, r2.Status)
	if err == nil || r.Status != "failed" || st.Status.AuthzRevision != before.Num || err2 != nil || r2.Status != "applied" {
		t.Fatal("rollback po błędzie Caddy nie działa")
	}
}

// Zabicie entrypointd w trakcie stosowania: rewizja ponawiana raz, a przy drugiej awarii wycofywana.
func TestState_KillDuringApply(t *testing.T) {
	e := h.New(t)
	e.FreshGateway("--test-apply-delay", "6s")
	start := func(name string) {
		go e.EP("service", "add", name)
		e.Eventually(10*time.Second, 200*time.Millisecond, "rewizja w stanie applying", func() error {
			r, err := tryRevisions(e)
			if err != nil {
				return err
			}
			if r[0].Status != "applying" || r[0].Summary == "" || !strings.Contains(r[0].Summary, name) {
				return errors.New("jeszcze nie")
			}
			return nil
		})
	}
	waitStatus := func(want string) revision {
		var r revision
		e.Eventually(60*time.Second, 300*time.Millisecond, "rewizja "+want, func() error {
			rs, err := tryRevisions(e)
			if err != nil {
				return err
			}
			r = rs[0]
			if r.Status != want {
				return fmt.Errorf("status %s", r.Status)
			}
			return nil
		})
		return r
	}
	// Jedna awaria: po restarcie rewizja jest ponawiana i stosowana.
	start("raz")
	e.MustExec(h.Gateway, "systemctl", "kill", "-s", "KILL", "entrypointd")
	r1 := waitStatus("applied")
	// Dwie awarie: po drugim restarcie rewizja jest wycofywana.
	start("dwa")
	e.MustExec(h.Gateway, "systemctl", "kill", "-s", "KILL", "entrypointd")
	e.Eventually(30*time.Second, 200*time.Millisecond, "ponowienie w toku", func() error {
		rs, err := tryRevisions(e)
		if err != nil {
			return err
		}
		r := rs[0]
		if r.Status != "applying" || r.Attempts < 2 {
			return fmt.Errorf("%s/%d", r.Status, r.Attempts)
		}
		return nil
	})
	e.MustExec(h.Gateway, "systemctl", "kill", "-s", "KILL", "entrypointd")
	r2 := waitStatus("failed")
	var svcs []struct{ Name string }
	e.EPJSON(&svcs, "service", "list")
	st := e.Status()
	result(t, "jedna awaria: rewizja %d %s po %d próbach; dwie awarie: rewizja %d %s (%s); serwisy: %v; snapshot = rewizja %d",
		r1.Num, r1.Status, r1.Attempts, r2.Num, r2.Status, r2.Error, svcs, st.Status.AuthzRevision)
	if r1.Attempts != 2 || len(svcs) != 1 || svcs[0].Name != "raz" || st.Status.AuthzRevision != r1.Num {
		t.Fatal("odtwarzanie po awarii w trakcie stosowania nie spełnia sekcji 4")
	}
}

// /load jest atomowe: błędna konfiguracja zostaje odrzucona, Caddy zostaje przy poprzedniej.
func TestState_CaddyLoadAtomic(t *testing.T) {
	e := h.New(t)
	e.FreshGateway()
	e.SetDNS(map[string]string{"s.ep.test": h.GatewayIP})
	e.Route("s.ep.test", "--to", h.UpstreamVPN)
	e.WaitHTTPS("https://s.ep.test/", 200)
	before := caddyConfigHash(e)
	bad := `{"admin":{"listen":"unix//run/entrypoint-caddy/admin.sock|0660"},"apps":{"http":{"servers":{"x":{"listen":[":443"],"routes":[{"handle":[{"handler":"nie_ma_takiego_modulu"}]}]}}}}}`
	e.WriteFile(h.Gateway, "/tmp/bad.json", bad, "644")
	out, _ := e.Sh(h.Gateway, "curl -s -o /dev/null -w '%{http_code}' --unix-socket /run/entrypoint-caddy/admin.sock -H 'Content-Type: application/json' --data @/tmp/bad.json http://caddy/load")
	after := caddyConfigHash(e)
	e.WaitHTTPS("https://s.ep.test/", 200)
	// Walidacja przed zapisem: Caddy odrzuca wyrenderowaną konfigurację, rewizja nie powstaje.
	revBefore := revisions(e)[0].Num
	_, err := e.EP("route", "add", "s.ep.test/v", "--to", "172.29.0.20:0")
	revAfter := revisions(e)[0].Num
	result(t, "błędne /load: kod %s, konfiguracja bez zmian: %v; błędny upstream w CLI odrzucony przed zapisem: %v, nowa rewizja: %v", out, before == after, err != nil, revAfter != revBefore)
	if before != after || out == "200" || err == nil || revAfter != revBefore {
		t.Fatal("atomowość /load albo walidacja przed zapisem nie działa")
	}
}

// Dryf: zmiana konfiguracji Caddy z zewnątrz jest wykrywana i cofana.
func TestState_Drift(t *testing.T) {
	e := h.New(t)
	e.FreshGateway()
	e.SetDNS(map[string]string{"s.ep.test": h.GatewayIP})
	e.Route("s.ep.test", "--to", h.UpstreamVPN)
	e.WaitHTTPS("https://s.ep.test/", 200)
	want := caddyConfigHash(e)
	// Pętla dryfu może cofnąć zmianę, zanim ją zobaczymy, więc sprawdzamy kod odpowiedzi i wpis w logu.
	code := e.MustSh(h.Gateway, `curl -s -o /dev/null -w '%{http_code}' --unix-socket /run/entrypoint-caddy/admin.sock -X DELETE http://caddy/config/apps/http/servers/entrypoint/routes/0`)
	if strings.TrimSpace(code) != "200" {
		t.Fatalf("zmiana z zewnątrz się nie powiodła: %s", code)
	}
	t0 := time.Now()
	e.Eventually(30*time.Second, 250*time.Millisecond, "przywrócenie konfiguracji", func() error {
		if caddyConfigHash(e) != want {
			return errors.New("dryf trwa")
		}
		return nil
	})
	var logged bool
	e.Eventually(15*time.Second, 250*time.Millisecond, "wpis o dryfie", func() error {
		if logged = strings.Contains(e.Journal(h.Gateway, "entrypointd"), "wykryto dryf"); !logged {
			return errors.New("brak wpisu")
		}
		return nil
	})
	took := time.Since(t0)
	result(t, "dryf cofnięty po %s (interwał 5 s w testach, domyślnie 30 s); wpis w logu: %v", ms(took), logged)
	if !logged {
		t.Fatal("brak wpisu o dryfie")
	}
}

// entrypointd startuje przed Caddy (pierwszy start na wolnej maszynie): rewizja początkowa
// czeka na Caddy zamiast kończyć się błędem.
func TestState_DaemonStartsBeforeCaddy(t *testing.T) {
	e := h.New(t)
	e.SetDNS(map[string]string{"s.ep.test": h.GatewayIP})
	e.ResetGateway(h.Gateway)
	e.InstallPrograms(h.Gateway)
	e.MustExec(h.Gateway, "entrypoint", "install", "--gateway-name", h.TunnelName, "--acme-ca", "https://pebble:14000/dir",
		"--acme-staging-ca", "https://pebble:14000/dir", "--acme-ca-root", "/pebble/pebble.minica.pem", "--public-ip", h.GatewayIP, "--no-start")
	e.MustExec(h.Gateway, "systemctl", "start", "entrypointd.socket", "entrypointd.service")
	time.Sleep(5 * time.Second)
	e.MustExec(h.Gateway, "systemctl", "start", "entrypoint-caddy", "entrypoint-frps")
	var revs []revision
	e.Eventually(30*time.Second, 500*time.Millisecond, "rewizja początkowa zastosowana", func() error {
		r, err := tryRevisions(e)
		if err != nil {
			return err
		}
		revs = r
		if r[0].Status != "applied" {
			return fmt.Errorf("status %s", r[0].Status)
		}
		return nil
	})
	e.Route("s.ep.test", "--to", h.UpstreamVPN)
	e.WaitHTTPS("https://s.ep.test/", 200)
	st := e.Status()
	result(t, "demon 5 s przed Caddy: rewizje %v; ostrzeżenia %v", revs, st.Status.Warnings)
	for _, r := range revs {
		if r.Status == "failed" {
			t.Fatal("rewizja początkowa zakończona błędem")
		}
	}
}
