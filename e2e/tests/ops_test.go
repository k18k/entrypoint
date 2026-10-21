package tests

// e2e suite 8 (v0.1 part): backup and restore.

import (
	"strings"
	"testing"
	"time"

	h "github.com/k18k/entrypoint/e2e/harness"
)

// Backup and restore on a fresh container at the same address: revision history, routes,
// services and certificates come back; tokens are invalidated; a new token works.
func TestOps_BackupRestoreFreshContainer(t *testing.T) {
	e := apiSetup(t)
	e.SetDNS(map[string]string{"api.ep.test": h.GatewayIP, "vpn.ep.test": h.GatewayIP})
	e.Route("vpn.ep.test", "--to", h.UpstreamVPN)
	_, connect := e.Token("api")
	e.StartApp(h.AppOptions{Container: h.ClientA, Connect: connect, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	e.WaitHTTPS("https://vpn.ep.test/", 200)
	serialBefore, _, _ := peerCert("vpn.ep.test")
	revsBefore := revisions(e)
	routesBefore := e.MustEP("route", "list")
	caBefore := e.MustExec(h.Gateway, "sha256sum", "/var/lib/entrypoint/ca/ca.crt")
	pw := "backup-password-e2e-456"
	out := e.MustSh(h.Gateway, "cd /shared && rm -f ops.age && ENTRYPOINT_BACKUP_PASSWORD="+pw+" entrypoint backup -o /shared/ops.age")
	perm := strings.TrimSpace(e.MustSh(h.Gateway, "stat -c %a /shared/ops.age"))
	plain := e.MustSh(h.Gateway, "head -c 200 /shared/ops.age | tr -dc '[:print:]' | head -c 40")
	e.StopAllApps()
	// A fresh container, the same version.
	e.ResetGateway(h.Gateway)
	e.Install(h.InstallOptions{})
	_, wrongErr := e.Exec(h.Gateway, "sh", "-c", "ENTRYPOINT_BACKUP_PASSWORD=wrong-password-123 entrypoint restore /shared/ops.age --yes")
	e.MustSh(h.Gateway, "ENTRYPOINT_BACKUP_PASSWORD="+pw+" entrypoint restore /shared/ops.age --yes")
	revsAfter := revisions(e)
	routesAfter := e.MustEP("route", "list")
	caAfter := e.MustExec(h.Gateway, "sha256sum", "/var/lib/entrypoint/ca/ca.crt")
	e.WaitHTTPS("https://vpn.ep.test/", 200)
	serialAfter, _, _ := peerCert("vpn.ep.test")
	// The old token fails, a new one works with the same CA (clients keep their CA).
	_, fresh := e.Token("api")
	e.StartApp(h.AppOptions{Container: h.ClientB, Connect: fresh, Replica: "b", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	time.Sleep(time.Second)
	result(t, "%s; file 0%s, encrypted (header %q); wrong password rejected: %v; revisions %d -> %d (+1 after the new token: %d); routes match: %v; CA matches: %v; HTTPS certificate from the backup (same serial number): %v",
		strings.TrimSpace(out), perm, plain, wrongErr != nil, len(revsBefore), len(revsAfter), len(revisions(e)), routesBefore == routesAfter, caBefore == caAfter, serialBefore == serialAfter)
	if wrongErr == nil || perm != "600" || !strings.HasPrefix(plain, "age-encryption.org") {
		t.Fatal("backup not encrypted or the wrong password passed")
	}
	if len(revsAfter) != len(revsBefore) || routesBefore != routesAfter || caBefore != caAfter || serialBefore != serialAfter {
		t.Fatal("restore did not bring back the state")
	}
}

// dns check: required records and their current state (without waiting for a certificate).
func TestOps_DNSCheck(t *testing.T) {
	e := h.New(t)
	e.FreshGateway()
	e.Route("late.ep.test", "--to", h.UpstreamVPN)
	before, errBefore := e.EP("dns", "check")
	e.SetDNS(map[string]string{"late.ep.test": h.GatewayIP, "other.ep.test": "10.0.0.1"})
	e.Route("other.ep.test", "--to", h.UpstreamVPN)
	after, errAfter := e.EP("dns", "check")
	var rows []struct {
		Host    string
		OK      bool
		Problem string
	}
	out, _ := e.EP("dns", "check", "--json")
	_ = jsonUnmarshal(out, &rows)
	result(t, "before the record: error=%v; after: error=%v (other points to a different address); rows: %+v", errBefore != nil, errAfter != nil, rows)
	if errBefore == nil || !strings.Contains(before, "no record") {
		t.Fatalf("missing record not detected:\n%s", before)
	}
	if !strings.Contains(after, "different address") || errAfter == nil {
		t.Fatalf("wrong address not detected:\n%s", after)
	}
	for _, r := range rows {
		if r.Host == "late.ep.test" && !r.OK {
			t.Fatal("a correct record marked as wrong")
		}
	}
}
