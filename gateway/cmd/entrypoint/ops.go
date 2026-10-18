package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/net/publicsuffix"
	"golang.org/x/term"

	"github.com/k18k/entrypoint/gateway/internal/api"
	"github.com/k18k/entrypoint/gateway/internal/backup"
	"github.com/k18k/entrypoint/gateway/internal/config"
	"github.com/k18k/entrypoint/gateway/internal/pki"
)

// localIPs zwraca adresy, pod którymi brama jest osiągalna: z konfiguracji albo z interfejsów.
func localIPs(cfg config.Config) []netip.Addr {
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

func cmdDNSCheck(args []string) error {
	fs := flag.NewFlagSet("dns check", flag.ContinueOnError)
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
	var routes []api.RouteInfo
	if err := c.Do(cx, "GET", "/v1/routes", nil, &routes); err != nil {
		return err
	}
	hosts := []string{cfg.GatewayName}
	seen := map[string]bool{cfg.GatewayName: true}
	for _, r := range routes {
		if !seen[r.Host] {
			seen[r.Host] = true
			hosts = append(hosts, r.Host)
		}
	}
	want := localIPs(cfg)
	wantSet := map[netip.Addr]bool{}
	for _, a := range want {
		wantSet[a] = true
	}
	type row struct {
		Host     string   `json:"host"`
		Record   string   `json:"record"`
		Current  []string `json:"current"`
		OK       bool     `json:"ok"`
		Problem  string   `json:"problem,omitempty"`
	}
	var rows []row
	problems := 0
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
		r := row{Host: h, Record: strings.Join(wantRecs, ", ")}
		lctx, lcancel := context.WithTimeout(context.Background(), 5*time.Second)
		ips, err := net.DefaultResolver.LookupNetIP(lctx, "ip", lookup)
		lcancel()
		if err != nil {
			r.Problem = "brak rekordu"
		} else {
			match := false
			for _, ip := range ips {
				ip = ip.Unmap()
				r.Current = append(r.Current, ip.String())
				if wantSet[ip] {
					match = true
				} else {
					r.Problem = "rekord wskazuje inny adres"
				}
			}
			if match && r.Problem == "" {
				r.OK = true
			} else if r.Problem == "" {
				r.Problem = "rekord wskazuje inny adres"
			}
		}
		if !r.OK {
			problems++
		}
		rows = append(rows, r)
	}
	if *asJSON {
		if err := printJSON(rows); err != nil {
			return err
		}
	} else {
		if len(want) == 0 {
			fmt.Println("uwaga: nie wykryto adresu bramy; podaj go przy instalacji: --public-ip")
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "HOST\tWYMAGANY REKORD\tSTAN\tOBECNIE")
		for _, r := range rows {
			state := "OK"
			if !r.OK {
				state = r.Problem
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Host, r.Record, state, dash(strings.Join(r.Current, " ")))
		}
		w.Flush()
		fmt.Println("\nTo test wstępny, a nie ochrona przed limitami Let's Encrypt. Entrypoint nie zmienia rekordów DNS.")
	}
	if problems > 0 {
		return exitError{1, ""}
	}
	return nil
}

func cmdCAStatus(args []string) error {
	fs := flag.NewFlagSet("ca status", flag.ContinueOnError)
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
	var info api.CAInfo
	if err := c.Do(cx, "GET", "/v1/ca", nil, &info); err != nil {
		return err
	}
	return printCA(info, *asJSON)
}

func printCA(info api.CAInfo, asJSON bool) error {
	if asJSON {
		return printJSON(info)
	}
	state := map[string]string{"": "brak rotacji", "staged": "etap 1: nowe CA w pakietach, serwer na starym", "switched": "etap 2: serwer na nowym CA, stare CA nadal w pakietach"}[info.State]
	fmt.Println("Rotacja:", state)
	fmt.Printf("CA bieżące: %s (ważne do %s)\n", info.Current.Fingerprint, info.Current.NotAfter.Format("2006-01-02"))
	if info.Next != nil {
		fmt.Printf("CA nowe:    %s (ważne do %s)\n", info.Next.Fingerprint, info.Next.NotAfter.Format("2006-01-02"))
	}
	if info.Old != nil {
		fmt.Printf("CA stare:   %s\n", info.Old.Fingerprint)
	}
	fmt.Printf("Certyfikat portu tunelu: %s, wystawca %s, ważny do %s\n", info.Server.Subject, info.Server.Issuer, info.Server.NotAfter.Format("2006-01-02"))
	fmt.Println("CA w nowych pakietach połączenia:", strings.Join(info.BundleCAs, ", "))
	if info.State != "" {
		if len(info.StaleClients) == 0 {
			fmt.Println("Wszyscy klienci logują się już z pakietem zawierającym nowe CA.")
		} else {
			fmt.Println("Klienci logujący się jeszcze ze starym pakietem:")
			for _, t := range info.StaleClients {
				fmt.Printf("  token %s (serwis %s), ostatnio %s z %s\n", t.ID, t.Service, t.LastLogin.LastLogin.Local().Format(time.DateTime), t.LastLogin.ClientIP)
			}
		}
	}
	return nil
}

func cmdCARotate(args []string) error {
	fs := flag.NewFlagSet("ca rotate", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "wyjście JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return exitError{2, `użycie: entrypoint ca rotate start|switch|finish
  start  — nowe CA; nowe pakiety połączenia zawierają oba CA (wydaj klientom nowe tokeny)
  switch — serwer tunelu przechodzi na nowe CA (klienci tylko ze starym CA zostaną odrzuceni)
  finish — stare CA znika z pakietów`}
	}
	c, err := client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	var info api.CAInfo
	if err := c.Do(cx, "POST", "/v1/ca/rotate", api.CARotate{Step: pos[0]}, &info); err != nil {
		return err
	}
	return printCA(info, *asJSON)
}

// countCerts liczy certyfikaty wystawione przez tę instalację w ostatnich 7 dniach, per domena rejestrowalna.
func countCerts(cfg config.Config) map[string]int {
	out := map[string]int{}
	root := filepath.Join(cfg.CaddyStorage(), "certificates")
	week := time.Now().Add(-7 * 24 * time.Hour)
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".crt") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		c, err := pki.ParseCert(b)
		if err != nil || c.NotBefore.Before(week) {
			return nil
		}
		name := c.Subject.CommonName
		if len(c.DNSNames) > 0 {
			name = c.DNSNames[0]
		}
		dom, err := publicsuffix.EffectiveTLDPlusOne(strings.TrimPrefix(name, "*."))
		if err != nil {
			dom = name
		}
		out[dom]++
		return nil
	})
	return out
}

func readPassword(prompt string, twice bool) (string, error) {
	if v := os.Getenv("ENTRYPOINT_BACKUP_PASSWORD"); v != "" {
		return v, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("podaj hasło przez --password-file albo ENTRYPOINT_BACKUP_PASSWORD")
	}
	fmt.Fprint(os.Stderr, prompt)
	a, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if twice {
		fmt.Fprint(os.Stderr, "Powtórz hasło: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			return "", errors.New("hasła się różnią")
		}
	}
	if len(a) < 12 {
		return "", errors.New("hasło backupu musi mieć co najmniej 12 znaków")
	}
	return string(a), nil
}

func passwordFrom(file string, twice bool) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return readPassword("Hasło backupu: ", twice)
}

func cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	out := fs.String("o", "", "plik wynikowy (domyślnie entrypoint-backup-<data>.age)")
	pwFile := fs.String("password-file", "", "plik z hasłem")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("backup wymaga uprawnień root (sudo)")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	pw, err := passwordFrom(*pwFile, true)
	if err != nil {
		return err
	}
	if *out == "" {
		*out = "entrypoint-backup-" + time.Now().Format("20060102-150405") + ".age"
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	m, err := backup.Create(cx, cfg, configPath(), pw, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(*out)
		return err
	}
	fmt.Printf("Backup zapisany: %s (wersja %s, rewizja %d).\n", *out, m.Version, m.LastApplied)
	fmt.Println("Przechowuj plik i hasło osobno, poza tą maszyną.")
	return nil
}

func cmdRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	pwFile := fs.String("password-file", "", "plik z hasłem")
	yes := fs.Bool("yes", false, "nie pytaj o potwierdzenie")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return exitError{2, "użycie: entrypoint restore <plik>"}
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "Restore zastąpi bieżący stan bramy stanem z backupu.")
		fmt.Fprintln(os.Stderr, "Wszystkie odtworzone tokeny zostaną unieważnione; trzeba będzie wydać nowe.")
		if !confirm("Kontynuować?") {
			return exitError{1, "przerwano"}
		}
	}
	pw, err := passwordFrom(*pwFile, false)
	if err != nil {
		return err
	}
	f, err := os.Open(pos[0])
	if err != nil {
		return err
	}
	defer f.Close()
	cx, cancel := ctx()
	defer cancel()
	m, n, err := backup.Restore(cx, configPath(), pw, f, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Printf("Odtworzono backup z %s (wersja %s, rewizja %d).\n", m.CreatedAt.Local().Format(time.DateTime), m.Version, m.LastApplied)
	fmt.Printf("Unieważniono %d tokenów. Wydaj nowe: sudo entrypoint token create --service <nazwa>\n", n)
	fmt.Println("Jeśli zmienił się adres IP, zaktualizuj rekordy DNS (sudo entrypoint dns check).")
	cfg, _ := loadConfig()
	c := api.NewClient(cfg.APISocket)
	var svcs []api.ServiceInfo
	if err := c.Do(cx, "GET", "/v1/services", nil, &svcs); err == nil && len(svcs) > 0 {
		names := make([]string, 0, len(svcs))
		for _, s := range svcs {
			names = append(names, s.Name)
		}
		sort.Strings(names)
		fmt.Println("Serwisy wymagające nowych tokenów:", strings.Join(names, ", "))
	}
	return nil
}
