package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/k18k/entrypoint/gateway/internal/api"
	"github.com/k18k/entrypoint/gateway/internal/config"
	"github.com/k18k/entrypoint/gateway/internal/install"
	"github.com/k18k/entrypoint/gateway/internal/store"
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	def := config.Default()
	name := fs.String("gateway-name", "", "the gateway's permanent DNS name, e.g. tunnel.example.com")
	email := fs.String("email", "", "ACME account e-mail (Let's Encrypt)")
	acmeCA := fs.String("acme-ca", def.ACME.CA, "ACME directory")
	acmeStaging := fs.String("acme-staging-ca", def.ACME.StagingCA, "ACME directory for hosts in staging mode")
	var roots multi
	fs.Var(&roots, "acme-ca-root", "extra trusted root for the ACME directory (PEM; tests)")
	tunnelPort := fs.Int("tunnel-port", def.TunnelPort, "tunnel port")
	var ips multi
	fs.Var(&ips, "public-ip", "the gateway's public IP address (for dns check); repeatable")
	pool := fs.String("port-pool", fmt.Sprintf("%d-%d", def.PortPool.Start, def.PortPool.End), "service port pool")
	heartbeat := fs.Int("heartbeat-timeout", def.HeartbeatTimeout, "seconds without a heartbeat after which frps drops the session")
	drift := fs.Duration("drift-interval", def.DriftInterval.Duration, "how often to check the Caddy configuration for drift")
	applyDelay := fs.Duration("test-apply-delay", 0, "tests only: pause while applying a revision")
	acmeProfile := fs.String("test-acme-profile", "", "tests only: ACME profile")
	renewInterval := fs.Duration("test-renew-interval", 0, "tests only: Caddy renewal check interval")
	vpnName := fs.String("vpn-name", "", "set up the built-in VPN with its control server at this DNS name, e.g. vpn.example.com")
	panelName := fs.String("panel-name", "", "serve the web panel at this DNS name, e.g. panel.example.com")
	perIP := fs.Int("per-ip-connections", def.Limits.PerIPConnections, "concurrent connections per client address on 80/443 (IPv6: per /64; 0 = no limit)")
	perIPRate := fs.Int("per-ip-rate", def.Limits.PerIPNewConnsPerSecond, "new connections per second per client address on 80/443 and the tunnel port (0 = no limit)")
	svcConns := fs.Int("service-connections", def.Limits.ConnectionsPerService, "concurrent requests to one service; more get 502 (0 = no limit)")
	noStart := fs.Bool("no-start", false, "do not start the services")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg := def
	if prev, err := config.Load(configPath()); err == nil {
		cfg = prev
	}
	if *name != "" {
		cfg.GatewayName = strings.ToLower(*name)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["email"] {
		cfg.ACME.Email = *email
	}
	if set["acme-ca"] || cfg.ACME.CA == "" {
		cfg.ACME.CA = *acmeCA
	}
	if set["acme-staging-ca"] || cfg.ACME.StagingCA == "" {
		cfg.ACME.StagingCA = *acmeStaging
	}
	if set["acme-ca-root"] {
		cfg.ACME.TrustedRoots = roots
	}
	if set["tunnel-port"] {
		cfg.TunnelPort = *tunnelPort
	}
	if set["public-ip"] {
		cfg.PublicIPs = ips
	}
	if set["port-pool"] {
		a, b, ok := strings.Cut(*pool, "-")
		s, e1 := strconv.Atoi(a)
		e, e2 := strconv.Atoi(b)
		if !ok || e1 != nil || e2 != nil {
			return exitError{2, "--port-pool: expected a range, e.g. 20000-29999"}
		}
		cfg.PortPool = config.PortPool{Start: s, End: e}
	}
	if set["heartbeat-timeout"] {
		cfg.HeartbeatTimeout = *heartbeat
	}
	if set["drift-interval"] {
		cfg.DriftInterval = config.Duration{Duration: *drift}
	}
	if set["test-apply-delay"] {
		cfg.Testing.ApplyDelay = config.Duration{Duration: *applyDelay}
	}
	if set["test-acme-profile"] {
		cfg.Testing.ACMEProfile = *acmeProfile
	}
	if set["per-ip-connections"] {
		cfg.Limits.PerIPConnections = *perIP
	}
	if set["per-ip-rate"] {
		cfg.Limits.PerIPNewConnsPerSecond = *perIPRate
	}
	if set["service-connections"] {
		cfg.Limits.ConnectionsPerService = *svcConns
	}
	if set["vpn-name"] {
		cfg.VPN.Name = strings.ToLower(*vpnName)
	}
	if set["panel-name"] {
		cfg.Panel.Name = strings.ToLower(*panelName)
	}
	if set["test-renew-interval"] {
		cfg.Testing.RenewInterval = config.Duration{Duration: *renewInterval}
	}
	if cfg.GatewayName == "" {
		return exitError{2, "pass --gateway-name, e.g. --gateway-name tunnel.example.com"}
	}
	if cfg.ACME.Email == "" {
		fmt.Fprintln(os.Stderr, "warning: no --email; Let's Encrypt will not send certificate expiry warnings")
	}
	if err := install.Install(install.Options{Config: cfg, ConfigPath: configPath(), Out: os.Stdout, NoStart: *noStart}); err != nil {
		return err
	}
	fmt.Printf("\nGateway %s is running.\n", cfg.GatewayName)
	fmt.Println("Next: entrypoint dns check, entrypoint service add <name>, entrypoint route add <host> ...")
	if cfg.VPN.Enabled() {
		fmt.Println("VPN: entrypoint vpn key prints a key for joining a machine.")
	}
	return nil
}

func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "also remove data and configuration")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := config.Load(configPath())
	if err != nil {
		cfg = config.Default()
	}
	if *purge && !*yes {
		fmt.Fprintln(os.Stderr, "--purge removes the revision database, the private CA, certificates and tokens.")
		fmt.Fprintln(os.Stderr, "Take a backup first: sudo entrypoint backup -o entrypoint-backup.age")
		if !confirm("Really remove all data?") {
			return exitError{1, "aborted"}
		}
	}
	return install.Uninstall(cfg, *purge, os.Stdout)
}

func unitState(u string) string {
	out, _ := exec.Command("systemctl", "is-active", u).Output()
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "unknown"
	}
	return s
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	c := api.NewClient(cfg.APISocket)
	cx, cancel := ctx()
	defer cancel()
	var st api.Status
	apiErr := c.Do(cx, "GET", "/v1/status", nil, &st)
	units := map[string]string{}
	names := []string{"entrypointd", "entrypoint-caddy", "entrypoint-frps"}
	if cfg.VPN.Enabled() {
		names = append(names, "entrypoint-headscale", "entrypoint-tailscaled")
	}
	if cfg.Panel.Enabled() {
		names = append(names, "entrypoint-panel")
	}
	for _, u := range names {
		units[u] = unitState(u + ".service")
	}
	certs := countCerts(cfg)
	if *asJSON {
		out := map[string]any{"units": units, "certificates": certs}
		if apiErr == nil {
			out["status"] = st
		} else {
			out["error"] = apiErr.Error()
		}
		return printJSON(out)
	}
	fmt.Printf("Gateway: %s (tunnel: port %d)\n", cfg.GatewayName, cfg.TunnelPort)
	fmt.Printf("Services: entrypointd=%s caddy=%s frps=%s\n", units["entrypointd"], units["entrypoint-caddy"], units["entrypoint-frps"])
	if cfg.VPN.Enabled() {
		fmt.Printf("VPN: %s headscale=%s tailscaled=%s\n", cfg.VPN.Name, units["entrypoint-headscale"], units["entrypoint-tailscaled"])
	}
	if cfg.Panel.Enabled() {
		fmt.Printf("Panel: https://%s panel=%s\n", cfg.Panel.Name, units["entrypoint-panel"])
	}
	if apiErr != nil {
		fmt.Println("entrypointd unavailable:", apiErr)
		return exitError{1, ""}
	}
	fmt.Printf("Version: %s\n", st.Version)
	if st.LastApplied != nil {
		fmt.Printf("Revision: %d applied", st.LastApplied.Num)
		for _, c := range st.Components {
			fmt.Printf(", %s=%d", c.Name, c.Applied)
		}
		fmt.Println()
	}
	if len(st.Services) > 0 {
		fmt.Println("\nServices:")
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  NAME\tPORT\tREPLICAS\tROUTES")
		for _, s := range st.Services {
			fmt.Fprintf(w, "  %s\t%d\t%d\t%s\n", s.Name, s.Port, len(s.Replicas), strings.Join(s.Routes, " "))
		}
		w.Flush()
	}
	if len(certs) > 0 {
		fmt.Println("\nACME certificates of this installation in the last 7 days (Let's Encrypt limit: 50 per domain):")
		for _, d := range sortedKeys(certs) {
			fmt.Printf("  %s: %d\n", d, certs[d])
		}
		fmt.Println("  (the counter sees only this installation; it is used for warnings only)")
	}
	for d, n := range certs {
		if n >= 40 {
			st.Warnings = append(st.Warnings, fmt.Sprintf("domain %s: %d new certificates in 7 days, close to the limit of 50", d, n))
		}
	}
	if len(st.Warnings) > 0 {
		fmt.Println("\nWarnings:")
		for _, w := range st.Warnings {
			fmt.Println("  ! " + w)
		}
	}
	return nil
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func cmdServiceAdd(args []string) error {
	fs := flag.NewFlagSet("service add", flag.ContinueOnError)
	health := fs.String("health", "", "replica health check path, e.g. /health")
	asJSON := fs.Bool("json", false, "JSON output")
	br := baseRevFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return exitError{2, "usage: entrypoint service add <name> [--health <path>]"}
	}
	c, err := client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	var res api.ChangeResult
	err = c.Do(cx, "POST", "/v1/services", api.ServiceAdd{Name: pos[0], Health: *health, BaseRev: baseRev(br)}, &res)
	if err == nil && !*asJSON && res.Service != nil {
		fmt.Printf("Service %s: port %d.\n", res.Service.Name, res.Service.Port)
	}
	return printChange(&res, err, *asJSON)
}

func cmdServiceRm(args []string) error {
	fs := flag.NewFlagSet("service rm", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	br := baseRevFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return exitError{2, "usage: entrypoint service rm <name>"}
	}
	c, err := client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	path := "/v1/services/" + api.PathEscape(pos[0])
	if b := baseRev(br); b != nil {
		path += "?baseRev=" + strconv.FormatInt(*b, 10)
	}
	var res api.ChangeResult
	err = c.Do(cx, "DELETE", path, nil, &res)
	return printChange(&res, err, *asJSON)
}

func cmdServiceList(args []string) error {
	fs := flag.NewFlagSet("service list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	var out []api.ServiceInfo
	if err := c.Do(cx, "GET", "/v1/services", nil, &out); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPORT\tHEALTH\tREPLICAS")
	for _, s := range out {
		var reps []string
		for _, r := range s.Replicas {
			reps = append(reps, r.Name)
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", s.Name, s.Port, dash(s.Health), dash(strings.Join(reps, " ")))
	}
	return w.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func parseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(s, u.suf) {
			s, mult = strings.TrimSuffix(s, u.suf), u.m
			break
		}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n <= 0 {
		return 0, errors.New("invalid size, e.g. 50MB")
	}
	return n * mult, nil
}

func cmdRouteAdd(args []string) error {
	fs := flag.NewFlagSet("route add", flag.ContinueOnError)
	to := fs.String("to", "", "upstream IP:port")
	service := fs.String("service", "", "tunnel service")
	strip := fs.Bool("strip-prefix", false, "strip the prefix before passing the request upstream")
	maxBody := fs.String("max-body", "", "maximum request body size (default 50MB)")
	headerTimeout := fs.Duration("header-timeout", 0, "response header timeout (default 60s)")
	staging := fs.Bool("acme-staging", false, "certificate from the ACME staging environment")
	var tlsNames multi
	fs.Var(&tlsNames, "tls-name", "wildcard route: a concrete name that gets a certificate (repeatable)")
	onDemand := fs.Bool("tls-on-demand", false, "wildcard route: on-demand certificates for any subdomain (at most 10 new per hour)")
	asJSON := fs.Bool("json", false, "JSON output")
	br := baseRevFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return exitError{2, "usage: entrypoint route add <host>[/prefix] (--to <ip:port> | --service <name>)"}
	}
	host, prefix, _ := strings.Cut(pos[0], "/")
	req := api.RouteAdd{Host: host, Prefix: "/" + prefix, Service: *service, Upstream: *to, StripPrefix: *strip, BaseRev: baseRev(br)}
	if *maxBody != "" {
		n, err := parseSize(*maxBody)
		if err != nil {
			return exitError{2, "--max-body: " + err.Error()}
		}
		req.MaxBodyBytes = n
	}
	if *headerTimeout > 0 {
		req.ResponseHeaderTimeout = int(headerTimeout.Seconds())
	}
	if *staging {
		req.ACME = "staging"
	}
	req.TLSNames = tlsNames
	req.TLSOnDemand = *onDemand
	c, err := client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	var res api.ChangeResult
	err = c.Do(cx, "POST", "/v1/routes", req, &res)
	if err == nil && !*asJSON && res.Route != nil {
		fmt.Printf("Route %s: %s%s -> %s\n", res.Route.ID, res.Route.Host, res.Route.Prefix, target(*res.Route))
	}
	return printChange(&res, err, *asJSON)
}

func target(r api.RouteInfo) string {
	if r.Service != "" {
		return "service " + r.Service
	}
	return r.Upstream
}

func cmdRouteRm(args []string) error {
	fs := flag.NewFlagSet("route rm", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	br := baseRevFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return exitError{2, "usage: entrypoint route rm <id>"}
	}
	c, err := client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	path := "/v1/routes/" + api.PathEscape(pos[0])
	if b := baseRev(br); b != nil {
		path += "?baseRev=" + strconv.FormatInt(*b, 10)
	}
	var res api.ChangeResult
	err = c.Do(cx, "DELETE", path, nil, &res)
	return printChange(&res, err, *asJSON)
}

func cmdRouteList(args []string) error {
	fs := flag.NewFlagSet("route list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	var out []api.RouteInfo
	if err := c.Do(cx, "GET", "/v1/routes", nil, &out); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tHOST\tPREFIX\tTARGET\tOPTIONS")
	for _, r := range out {
		var opts []string
		if r.StripPrefix {
			opts = append(opts, "strip-prefix")
		}
		if r.ACME == "staging" {
			opts = append(opts, "acme-staging")
		}
		if len(r.TLSNames) > 0 {
			opts = append(opts, "tls-names="+strings.Join(r.TLSNames, ","))
		}
		if r.TLSOnDemand {
			opts = append(opts, "tls-on-demand")
		}
		opts = append(opts, fmt.Sprintf("max-body=%dMB", r.MaxBodyBytes>>20), fmt.Sprintf("header-timeout=%ds", r.ResponseHeaderTimeout))
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.Host, r.Prefix, target(r), strings.Join(opts, " "))
	}
	return w.Flush()
}

func cmdTokenCreate(args []string) error {
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	service := fs.String("service", "", "the service this token can connect replicas to")
	label := fs.String("label", "", "token description")
	maxRep := fs.Int("max-replicas", 0, "replica limit per token (default 10)")
	only := fs.Bool("connect-only", false, "print only ENTRYPOINT_CONNECT (for scripts)")
	asJSON := fs.Bool("json", false, "JSON output")
	br := baseRevFlag(fs)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *service == "" {
		return exitError{2, "usage: entrypoint token create --service <name>"}
	}
	c, err := client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	var res api.ChangeResult
	err = c.Do(cx, "POST", "/v1/tokens", api.TokenCreate{Service: *service, Label: *label, MaxReplicas: *maxRep, BaseRev: baseRev(br)}, &res)
	if err != nil || res.Token == nil {
		return printChange(&res, err, *asJSON)
	}
	switch {
	case *asJSON:
		return printJSON(res)
	case *only:
		fmt.Println(res.Token.Connect)
		return nil
	}
	fmt.Printf("Revision %d: applied (%s).\n", res.Revision.Num, res.Revision.Summary)
	fmt.Printf("Token %s for service %s.\n\n", res.Token.ID, res.Token.Service)
	fmt.Fprintln(os.Stderr, "ENTRYPOINT_CONNECT (shown only once; store it like a password):")
	fmt.Println(res.Token.Connect)
	return nil
}

func cmdTokenList(args []string) error {
	fs := flag.NewFlagSet("token list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	var out []api.TokenInfo
	if err := c.Do(cx, "GET", "/v1/tokens", nil, &out); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSERVICE\tSTATE\tCREATED\tLAST LOGIN\tDESCRIPTION")
	for _, t := range out {
		state := "active"
		if t.Revoked {
			state = "revoked"
		}
		last := "-"
		if t.LastLogin != nil {
			last = t.LastLogin.LastLogin.Local().Format("2006-01-02 15:04") + " " + t.LastLogin.ClientIP
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Service, state, t.CreatedAt.Local().Format("2006-01-02 15:04"), last, dash(t.Label))
	}
	return w.Flush()
}

func cmdTokenRevoke(args []string) error {
	fs := flag.NewFlagSet("token revoke", flag.ContinueOnError)
	now := fs.Bool("now", false, "drop the session immediately by restarting frps (a short break for all tunnels)")
	reason := fs.String("reason", "", "revocation reason")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return exitError{2, "usage: entrypoint token revoke <id>"}
	}
	c, err := client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	if err := c.Do(cx, "POST", "/v1/tokens/"+api.PathEscape(pos[0])+"/revoke", api.TokenRevoke{Reason: *reason}, nil); err != nil {
		return err
	}
	cfg, _ := loadConfig()
	fmt.Printf("Token %s revoked. New logins are rejected from now on.\n", pos[0])
	if *now {
		if err := exec.Command("systemctl", "restart", "entrypoint-frps.service").Run(); err != nil {
			return fmt.Errorf("restart frps: %w", err)
		}
		fmt.Println("frps restarted: all sessions dropped, the other tunnels reconnect.")
	} else {
		fmt.Printf("Active sessions of this token are dropped at the next heartbeat (within %ds).\n", cfg.HeartbeatTimeout)
	}
	return nil
}

func cmdRevisionList(args []string) error {
	fs := flag.NewFlagSet("revision list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	limit := fs.Int("n", 30, "number of revisions")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	var out []*store.Revision
	if err := c.Do(cx, "GET", "/v1/revisions?limit="+strconv.Itoa(*limit), nil, &out); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(out)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NUM\tSTATUS\tTIME\tAUTHOR\tDESCRIPTION")
	for _, r := range out {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", r.Num, r.Status, r.CreatedAt.Local().Format(time.DateTime), r.Author, r.Summary)
	}
	return w.Flush()
}

func cmdRevisionShow(args []string) error {
	fs := flag.NewFlagSet("revision show", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return exitError{2, "usage: entrypoint revision show <n>"}
	}
	c, err := client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	var r store.Revision
	if err := c.Do(cx, "GET", "/v1/revisions/"+api.PathEscape(pos[0]), nil, &r); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(r)
	}
	fmt.Printf("Revision %d (base %d)\nStatus: %s\nAuthor: %s\nTime: %s\nDescription: %s\n", r.Num, r.Base, r.Status, r.Author, r.CreatedAt.Local().Format(time.DateTime), r.Summary)
	if r.Error != "" {
		fmt.Println("Error:", r.Error)
	}
	if len(r.Diff) > 0 {
		fmt.Println("Changes:")
		for _, d := range r.Diff {
			fmt.Println("  " + d)
		}
	}
	return nil
}

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	follow := fs.Bool("f", false, "follow")
	n := fs.Int("n", 200, "number of lines")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return exitError{2, "usage: entrypoint logs <entrypointd|caddy|frps|headscale|tailscaled|panel>"}
	}
	unit := map[string]string{"entrypointd": "entrypointd.service", "caddy": "entrypoint-caddy.service", "frps": "entrypoint-frps.service",
		"headscale": "entrypoint-headscale.service", "tailscaled": "entrypoint-tailscaled.service",
		"panel": "entrypoint-panel.service"}[pos[0]]
	if unit == "" {
		return exitError{2, "service: entrypointd, caddy, frps, headscale, tailscaled or panel"}
	}
	a := []string{"-u", unit, "--no-pager", "-n", strconv.Itoa(*n)}
	if *follow {
		a = append(a, "-f")
	}
	cmd := exec.Command("journalctl", a...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func cmdUpdate(args []string) error {
	fmt.Println(`In v0.1 updates are manual (automatic rollback arrives in v0.2.0):
  1. sudo entrypoint backup -o entrypoint-before-update.age
  2. curl -fsSL https://raw.githubusercontent.com/k18k/entrypoint/master/scripts/install.sh | sudo ENTRYPOINT_VERSION=<version> sh
  3. sudo entrypoint install
  4. sudo entrypoint status`)
	return exitError{1, ""}
}
