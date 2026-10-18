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
	name := fs.String("gateway-name", "", "stała nazwa DNS bramy, np. tunnel.example.com")
	email := fs.String("email", "", "e-mail konta ACME (Let's Encrypt)")
	acmeCA := fs.String("acme-ca", def.ACME.CA, "katalog ACME")
	acmeStaging := fs.String("acme-staging-ca", def.ACME.StagingCA, "katalog ACME dla hostów w trybie testowym")
	var roots multi
	fs.Var(&roots, "acme-ca-root", "dodatkowy zaufany root katalogu ACME (PEM; testy)")
	tunnelPort := fs.Int("tunnel-port", def.TunnelPort, "port tunelu")
	var ips multi
	fs.Var(&ips, "public-ip", "publiczny adres IP bramy (dla dns check); można powtórzyć")
	pool := fs.String("port-pool", fmt.Sprintf("%d-%d", def.PortPool.Start, def.PortPool.End), "pula portów serwisów")
	heartbeat := fs.Int("heartbeat-timeout", def.HeartbeatTimeout, "sekundy bez heartbeat, po których frps zrywa sesję")
	drift := fs.Duration("drift-interval", def.DriftInterval.Duration, "co ile sprawdzać dryf konfiguracji Caddy")
	applyDelay := fs.Duration("test-apply-delay", 0, "tylko testy: pauza w trakcie stosowania rewizji")
	acmeProfile := fs.String("test-acme-profile", "", "tylko testy: profil ACME")
	renewInterval := fs.Duration("test-renew-interval", 0, "tylko testy: interwał sprawdzania odnowień Caddy")
	noStart := fs.Bool("no-start", false, "nie uruchamiaj usług")
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
			return exitError{2, "--port-pool: oczekiwano zakresu, np. 20000-29999"}
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
	if set["test-renew-interval"] {
		cfg.Testing.RenewInterval = config.Duration{Duration: *renewInterval}
	}
	if cfg.GatewayName == "" {
		return exitError{2, "podaj --gateway-name, np. --gateway-name tunnel.example.com"}
	}
	if cfg.ACME.Email == "" {
		fmt.Fprintln(os.Stderr, "uwaga: brak --email; Let's Encrypt nie wyśle ostrzeżeń o wygasaniu certyfikatów")
	}
	if err := install.Install(install.Options{Config: cfg, ConfigPath: configPath(), Out: os.Stdout, NoStart: *noStart}); err != nil {
		return err
	}
	fmt.Printf("\nBrama %s działa.\n", cfg.GatewayName)
	fmt.Println("Dalej: entrypoint dns check, entrypoint service add <nazwa>, entrypoint route add <host> ...")
	return nil
}

func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "usuń także dane i konfigurację")
	yes := fs.Bool("yes", false, "nie pytaj o potwierdzenie")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := config.Load(configPath())
	if err != nil {
		cfg = config.Default()
	}
	if *purge && !*yes {
		fmt.Fprintln(os.Stderr, "--purge usunie bazę z rewizjami, prywatne CA, certyfikaty i tokeny.")
		fmt.Fprintln(os.Stderr, "Zalecany jest wcześniejszy backup: sudo entrypoint backup -o entrypoint-backup.age")
		if !confirm("Czy na pewno usunąć wszystkie dane?") {
			return exitError{1, "przerwano"}
		}
	}
	return install.Uninstall(cfg, *purge, os.Stdout)
}

func unitState(u string) string {
	out, _ := exec.Command("systemctl", "is-active", u).Output()
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "nieznany"
	}
	return s
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "wyjście JSON")
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
	for _, u := range []string{"entrypointd", "entrypoint-caddy", "entrypoint-frps"} {
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
	fmt.Printf("Brama: %s (tunel: port %d)\n", cfg.GatewayName, cfg.TunnelPort)
	fmt.Printf("Usługi: entrypointd=%s caddy=%s frps=%s\n", units["entrypointd"], units["entrypoint-caddy"], units["entrypoint-frps"])
	if apiErr != nil {
		fmt.Println("entrypointd niedostępny:", apiErr)
		return exitError{1, ""}
	}
	fmt.Printf("Wersja: %s\n", st.Version)
	if st.LastApplied != nil {
		fmt.Printf("Rewizja: %d zastosowana", st.LastApplied.Num)
		for _, c := range st.Components {
			fmt.Printf(", %s=%d", c.Name, c.Applied)
		}
		fmt.Println()
	}
	if len(st.Services) > 0 {
		fmt.Println("\nSerwisy:")
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "  NAZWA\tPORT\tREPLIKI\tTRASY")
		for _, s := range st.Services {
			fmt.Fprintf(w, "  %s\t%d\t%d\t%s\n", s.Name, s.Port, len(s.Replicas), strings.Join(s.Routes, " "))
		}
		w.Flush()
	}
	if len(certs) > 0 {
		fmt.Println("\nCertyfikaty ACME tej instalacji w ostatnich 7 dniach (limit Let's Encrypt: 50 na domenę):")
		for _, d := range sortedKeys(certs) {
			fmt.Printf("  %s: %d\n", d, certs[d])
		}
		fmt.Println("  (licznik widzi tylko tę instalację; służy wyłącznie do ostrzeżeń)")
	}
	for d, n := range certs {
		if n >= 40 {
			st.Warnings = append(st.Warnings, fmt.Sprintf("domena %s: %d nowych certyfikatów w 7 dni, blisko limitu 50", d, n))
		}
	}
	if len(st.Warnings) > 0 {
		fmt.Println("\nOstrzeżenia:")
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
	health := fs.String("health", "", "ścieżka health checku replik, np. /health")
	asJSON := fs.Bool("json", false, "wyjście JSON")
	br := baseRevFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return exitError{2, "użycie: entrypoint service add <nazwa> [--health <ścieżka>]"}
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
		fmt.Printf("Serwis %s: port %d.\n", res.Service.Name, res.Service.Port)
	}
	return printChange(&res, err, *asJSON)
}

func cmdServiceRm(args []string) error {
	fs := flag.NewFlagSet("service rm", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "wyjście JSON")
	br := baseRevFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return exitError{2, "użycie: entrypoint service rm <nazwa>"}
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
	asJSON := fs.Bool("json", false, "wyjście JSON")
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
	fmt.Fprintln(w, "NAZWA\tPORT\tHEALTH\tREPLIKI")
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
		return 0, errors.New("niepoprawny rozmiar, np. 50MB")
	}
	return n * mult, nil
}

func cmdRouteAdd(args []string) error {
	fs := flag.NewFlagSet("route add", flag.ContinueOnError)
	to := fs.String("to", "", "upstream IP:port")
	service := fs.String("service", "", "serwis tunelu")
	strip := fs.Bool("strip-prefix", false, "usuń prefiks przed przekazaniem do upstreamu")
	maxBody := fs.String("max-body", "", "maksymalny rozmiar treści żądania (domyślnie 50MB)")
	headerTimeout := fs.Duration("header-timeout", 0, "timeout nagłówków odpowiedzi (domyślnie 60s)")
	staging := fs.Bool("acme-staging", false, "certyfikat z testowego środowiska ACME (staging)")
	asJSON := fs.Bool("json", false, "wyjście JSON")
	br := baseRevFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return exitError{2, "użycie: entrypoint route add <host>[/prefiks] (--to <ip:port> | --service <nazwa>)"}
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
	c, err := client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	var res api.ChangeResult
	err = c.Do(cx, "POST", "/v1/routes", req, &res)
	if err == nil && !*asJSON && res.Route != nil {
		fmt.Printf("Trasa %s: %s%s -> %s\n", res.Route.ID, res.Route.Host, res.Route.Prefix, target(*res.Route))
	}
	return printChange(&res, err, *asJSON)
}

func target(r api.RouteInfo) string {
	if r.Service != "" {
		return "serwis " + r.Service
	}
	return r.Upstream
}

func cmdRouteRm(args []string) error {
	fs := flag.NewFlagSet("route rm", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "wyjście JSON")
	br := baseRevFlag(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return exitError{2, "użycie: entrypoint route rm <id>"}
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
	asJSON := fs.Bool("json", false, "wyjście JSON")
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
	fmt.Fprintln(w, "ID\tHOST\tPREFIKS\tCEL\tOPCJE")
	for _, r := range out {
		var opts []string
		if r.StripPrefix {
			opts = append(opts, "strip-prefix")
		}
		if r.ACME == "staging" {
			opts = append(opts, "acme-staging")
		}
		opts = append(opts, fmt.Sprintf("max-body=%dMB", r.MaxBodyBytes>>20), fmt.Sprintf("header-timeout=%ds", r.ResponseHeaderTimeout))
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.Host, r.Prefix, target(r), strings.Join(opts, " "))
	}
	return w.Flush()
}

func cmdTokenCreate(args []string) error {
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	service := fs.String("service", "", "serwis, do którego token pozwala podłączać repliki")
	label := fs.String("label", "", "opis tokenu")
	maxRep := fs.Int("max-replicas", 0, "limit replik na token (domyślnie 10)")
	only := fs.Bool("connect-only", false, "wypisz wyłącznie ENTRYPOINT_CONNECT (dla skryptów)")
	asJSON := fs.Bool("json", false, "wyjście JSON")
	br := baseRevFlag(fs)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *service == "" {
		return exitError{2, "użycie: entrypoint token create --service <nazwa>"}
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
	fmt.Printf("Rewizja %d: zastosowana (%s).\n", res.Revision.Num, res.Revision.Summary)
	fmt.Printf("Token %s dla serwisu %s.\n\n", res.Token.ID, res.Token.Service)
	fmt.Fprintln(os.Stderr, "ENTRYPOINT_CONNECT (pokazywany tylko raz; przechowuj jak hasło):")
	fmt.Println(res.Token.Connect)
	return nil
}

func cmdTokenList(args []string) error {
	fs := flag.NewFlagSet("token list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "wyjście JSON")
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
	fmt.Fprintln(w, "ID\tSERWIS\tSTAN\tUTWORZONY\tOSTATNIE LOGOWANIE\tOPIS")
	for _, t := range out {
		state := "aktywny"
		if t.Revoked {
			state = "odwołany"
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
	now := fs.Bool("now", false, "zerwij sesję natychmiast restartem frps (krótka przerwa dla wszystkich tuneli)")
	reason := fs.String("reason", "", "powód odwołania")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return exitError{2, "użycie: entrypoint token revoke <id>"}
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
	fmt.Printf("Token %s odwołany. Nowe logowania są odrzucane od teraz.\n", pos[0])
	if *now {
		if err := exec.Command("systemctl", "restart", "entrypoint-frps.service").Run(); err != nil {
			return fmt.Errorf("restart frps: %w", err)
		}
		fmt.Println("frps zrestartowany: wszystkie sesje zerwane, pozostałe tunele łączą się ponownie.")
	} else {
		fmt.Printf("Aktywne sesje tego tokenu zostaną zerwane przy najbliższym heartbeat (najpóźniej po %ds).\n", cfg.HeartbeatTimeout)
	}
	return nil
}

func cmdRevisionList(args []string) error {
	fs := flag.NewFlagSet("revision list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "wyjście JSON")
	limit := fs.Int("n", 30, "liczba rewizji")
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
	fmt.Fprintln(w, "NR\tSTATUS\tCZAS\tAUTOR\tOPIS")
	for _, r := range out {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", r.Num, r.Status, r.CreatedAt.Local().Format(time.DateTime), r.Author, r.Summary)
	}
	return w.Flush()
}

func cmdRevisionShow(args []string) error {
	fs := flag.NewFlagSet("revision show", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "wyjście JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return exitError{2, "użycie: entrypoint revision show <n>"}
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
	fmt.Printf("Rewizja %d (bazowa %d)\nStatus: %s\nAutor: %s\nCzas: %s\nOpis: %s\n", r.Num, r.Base, r.Status, r.Author, r.CreatedAt.Local().Format(time.DateTime), r.Summary)
	if r.Error != "" {
		fmt.Println("Błąd:", r.Error)
	}
	if len(r.Diff) > 0 {
		fmt.Println("Zmiany:")
		for _, d := range r.Diff {
			fmt.Println("  " + d)
		}
	}
	return nil
}

func cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	follow := fs.Bool("f", false, "śledź na bieżąco")
	n := fs.Int("n", 200, "liczba linii")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return exitError{2, "użycie: entrypoint logs <entrypointd|caddy|frps>"}
	}
	unit := map[string]string{"entrypointd": "entrypointd.service", "caddy": "entrypoint-caddy.service", "frps": "entrypoint-frps.service"}[pos[0]]
	if unit == "" {
		return exitError{2, "usługa: entrypointd, caddy albo frps"}
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
	fmt.Println(`W v0.1 aktualizacja jest ręczna (automatyczny rollback wchodzi w v0.2.0):
  1. sudo entrypoint backup -o entrypoint-przed-aktualizacja.age
  2. curl -fsSL <adres>/install.sh | sudo ENTRYPOINT_VERSION=<wersja> sh
  3. sudo entrypoint install
  4. sudo entrypoint status`)
	return exitError{1, ""}
}
