package tests

// Zestaw e2e 1: instalator i odinstalowanie.

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	h "github.com/k18k/entrypoint/e2e/harness"
)

// openPorts skanuje wszystkie porty TCP bramy z sieci (runner).
func openPorts(ip string) []int {
	var mu sync.Mutex
	var open []int
	sem := make(chan struct{}, 512)
	var wg sync.WaitGroup
	for p := 1; p <= 65535; p++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(p int) {
			defer wg.Done()
			defer func() { <-sem }()
			c, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", ip, p), 300*time.Millisecond)
			if err == nil {
				c.Close()
				mu.Lock()
				open = append(open, p)
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()
	sort.Ints(open)
	return open
}

func TestInstall_FreshAndPortScan(t *testing.T) {
	e := apiSetup(t)
	_, ca := e.Token("api")
	e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Wait: true})
	units := e.Status().Units
	open := openPorts(h.GatewayIP)
	listen := e.MustSh(h.Gateway, "ss -Hltn | awk '{print $4}' | sort -u | tr '\\n' ' '")
	result(t, "usługi: %v; otwarte porty TCP z sieci: %v; nasłuch lokalny: %s", units, open, listen)
	for _, u := range []string{"entrypointd", "entrypoint-caddy", "entrypoint-frps"} {
		if units[u] != "active" {
			t.Errorf("usługa %s: %s", u, units[u])
		}
	}
	allowed := map[int]bool{80: true, 443: true, 55555: true}
	for _, p := range open {
		if !allowed[p] {
			t.Errorf("port %d osiągalny z sieci (admin API Caddy, dashboard frps i porty serwisów muszą być prywatne)", p)
		}
	}
	if len(open) != 3 {
		t.Errorf("oczekiwano dokładnie portów 80, 443, 55555; są %v", open)
	}
	for _, priv := range []string{"127.0.0.1:7400", "127.0.0.1:7500", "127.0.0.1:20000"} {
		if !strings.Contains(listen, priv) {
			t.Errorf("brak lokalnego nasłuchu %s", priv)
		}
	}
	if strings.Contains(listen, ":2019") {
		t.Error("admin API Caddy na porcie TCP")
	}
}

func TestInstall_ReinstallKeepsState(t *testing.T) {
	e := apiSetup(t)
	_, ca := e.Token("api")
	caBefore := e.MustExec(h.Gateway, "sha256sum", "/var/lib/entrypoint/ca/ca.crt")
	revsBefore := len(revisions(e))
	a := e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	// Ponowna instalacja tej samej wersji: install.sh i entrypoint install bez flag.
	e.InstallPrograms(h.Gateway)
	e.MustEP("install")
	caAfter := e.MustExec(h.Gateway, "sha256sum", "/var/lib/entrypoint/ca/ca.crt")
	e.WaitReplicas("api", []string{"a"}, 60*time.Second)
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	revsAfter := len(revisions(e))
	_, err := e.EP("install", "--gateway-name", "inna.ep.test")
	result(t, "po ponownej instalacji: CA bez zmian %v, rewizje %d -> %d, replika wróciła, tunel z dotychczasowym tokenem działa; zmiana nazwy bramy odrzucona: %v",
		caBefore == caAfter, revsBefore, revsAfter, err != nil)
	_ = a
	if caBefore != caAfter || revsAfter < revsBefore || err == nil {
		t.Fatal("ponowna instalacja naruszyła stan")
	}
}

func TestInstall_UninstallAndPurge(t *testing.T) {
	e := apiSetup(t)
	e.MustEP("uninstall")
	units, _ := e.Sh(h.Gateway, "ls /etc/systemd/system | grep -c entrypoint || true")
	dataKept := e.MustSh(h.Gateway, "test -f /var/lib/entrypoint/db/entrypoint.db && test -f /etc/entrypoint/entrypoint.json && echo tak || echo nie")
	progs := e.MustSh(h.Gateway, "test -e /opt/entrypoint && echo są || echo brak")
	// Instalacja po odinstalowaniu odtwarza bramę z zachowanych danych.
	e.InstallPrograms(h.Gateway)
	e.MustEP("install")
	var svcs []struct{ Name string }
	e.EPJSON(&svcs, "service", "list")
	_, err := e.EP("uninstall", "--purge", "--yes")
	gone := e.MustSh(h.Gateway, "test -e /var/lib/entrypoint || test -e /etc/entrypoint || test -e /opt/entrypoint && echo zostały || echo usunięte")
	users, _ := e.Sh(h.Gateway, "getent passwd entrypoint entrypoint-caddy entrypoint-frps | wc -l")
	result(t, "uninstall: jednostki=%s, dane zachowane=%s, programy=%s; po ponownej instalacji serwisy=%v; uninstall --purge: błąd=%v, dane=%s, użytkownicy=%s",
		strings.TrimSpace(units), strings.TrimSpace(dataKept), strings.TrimSpace(progs), svcs, err, strings.TrimSpace(gone), strings.TrimSpace(users))
	if strings.TrimSpace(units) != "0" || strings.TrimSpace(dataKept) != "tak" || strings.TrimSpace(progs) != "brak" {
		t.Fatal("uninstall nie zostawił danych albo nie usunął usług")
	}
	if len(svcs) != 1 || err != nil || strings.TrimSpace(gone) != "usunięte" || strings.TrimSpace(users) != "0" {
		t.Fatal("purge albo ponowna instalacja nie działa")
	}
}
