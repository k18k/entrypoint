// Package dnscheck compares the DNS records of the gateway's hosts with its addresses.
// It is a preflight check: the gateway never changes DNS.
package dnscheck

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/k18k/entrypoint/gateway/internal/config"
)

// Row is the state of one host.
type Row struct {
	Host    string   `json:"host"`
	Record  string   `json:"record"`
	Current []string `json:"current"`
	OK      bool     `json:"ok"`
	Problem string   `json:"problem,omitempty"`
}

// Hosts returns the gateway's own names followed by the route hosts, without duplicates.
func Hosts(cfg config.Config, routeHosts []string) []string {
	hosts := []string{cfg.GatewayName}
	if cfg.VPN.Enabled() {
		hosts = append(hosts, cfg.VPN.Name)
	}
	if cfg.Panel.Enabled() {
		hosts = append(hosts, cfg.Panel.Name)
	}
	seen := map[string]bool{}
	for _, h := range hosts {
		seen[h] = true
	}
	for _, h := range routeHosts {
		if !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// Check looks up every host. want are the addresses the records should point to.
func Check(cfg config.Config, hosts []string) (rows []Row, want []netip.Addr) {
	want = LocalIPs(cfg)
	wantSet := map[netip.Addr]bool{}
	for _, a := range want {
		wantSet[a] = true
	}
	for _, h := range hosts {
		lookup := h
		if strings.HasPrefix(h, "*.") {
			lookup = "entrypoint-dns-check" + h[1:]
		}
		var wantRecs []string
		for _, a := range want {
			t := "A"
			if a.Is6() {
				t = "AAAA"
			}
			wantRecs = append(wantRecs, t+" "+a.String())
		}
		r := Row{Host: h, Record: strings.Join(wantRecs, ", ")}
		lctx, lcancel := context.WithTimeout(context.Background(), 5*time.Second)
		ips, err := net.DefaultResolver.LookupNetIP(lctx, "ip", lookup)
		lcancel()
		if err != nil {
			r.Problem = "no record"
		} else {
			match := false
			for _, ip := range ips {
				ip = ip.Unmap()
				r.Current = append(r.Current, ip.String())
				if wantSet[ip] {
					match = true
				} else {
					r.Problem = "record points to a different address"
				}
			}
			if match && r.Problem == "" {
				r.OK = true
			} else if r.Problem == "" {
				r.Problem = "record points to a different address"
			}
		}
		rows = append(rows, r)
	}
	return rows, want
}

// LocalIPs returns the addresses the gateway is reachable at: from the configuration or the interfaces.
func LocalIPs(cfg config.Config) []netip.Addr {
	var out []netip.Addr
	for _, s := range cfg.PublicIPs {
		if a, err := netip.ParseAddr(s); err == nil {
			out = append(out, a.Unmap())
		}
	}
	if len(out) > 0 {
		return out
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			ip, ok := netip.AddrFromSlice(n.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || !ip.IsGlobalUnicast() {
				continue
			}
			out = append(out, ip)
		}
	}
	return out
}
