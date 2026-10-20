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
	h.Experiment(t)
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
	dns := map[string]string{"routed.ep.test": h.GatewayIP, "noroute.ep.test": h.GatewayIP, "a.wild.ep.test": h.GatewayIP,
		"b.wild.ep.test": h.GatewayIP, "x.od.ep.test": h.GatewayIP}
	random := []string{}
	for i := 0; i < 10; i++ {
		n := fmt.Sprintf("rnd%d-%d.wild.ep.test", i, time.Now().UnixNano()%100000)
		random = append(random, n)
		dns[n] = h.GatewayIP
	}
	e.SetDNS(dns)
	e.Route("routed.ep.test", "--to", h.UpstreamVPN)
	// Issue #3: a wildcard route authorizes only explicit names; on-demand is an explicit opt-in.
	e.Route("*.wild.ep.test", "--to", h.UpstreamVPN, "--tls-name", "a.wild.ep.test")
	e.Route("*.od.ep.test", "--to", h.UpstreamVPN, "--tls-on-demand")
	e.WaitHTTPS("https://routed.ep.test/", 200)
	e.WaitHTTPS("https://a.wild.ep.test/", 200) // explicit name, obtained up front
	e.WaitHTTPS("https://x.od.ep.test/", 200)   // opted-in on-demand subdomain
	_, _, errNo := peerCert("noroute.ep.test")
	_, _, errB := peerCert("b.wild.ep.test")
	issued := 0
	for _, n := range random {
		if _, _, err := peerCert(n); err == nil {
			issued++
		}
	}
	time.Sleep(3 * time.Second)
	certs := e.MustSh(h.Gateway, "find /var/lib/entrypoint/caddy/storage/certificates -name '*.crt' | sed 's|.*/||' | sort | tr '\\n' ' '")
	orders := strings.Count(e.Journal(h.Gateway, "entrypoint-caddy"), "rnd")
	result(t, "no route: cert=%v; unapproved wildcard subdomain: cert=%v; %d random SNIs: certs=%d, Caddy log mentions=%d; storage: %s",
		errNo == nil, errB == nil, len(random), issued, orders, certs)
	if errNo == nil || errB == nil || issued > 0 || strings.Contains(certs, "rnd") || strings.Contains(certs, "noroute") ||
		strings.Contains(certs, "b.wild") || strings.Contains(certs, "*.wild") {
		t.Fatal("certificate issued for a name without consent")
	}
	if !strings.Contains(certs, "a.wild.ep.test.crt") || !strings.Contains(certs, "x.od.ep.test.crt") {
		t.Fatalf("approved names without certificate: %s", certs)
	}
	if st := e.MustEP("status"); !strings.Contains(st, "ep.test: 3") {
		t.Errorf("certificate counter in status:\n%s", st)
	}
}

func TestTLS_OnDemandRateLimit(t *testing.T) {
	h.Experiment(t)
	e := h.New(t)
	e.FreshGateway()
	recs := map[string]string{}
	for i := 0; i < 12; i++ {
		recs[fmt.Sprintf("s%d.burst.ep.test", i)] = h.GatewayIP
	}
	e.SetDNS(recs)
	e.Route("*.burst.ep.test", "--to", h.UpstreamVPN, "--tls-on-demand")
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
	h.Experiment(t)
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
	var logs string
	e.Eventually(20*time.Second, 300*time.Millisecond, "tls_error w logu konektora", func() error {
		logs, _ = e.Sh(h.ClientA, "cat /tmp/app-a.log")
		if !strings.Contains(logs, "tls_error") {
			return fmt.Errorf("brak")
		}
		return nil
	})
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
