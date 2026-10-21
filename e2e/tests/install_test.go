package tests

// e2e suite 1: installer and uninstall.

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

// openPorts scans all of the gateway's TCP ports from the network (runner).
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
	result(t, "services: %v; TCP ports open from the network: %v; local listeners: %s", units, open, listen)
	for _, u := range []string{"entrypointd", "entrypoint-caddy", "entrypoint-frps"} {
		if units[u] != "active" {
			t.Errorf("service %s: %s", u, units[u])
		}
	}
	allowed := map[int]bool{80: true, 443: true, 55555: true}
	for _, p := range open {
		if !allowed[p] {
			t.Errorf("port %d reachable from the network (Caddy admin API, frps dashboard and service ports must be private)", p)
		}
	}
	if len(open) != 3 {
		t.Errorf("expected exactly ports 80, 443, 55555; got %v", open)
	}
	for _, priv := range []string{"127.0.0.1:7400", "127.0.0.1:7500", "127.0.0.1:20000"} {
		if !strings.Contains(listen, priv) {
			t.Errorf("no local listener %s", priv)
		}
	}
	if strings.Contains(listen, ":2019") {
		t.Error("Caddy admin API on a TCP port")
	}
}

func TestInstall_ReinstallKeepsState(t *testing.T) {
	e := apiSetup(t)
	_, ca := e.Token("api")
	caBefore := e.MustExec(h.Gateway, "sha256sum", "/var/lib/entrypoint/ca/ca.crt")
	revsBefore := len(revisions(e))
	a := e.StartApp(h.AppOptions{Container: h.ClientA, Connect: ca, Replica: "a", Wait: true})
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	// Reinstalling the same version: install.sh and entrypoint install without flags.
	e.InstallPrograms(h.Gateway)
	e.MustEP("install")
	caAfter := e.MustExec(h.Gateway, "sha256sum", "/var/lib/entrypoint/ca/ca.crt")
	e.WaitReplicas("api", []string{"a"}, 60*time.Second)
	e.WaitHTTPS("https://api.ep.test/whoami", 200)
	revsAfter := len(revisions(e))
	_, err := e.EP("install", "--gateway-name", "other.ep.test")
	result(t, "after reinstall: CA unchanged %v, revisions %d -> %d, replica back, tunnel with the existing token works; gateway rename rejected: %v",
		caBefore == caAfter, revsBefore, revsAfter, err != nil)
	_ = a
	if caBefore != caAfter || revsAfter < revsBefore || err == nil {
		t.Fatal("reinstall broke the state")
	}
}

func TestInstall_UninstallAndPurge(t *testing.T) {
	e := apiSetup(t)
	e.MustEP("uninstall")
	units, _ := e.Sh(h.Gateway, "ls /etc/systemd/system | grep -c entrypoint || true")
	dataKept := e.MustSh(h.Gateway, "test -f /var/lib/entrypoint/db/entrypoint.db && test -f /etc/entrypoint/entrypoint.json && echo yes || echo no")
	progs := e.MustSh(h.Gateway, "test -e /opt/entrypoint && echo present || echo missing")
	// Installing after an uninstall brings the gateway back from the kept data.
	e.InstallPrograms(h.Gateway)
	e.MustEP("install")
	var svcs []struct{ Name string }
	e.EPJSON(&svcs, "service", "list")
	_, err := e.EP("uninstall", "--purge", "--yes")
	gone := e.MustSh(h.Gateway, "test -e /var/lib/entrypoint || test -e /etc/entrypoint || test -e /opt/entrypoint && echo left || echo removed")
	users, _ := e.Sh(h.Gateway, "getent passwd entrypoint entrypoint-caddy entrypoint-frps | wc -l")
	result(t, "uninstall: units=%s, data kept=%s, programs=%s; services after reinstall=%v; uninstall --purge: error=%v, data=%s, users=%s",
		strings.TrimSpace(units), strings.TrimSpace(dataKept), strings.TrimSpace(progs), svcs, err, strings.TrimSpace(gone), strings.TrimSpace(users))
	if strings.TrimSpace(units) != "0" || strings.TrimSpace(dataKept) != "yes" || strings.TrimSpace(progs) != "missing" {
		t.Fatal("uninstall did not keep the data or did not remove the services")
	}
	if len(svcs) != 1 || err != nil || strings.TrimSpace(gone) != "removed" || strings.TrimSpace(users) != "0" {
		t.Fatal("purge or reinstall does not work")
	}
}
