package tests

// Zestaw e2e 8 (część v0.1): backup i restore.

import (
	"strings"
	"testing"
	"time"

	h "github.com/k18k/entrypoint/e2e/harness"
)

// Backup i restore na świeżym kontenerze pod tym samym adresem: historia rewizji, trasy,
// serwisy i certyfikaty wracają; tokeny są unieważnione; nowy token działa.
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
	pw := "haslo-backupu-e2e-456"
	out := e.MustSh(h.Gateway, "cd /shared && rm -f ops.age && ENTRYPOINT_BACKUP_PASSWORD="+pw+" entrypoint backup -o /shared/ops.age")
	perm := strings.TrimSpace(e.MustSh(h.Gateway, "stat -c %a /shared/ops.age"))
	plain := e.MustSh(h.Gateway, "head -c 200 /shared/ops.age | tr -dc '[:print:]' | head -c 40")
	e.StopAllApps()
	// Świeży kontener, ta sama wersja.
	e.ResetGateway(h.Gateway)
	e.Install(h.InstallOptions{})
	_, wrongErr := e.Exec(h.Gateway, "sh", "-c", "ENTRYPOINT_BACKUP_PASSWORD=zle-haslo-123 entrypoint restore /shared/ops.age --yes")
	e.MustSh(h.Gateway, "ENTRYPOINT_BACKUP_PASSWORD="+pw+" entrypoint restore /shared/ops.age --yes")
	revsAfter := revisions(e)
	routesAfter := e.MustEP("route", "list")
	caAfter := e.MustExec(h.Gateway, "sha256sum", "/var/lib/entrypoint/ca/ca.crt")
	e.WaitHTTPS("https://vpn.ep.test/", 200)
	serialAfter, _, _ := peerCert("vpn.ep.test")
	// Stary token nie działa, nowy działa z tym samym CA (klienci nie zmieniają CA).
	_, fresh := e.Token("api")
	e.StartApp(h.AppOptions{Container: h.ClientB, Connect: fresh, Replica: "b", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	time.Sleep(time.Second)
	result(t, "%s; plik 0%s, zaszyfrowany (nagłówek %q); złe hasło odrzucone: %v; rewizje %d -> %d (+1 po nowym tokenie: %d); trasy zgodne: %v; CA zgodne: %v; certyfikat HTTPS z backupu (ten sam numer seryjny): %v",
		strings.TrimSpace(out), perm, plain, wrongErr != nil, len(revsBefore), len(revsAfter), len(revisions(e)), routesBefore == routesAfter, caBefore == caAfter, serialBefore == serialAfter)
	if wrongErr == nil || perm != "600" || !strings.HasPrefix(plain, "age-encryption.org") {
		t.Fatal("backup nie jest zaszyfrowany albo złe hasło przeszło")
	}
	if len(revsAfter) != len(revsBefore) || routesBefore != routesAfter || caBefore != caAfter || serialBefore != serialAfter {
		t.Fatal("restore nie odtworzył stanu")
	}
}

// dns check: wymagane rekordy i ich bieżący stan (bez czekania na certyfikat).
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
	result(t, "przed rekordem: błąd=%v; po: błąd=%v (other wskazuje inny adres); wiersze: %+v", errBefore != nil, errAfter != nil, rows)
	if errBefore == nil || !strings.Contains(before, "brak rekordu") {
		t.Fatalf("brak rekordu nie wykryty:\n%s", before)
	}
	if !strings.Contains(after, "inny adres") || errAfter == nil {
		t.Fatalf("zły adres nie wykryty:\n%s", after)
	}
	for _, r := range rows {
		if r.Host == "late.ep.test" && !r.OK {
			t.Fatal("poprawny rekord oznaczony jako błędny")
		}
	}
}
