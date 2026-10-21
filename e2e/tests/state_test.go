package tests

// e2e suite 6: configuration state (revisions, rollback, crash while applying, drift).

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

// tryRevisions tolerates the API being briefly unavailable (daemon restart).
func tryRevisions(e *h.Env) ([]revision, error) {
	out, err := e.EP("revision", "list", "--json")
	if err != nil {
		return nil, err
	}
	var r []revision
	if err := jsonUnmarshal(out, &r); err != nil || len(r) == 0 {
		return nil, fmt.Errorf("no revisions: %v", err)
	}
	return r, nil
}

func caddyConfigHash(e *h.Env) string {
	return strings.TrimSpace(e.MustSh(h.Gateway, "curl -s --unix-socket /run/entrypoint-caddy/admin.sock http://caddy/config/ | sha256sum | cut -c1-16"))
}

// A Caddy error after the authorization step: revision failed, snapshot and Caddy go back to the previous one.
func TestState_CaddyFailureAfterAuthz(t *testing.T) {
	e := h.New(t)
	e.FreshGateway()
	e.SetDNS(map[string]string{"s.ep.test": h.GatewayIP})
	e.Route("s.ep.test", "--to", h.UpstreamVPN)
	e.WaitHTTPS("https://s.ep.test/", 200)
	before := revisions(e)[0]
	e.MustExec(h.Gateway, "systemctl", "stop", "entrypoint-caddy")
	_, err := e.EP("service", "add", "fresh")
	e.MustExec(h.Gateway, "systemctl", "start", "entrypoint-caddy")
	r := revisions(e)[0]
	st := e.Status()
	e.WaitHTTPS("https://s.ep.test/", 200)
	// The next change builds on the last successful revision and succeeds.
	_, err2 := e.EP("service", "add", "fresh")
	r2 := revisions(e)[0]
	result(t, "change with Caddy stopped: CLI error=%v, revision %d status %s; snapshot = revision %d; warnings: %v; next change: revision %d %s",
		err != nil, r.Num, r.Status, st.Status.AuthzRevision, st.Status.Warnings, r2.Num, r2.Status)
	if err == nil || r.Status != "failed" || st.Status.AuthzRevision != before.Num || err2 != nil || r2.Status != "applied" {
		t.Fatal("rollback after a Caddy error does not work")
	}
}

// Killing entrypointd while applying: the revision is retried once and rolled back on the second crash.
func TestState_KillDuringApply(t *testing.T) {
	e := h.New(t)
	e.FreshGateway("--test-apply-delay", "6s")
	start := func(name string) {
		go e.EP("service", "add", name)
		e.Eventually(10*time.Second, 200*time.Millisecond, "revision in applying state", func() error {
			r, err := tryRevisions(e)
			if err != nil {
				return err
			}
			if r[0].Status != "applying" || r[0].Summary == "" || !strings.Contains(r[0].Summary, name) {
				return errors.New("not yet")
			}
			return nil
		})
	}
	waitStatus := func(want string) revision {
		var r revision
		e.Eventually(60*time.Second, 300*time.Millisecond, "revision "+want, func() error {
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
	// One crash: after the restart the revision is retried and applied.
	start("once")
	e.MustExec(h.Gateway, "systemctl", "kill", "-s", "KILL", "entrypointd")
	r1 := waitStatus("applied")
	// Two crashes: after the second restart the revision is rolled back.
	start("twice")
	e.MustExec(h.Gateway, "systemctl", "kill", "-s", "KILL", "entrypointd")
	e.Eventually(30*time.Second, 200*time.Millisecond, "retry in progress", func() error {
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
	result(t, "one crash: revision %d %s after %d attempts; two crashes: revision %d %s (%s); services: %v; snapshot = revision %d",
		r1.Num, r1.Status, r1.Attempts, r2.Num, r2.Status, r2.Error, svcs, st.Status.AuthzRevision)
	if r1.Attempts != 2 || len(svcs) != 1 || svcs[0].Name != "once" || st.Status.AuthzRevision != r1.Num {
		t.Fatal("recovery after a crash while applying does not meet section 4")
	}
}

// /load is atomic: an invalid configuration is rejected and Caddy keeps the previous one.
func TestState_CaddyLoadAtomic(t *testing.T) {
	h.Experiment(t)
	e := h.New(t)
	e.FreshGateway()
	e.SetDNS(map[string]string{"s.ep.test": h.GatewayIP})
	e.Route("s.ep.test", "--to", h.UpstreamVPN)
	e.WaitHTTPS("https://s.ep.test/", 200)
	before := caddyConfigHash(e)
	bad := `{"admin":{"listen":"unix//run/entrypoint-caddy/admin.sock|0660"},"apps":{"http":{"servers":{"x":{"listen":[":443"],"routes":[{"handle":[{"handler":"no_such_module"}]}]}}}}}`
	e.WriteFile(h.Gateway, "/tmp/bad.json", bad, "644")
	out, _ := e.Sh(h.Gateway, "curl -s -o /dev/null -w '%{http_code}' --unix-socket /run/entrypoint-caddy/admin.sock -H 'Content-Type: application/json' --data @/tmp/bad.json http://caddy/load")
	after := caddyConfigHash(e)
	e.WaitHTTPS("https://s.ep.test/", 200)
	// Validation before saving: Caddy rejects the rendered configuration, no revision is created.
	revBefore := revisions(e)[0].Num
	_, err := e.EP("route", "add", "s.ep.test/v", "--to", h.UpstreamIP+":0")
	revAfter := revisions(e)[0].Num
	result(t, "invalid /load: status %s, configuration unchanged: %v; invalid upstream in the CLI rejected before saving: %v, new revision: %v", out, before == after, err != nil, revAfter != revBefore)
	if before != after || out == "200" || err == nil || revAfter != revBefore {
		t.Fatal("/load atomicity or validation before saving does not work")
	}
}

// Drift: an outside change to Caddy's configuration is detected and reverted.
func TestState_Drift(t *testing.T) {
	e := h.New(t)
	e.FreshGateway()
	e.SetDNS(map[string]string{"s.ep.test": h.GatewayIP})
	e.Route("s.ep.test", "--to", h.UpstreamVPN)
	e.WaitHTTPS("https://s.ep.test/", 200)
	want := caddyConfigHash(e)
	// The drift loop may revert the change before we see it, so check the response status and the log entry.
	code := e.MustSh(h.Gateway, `curl -s -o /dev/null -w '%{http_code}' --unix-socket /run/entrypoint-caddy/admin.sock -X DELETE http://caddy/config/apps/http/servers/entrypoint/routes/0`)
	if strings.TrimSpace(code) != "200" {
		t.Fatalf("the outside change failed: %s", code)
	}
	t0 := time.Now()
	e.Eventually(30*time.Second, 250*time.Millisecond, "configuration restored", func() error {
		if caddyConfigHash(e) != want {
			return errors.New("drift persists")
		}
		return nil
	})
	var logged bool
	e.Eventually(15*time.Second, 250*time.Millisecond, "drift log entry", func() error {
		if logged = strings.Contains(e.Journal(h.Gateway, "entrypointd"), "drift detected"); !logged {
			return errors.New("no entry")
		}
		return nil
	})
	took := time.Since(t0)
	result(t, "drift reverted after %s (5 s interval in tests, 30 s by default); log entry: %v", ms(took), logged)
	if !logged {
		t.Fatal("no drift log entry")
	}
}

// entrypointd starts before Caddy (first start on a slow machine): the initial revision
// waits for Caddy instead of failing.
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
	e.Eventually(30*time.Second, 500*time.Millisecond, "initial revision applied", func() error {
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
	result(t, "daemon 5 s before Caddy: revisions %v; warnings %v", revs, st.Status.Warnings)
	for _, r := range revs {
		if r.Status == "failed" {
			t.Fatal("initial revision failed")
		}
	}
}
