package tests

// e2e suite 8 (v0.2.0 part) and tests 3.4 and 3.5: `entrypoint update` with maintenance mode,
// signed releases and automatic rollback. Test releases (version 9.9.9) are built by
// scripts/e2e-update-releases.sh: good, with a broken migration, and one that crashes after start.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	h "github.com/k18k/entrypoint/e2e/harness"
)

const newVersion = "9.9.9"

type updEnv struct {
	*h.Env
	revokeID string
}

func updateSetup(t *testing.T) *updEnv {
	e := h.New(t)
	e.StopAllApps()
	t.Cleanup(e.StopAllApps)
	key, err := os.ReadFile(filepath.Join(h.Repo(), "dist", "update", "test.pub"))
	if err != nil {
		t.Fatal(err)
	}
	e.SetDNS(map[string]string{"api.ep.test": h.GatewayIP})
	e.ResetGateway(h.Gateway)
	e.Install(h.InstallOptions{Extra: []string{"--test-release-key", strings.TrimSpace(string(key))}})
	e.Service("api", "--health", "/health")
	e.Route("api.ep.test", "--service", "api")
	id, _ := e.Token("api")
	_, connect := e.Token("api")
	e.StartApp(h.AppOptions{Container: h.ClientA, Connect: connect, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	return &updEnv{e, id}
}

type gwStatus struct {
	Status struct {
		Version     string `json:"version"`
		Maintenance *struct {
			Reason string `json:"reason"`
		} `json:"maintenance"`
	} `json:"status"`
}

func (e *updEnv) status() gwStatus {
	var s gwStatus
	out, err := e.EP("status", "--json")
	if err == nil || out != "" {
		_ = jsonUnmarshal(out, &s)
	}
	return s
}

// startUpdate runs the update in the background; wait() returns its exit code and output.
func (e *updEnv) startUpdate(variant string, extra string) (wait func() (int, string)) {
	e.MustSh(h.Gateway, "rm -f /tmp/update.rc; (entrypoint update --from /dist/update/"+variant+" --health-time 10s "+extra+" > /tmp/update.log 2>&1; echo $? > /tmp/update.rc) > /dev/null 2>&1 &")
	return func() (int, string) {
		var rc string
		e.Eventually(5*time.Minute, time.Second, "update finished", func() error {
			out, err := e.Exec(h.Gateway, "cat", "/tmp/update.rc")
			rc = strings.TrimSpace(out)
			return err
		})
		log, _ := e.Exec(h.Gateway, "cat", "/tmp/update.log")
		n := 0
		for _, c := range rc {
			n = n*10 + int(c-'0')
		}
		return n, log
	}
}

func (e *updEnv) revoked(id string) bool {
	var toks []struct {
		ID      string `json:"id"`
		Revoked bool   `json:"revoked"`
	}
	e.EPJSON(&toks, "token", "list", "--json")
	for _, t := range toks {
		if t.ID == id {
			return t.Revoked
		}
	}
	return false
}

// revokeWhen revokes the token as soon as cond holds (polling the gateway).
func (e *updEnv) revokeWhen(what string, cond func(gwStatus) bool) {
	e.Eventually(2*time.Minute, 100*time.Millisecond, what, func() error {
		if !cond(e.status()) {
			return errors.New("not yet")
		}
		_, err := e.EP("token", "revoke", e.revokeID)
		return err
	})
}

func (e *updEnv) afterChecks() (string, bool, bool) {
	st := e.status()
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	_, err := e.EP("service", "add", "after-update")
	return st.Status.Version, st.Status.Maintenance == nil, err == nil
}

func TestUpdate_Success(t *testing.T) {
	e := updateSetup(t)
	wait := e.startUpdate("good", "--test-pause 5s")
	var blocked error
	e.revokeWhen("maintenance mode on", func(s gwStatus) bool {
		if s.Status.Maintenance == nil {
			return false
		}
		_, blocked = e.EP("service", "add", "during-update")
		return true
	})
	rc, log := wait()
	ver, off, changes := e.afterChecks()
	result(t, "update to %s: exit %d; change during maintenance refused: %v; revocation during it kept: %v; version after %s, maintenance off %v, tunnel back, changes work %v",
		newVersion, rc, blocked != nil && strings.Contains(blocked.Error(), "maintenance"), e.revoked(e.revokeID), ver, off, changes)
	if rc != 0 || ver != newVersion || !off || !changes || blocked == nil || !e.revoked(e.revokeID) {
		t.Fatalf("update failed:\n%s", log)
	}
}

// Test 3.4 and suite 8: a broken migration rolls back; a revocation from maintenance mode is kept.
func TestUpdate_BrokenMigrationRollsBack(t *testing.T) {
	e := updateSetup(t)
	wait := e.startUpdate("migration", "--test-pause 5s")
	e.revokeWhen("maintenance mode on", func(s gwStatus) bool { return s.Status.Maintenance != nil })
	rc, log := wait()
	ver, off, changes := e.afterChecks()
	result(t, "broken migration: exit %d, rolled back: %v; version after %s, maintenance off %v, revocation kept %v, tunnel back, changes work %v",
		rc, strings.Contains(log, "Rolled back"), ver, off, e.revoked(e.revokeID), changes)
	if rc == 0 || ver != h.Version || !off || !changes || !e.revoked(e.revokeID) {
		t.Fatalf("rollback after a broken migration failed:\n%s", log)
	}
}

// Test 3.5 and suite 8: the migration succeeds, the new version fails its health check; the
// rollback restores the old database and keeps a revocation made while the new version ran.
func TestUpdate_FailedHealthRollsBack(t *testing.T) {
	e := updateSetup(t)
	wait := e.startUpdate("health", "")
	e.revokeWhen("the new version answers", func(s gwStatus) bool { return s.Status.Version == newVersion })
	rc, log := wait()
	ver, off, changes := e.afterChecks()
	schema := strings.TrimSpace(e.MustExec(h.Gateway, "sqlite3", "/var/lib/entrypoint/db/entrypoint.db", "PRAGMA user_version"))
	result(t, "new version crashing after start: exit %d, rolled back: %v; version after %s, schema %s, maintenance off %v, revocation made under the new version kept %v, tunnel back, changes work %v",
		rc, strings.Contains(log, "Rolled back"), ver, schema, off, e.revoked(e.revokeID), changes)
	if rc == 0 || ver != h.Version || schema != "1" || !off || !changes || !e.revoked(e.revokeID) {
		t.Fatalf("rollback after a failed health check failed:\n%s", log)
	}
}
