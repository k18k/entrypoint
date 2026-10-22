package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/k18k/entrypoint/gateway/internal/api"
	"github.com/k18k/entrypoint/gateway/internal/dnscheck"
	"github.com/k18k/entrypoint/gateway/internal/install"
	"github.com/k18k/entrypoint/gateway/internal/token"
)

var errNoVPN = userError{errors.New("the VPN is not set up; reinstall with: sudo entrypoint install --vpn-name vpn.example.com")}

func (d *Daemon) hVPN(w http.ResponseWriter, r *http.Request) {
	out := api.VPN{Enabled: d.cfg.VPN.Enabled(), Nodes: []api.VPNNode{}}
	if !out.Enabled {
		reply(w, 200, out)
		return
	}
	out.Name, out.Domain = d.cfg.VPN.Name, install.VPNDomain
	raw, err := install.Headscale(d.cfg, "nodes", "list", "-o", "json")
	if err != nil {
		fail(w, 502, err)
		return
	}
	var nodes []struct {
		ID        uint64   `json:"id"`
		Name      string   `json:"given_name"`
		Addresses []string `json:"ip_addresses"`
		Online    bool     `json:"online"`
		LastSeen  struct {
			Seconds int64 `json:"seconds"`
		} `json:"last_seen"`
	}
	if err := json.Unmarshal(raw, &nodes); err != nil {
		fail(w, 502, fmt.Errorf("headscale nodes list: %w", err))
		return
	}
	for _, n := range nodes {
		vn := api.VPNNode{ID: n.ID, Name: n.Name, Addresses: n.Addresses, Online: n.Online}
		if n.LastSeen.Seconds > 0 {
			t := time.Unix(n.LastSeen.Seconds, 0).UTC()
			vn.LastSeen = &t
		}
		out.Nodes = append(out.Nodes, vn)
	}
	reply(w, 200, out)
}

func (d *Daemon) hVPNKey(w http.ResponseWriter, r *http.Request) {
	if !d.cfg.VPN.Enabled() {
		fail(w, 400, errNoVPN)
		return
	}
	var req api.VPNKeyCreate
	if !decode(w, r, &req) {
		return
	}
	exp := time.Hour
	if req.Expiration != "" {
		v, err := time.ParseDuration(req.Expiration)
		if err != nil || v < time.Minute || v > 365*24*time.Hour {
			fail(w, 400, errors.New("expiration: a duration between 1m and 8760h, e.g. 24h"))
			return
		}
		exp = v
	}
	key, err := install.PreAuthKey(d.cfg, exp.String(), req.Reusable)
	if err != nil {
		fail(w, 502, err)
		return
	}
	server := "https://" + d.cfg.VPN.Name
	bundle, err := token.VPNBundle{Server: server, Key: key}.Encode()
	if err != nil {
		fail(w, 500, err)
		return
	}
	d.log.Info("VPN key issued", "reusable", req.Reusable, "expiration", exp.String(), "author", author(r))
	reply(w, 200, api.VPNKey{Key: key, Bundle: bundle, Server: server, Reusable: req.Reusable, Expiration: time.Now().Add(exp).UTC()})
}

func (d *Daemon) hVPNNodeRm(w http.ResponseWriter, r *http.Request) {
	if !d.cfg.VPN.Enabled() {
		fail(w, 400, errNoVPN)
		return
	}
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, 400, errors.New("the node ID is a number"))
		return
	}
	if _, err := install.Headscale(d.cfg, "nodes", "delete", "-i", strconv.FormatUint(id, 10), "-o", "json"); err != nil {
		fail(w, 502, err)
		return
	}
	d.log.Info("VPN node removed", "id", id, "author", author(r))
	reply(w, 200, map[string]any{"removed": id})
}

// hDNS checks the DNS records of the gateway's names and route hosts (for the panel).
func (d *Daemon) hDNS(w http.ResponseWriter, r *http.Request) {
	head, err := d.headSpec(r.Context())
	if err != nil {
		fail(w, 500, err)
		return
	}
	var hosts []string
	for _, rt := range head.Spec.Routes {
		hosts = append(hosts, rt.Host)
	}
	rows, want := dnscheck.Check(d.cfg, dnscheck.Hosts(d.cfg, hosts))
	addrs := []string{}
	for _, a := range want {
		addrs = append(addrs, a.String())
	}
	if rows == nil {
		rows = []dnscheck.Row{}
	}
	reply(w, 200, map[string]any{"rows": rows, "addresses": addrs})
}
