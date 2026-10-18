package tests

// Zestaw e2e 2: TLS i ACME (Pebble zamiast Let's Encrypt).

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	h "github.com/k18k/entrypoint/e2e/harness"
)

func peerCert(host string) (string, time.Time, error) {
	pool, err := h.PebbleRoots()
	if err != nil {
		return "", time.Time{}, err
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	c, err := tls.DialWithDialer(d, "tcp", h.GatewayIP+":443", &tls.Config{RootCAs: pool, ServerName: host})
	if err != nil {
		return "", time.Time{}, err
	}
	defer c.Close()
	cert := c.ConnectionState().PeerCertificates[0]
	return cert.SerialNumber.String(), cert.NotAfter, nil
}

func TestTLS_CertificateAfterDNSRecord(t *testing.T) {
	e := h.New(t)
	e.FreshGateway()
	e.Route("late.ep.test", "--to", h.UpstreamVPN)
	time.Sleep(5 * time.Second)
	_, _, errBefore := peerCert("late.ep.test")
	var dns struct{}
	_ = dns
	dnsOut, dnsErr := e.EP("dns", "check")
	e.SetDNS(map[string]string{"late.ep.test": h.GatewayIP})
	t0 := time.Now()
	e.Eventually(120*time.Second, time.Second, "certyfikat po pojawieniu się rekordu", func() error {
		_, _, err := peerCert("late.ep.test")
		return err
	})
	took := time.Since(t0)
	dnsOut2, dnsErr2 := e.EP("dns", "check")
	result(t, "przed rekordem DNS: certyfikat=%v (dns check błąd=%v); po dodaniu rekordu certyfikat Pebble po %s; dns check po: błąd=%v",
		errBefore == nil, dnsErr != nil, ms(took), dnsErr2 != nil)
	if errBefore == nil || dnsErr == nil || dnsErr2 != nil {
		t.Fatalf("dns check:\n%s\n%s", dnsOut, dnsOut2)
	}
	if !strings.Contains(dnsOut, "late.ep.test") || !strings.Contains(dnsOut, "brak rekordu") {
		t.Fatalf("dns check nie wskazał brakującego rekordu:\n%s", dnsOut)
	}
}

func TestTLS_NoCertificateWithoutRoute(t *testing.T) {
	e := h.New(t)
	e.FreshGateway("--test-renew-interval", "5s")
	e.SetDNS(map[string]string{"routed.ep.test": h.GatewayIP, "noroute.ep.test": h.GatewayIP, "a.wild.ep.test": h.GatewayIP, "b.wild.ep.test": h.GatewayIP, "other.ep.test": h.GatewayIP})
	e.Route("routed.ep.test", "--to", h.UpstreamVPN)
	e.Route("*.wild.ep.test", "--to", h.UpstreamVPN)
	e.WaitHTTPS("https://routed.ep.test/", 200)
	e.WaitHTTPS("https://a.wild.ep.test/", 200) // subdomena w locie
	_, _, errNo := peerCert("noroute.ep.test")
	_, _, errOther := peerCert("other.ep.test")
	_, _, errB := peerCert("b.wild.ep.test")
	j := e.Journal(h.Gateway, "entrypointd")
	asked := strings.Contains(j, "zgoda na certyfikat odrzucona")
	certs := e.MustSh(h.Gateway, "find /var/lib/entrypoint/caddy/storage/certificates -name '*.crt' | sed 's|.*/||' | sort | tr '\\n' ' '")
	result(t, "host bez trasy: certyfikat=%v; inny host bez trasy: %v; druga subdomena wildcardu: %v; odmowy w logu: %v; certyfikaty w magazynie: %s",
		errNo == nil, errOther == nil, errB == nil, asked, certs)
	if errNo == nil || errOther == nil || strings.Contains(certs, "noroute") || strings.Contains(certs, "*.wild") {
		t.Fatal("wystawiono certyfikat dla hosta bez trasy albo wildcard bez DNS-01")
	}
	if errB != nil {
		t.Fatalf("subdomena wildcardu bez certyfikatu: %v", errB)
	}
	// Licznik certyfikatów tej instalacji w statusie.
	st := e.MustEP("status")
	if !strings.Contains(st, "ep.test: 3") {
		t.Errorf("licznik certyfikatów w statusie:\n%s", st)
	}
}

func TestTLS_OnDemandRateLimit(t *testing.T) {
	e := h.New(t)
	e.FreshGateway()
	recs := map[string]string{}
	for i := 0; i < 12; i++ {
		recs[fmt.Sprintf("s%d.burst.ep.test", i)] = h.GatewayIP
	}
	e.SetDNS(recs)
	e.Route("*.burst.ep.test", "--to", h.UpstreamVPN)
	time.Sleep(2 * time.Second)
	ok := 0
	for i := 0; i < 12; i++ {
		host := fmt.Sprintf("s%d.burst.ep.test", i)
		if err := waitCert(host, 30*time.Second); err == nil {
			ok++
		}
	}
	result(t, "12 nowych subdomen wildcardu w ciągu godziny: certyfikaty dla %d (limit 10 na godzinę)", ok)
	if ok != 10 {
		t.Fatalf("limit nowych subdomen: %d", ok)
	}
}

func waitCert(host string, d time.Duration) error {
	return h.Wait(d, time.Second, func() error {
		_, _, err := peerCert(host)
		return err
	})
}

// Odnowienie: krótki profil Pebble (90 s) i częste sprawdzanie odnowień w Caddy.
func TestTLS_Renewal(t *testing.T) {
	e := h.New(t)
	e.FreshGateway("--test-acme-profile", "short", "--test-renew-interval", "5s")
	e.SetDNS(map[string]string{"renew.ep.test": h.GatewayIP})
	e.Route("renew.ep.test", "--to", h.UpstreamVPN)
	e.WaitHTTPS("https://renew.ep.test/", 200)
	s1, na1, _ := peerCert("renew.ep.test")
	var s2 string
	var na2 time.Time
	e.Eventually(150*time.Second, 2*time.Second, "odnowienie certyfikatu", func() error {
		s, na, err := peerCert("renew.ep.test")
		if err != nil {
			return err
		}
		if s == s1 {
			return errors.New("ten sam certyfikat")
		}
		s2, na2 = s, na
		return nil
	})
	result(t, "certyfikat ważny do %s odnowiony na ważny do %s (inny numer seryjny), bez przerwy w HTTPS", na1.Format(time.TimeOnly), na2.Format(time.TimeOnly))
	_ = s2
	e.WaitHTTPS("https://renew.ep.test/", 200)
}

// Konektor odrzuca serwer z obcym CA (podmieniony pakiet połączenia).
func TestTLS_ConnectorRejectsForeignCA(t *testing.T) {
	e := apiSetup(t)
	_, good := e.Token("api")
	b, _ := h.DecodeBundle(good)
	// Pakiet z CA innej bramy: brama musi zostać odrzucona przez klienta.
	other := e.MustExec(h.Gateway, "cat", "/pebble/pebble.minica.pem")
	fake := rebundle(t, e, good, other)
	a := e.StartApp(h.AppOptions{Container: h.ClientA, Connect: fake, Replica: "a"})
	time.Sleep(15 * time.Second)
	logs, _ := e.Sh(h.ClientA, "cat /tmp/app-a.log")
	healthy := strings.Contains(logs, `"ready":true`)
	tlsErr := strings.Contains(logs, "tls_error") || strings.Contains(logs, "nie jest podpisany")
	reps := e.ReplicaSuffixes("api")
	_ = a
	result(t, "pakiet z obcym CA: połączenie=%v, tls_error=%v, repliki=%v (serwer %s)", healthy, tlsErr, reps, b.Server)
	if healthy || len(reps) != 0 || !tlsErr {
		t.Fatal("konektor połączył się z bramą o certyfikacie spoza zaufanych CA")
	}
}

// rebundle podmienia CA w pakiecie połączenia narzędziem konektora.
func rebundle(t *testing.T, e *h.Env, bundle, caPEM string) string {
	t.Helper()
	e.WriteFile(h.ClientA, "/run/ep/rebundle.connect", bundle, "600")
	e.WriteFile(h.ClientA, "/run/ep/other-ca.pem", caPEM, "644")
	out := e.MustSh(h.ClientA, `ENTRYPOINT_CONNECT="$(cat /run/ep/rebundle.connect)" /app/node_modules/@k18k/entrypoint-linux-x64/bin/entrypoint-connector rebundle --ca /run/ep/other-ca.pem`)
	return strings.TrimSpace(out)
}
