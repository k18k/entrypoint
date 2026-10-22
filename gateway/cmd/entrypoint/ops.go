package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
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
	"github.com/k18k/entrypoint/gateway/internal/dnscheck"
	"github.com/k18k/entrypoint/gateway/internal/pki"
)

func cmdDNSCheck(args []string) error {
	fs := flag.NewFlagSet("dns check", flag.ContinueOnError)
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
	var routes []api.RouteInfo
	if err := c.Do(cx, "GET", "/v1/routes", nil, &routes); err != nil {
		return err
	}
	var hosts []string
	for _, r := range routes {
		hosts = append(hosts, r.Host)
	}
	rows, want := dnscheck.Check(cfg, dnscheck.Hosts(cfg, hosts))
	problems := 0
	for _, r := range rows {
		if !r.OK {
			problems++
		}
	}
	if *asJSON {
		if err := printJSON(rows); err != nil {
			return err
		}
	} else {
		if len(want) == 0 {
			fmt.Println("warning: gateway address not detected; pass it at install time: --public-ip")
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "HOST\tREQUIRED RECORD\tSTATE\tCURRENT")
		for _, r := range rows {
			state := "OK"
			if !r.OK {
				state = r.Problem
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Host, r.Record, state, dash(strings.Join(r.Current, " ")))
		}
		w.Flush()
		fmt.Println("\nThis is a preflight check, not protection against Let's Encrypt limits. Entrypoint does not change DNS records.")
	}
	if problems > 0 {
		return exitError{1, ""}
	}
	return nil
}

func cmdCAStatus(args []string) error {
	fs := flag.NewFlagSet("ca status", flag.ContinueOnError)
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
	state := map[string]string{"": "no rotation", "staged": "step 1: new CA in bundles, server on the old one", "switched": "step 2: server on the new CA, old CA still in bundles"}[info.State]
	fmt.Println("Rotation:", state)
	fmt.Printf("Current CA: %s (valid until %s)\n", info.Current.Fingerprint, info.Current.NotAfter.Format("2006-01-02"))
	if info.Next != nil {
		fmt.Printf("New CA:     %s (valid until %s)\n", info.Next.Fingerprint, info.Next.NotAfter.Format("2006-01-02"))
	}
	if info.Old != nil {
		fmt.Printf("Old CA:     %s\n", info.Old.Fingerprint)
	}
	fmt.Printf("Tunnel port certificate: %s, issuer %s, valid until %s\n", info.Server.Subject, info.Server.Issuer, info.Server.NotAfter.Format("2006-01-02"))
	fmt.Println("CAs in new connection bundles:", strings.Join(info.BundleCAs, ", "))
	if info.State != "" {
		if len(info.StaleClients) == 0 {
			fmt.Println("All clients already log in with a bundle that contains the new CA.")
		} else {
			fmt.Println("Clients still logging in with an old bundle:")
			for _, t := range info.StaleClients {
				fmt.Printf("  token %s (service %s), last %s from %s\n", t.ID, t.Service, t.LastLogin.LastLogin.Local().Format(time.DateTime), t.LastLogin.ClientIP)
			}
		}
	}
	return nil
}

func cmdCARotate(args []string) error {
	fs := flag.NewFlagSet("ca rotate", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return exitError{2, `usage: entrypoint ca rotate start|switch|finish
  start  — new CA; new connection bundles contain both CAs (issue new tokens to clients)
  switch — the tunnel server moves to the new CA (clients with only the old CA are rejected)
  finish — the old CA leaves the bundles`}
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

// countCerts counts certificates issued by this installation in the last 7 days, per registrable domain.
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
		return "", errors.New("pass the password with --password-file or ENTRYPOINT_BACKUP_PASSWORD")
	}
	fmt.Fprint(os.Stderr, prompt)
	a, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if twice {
		fmt.Fprint(os.Stderr, "Repeat the password: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			return "", errors.New("passwords do not match")
		}
	}
	if len(a) < 12 {
		return "", errors.New("the backup password must have at least 12 characters")
	}
	return string(a), nil
}

func passwordFrom(file string, creating bool) (string, error) {
	var pw string
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		pw = strings.TrimRight(string(b), "\r\n")
	} else {
		var err error
		if pw, err = readPassword("Backup password: ", creating); err != nil {
			return "", err
		}
	}
	// Enforced for --password-file and ENTRYPOINT_BACKUP_PASSWORD too (Strix vuln-0005).
	if creating && len(pw) < 12 {
		return "", errors.New("backup passphrase must be at least 12 characters")
	}
	return pw, nil
}

func cmdBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	out := fs.String("o", "", "output file (default entrypoint-backup-<date>.age)")
	pwFile := fs.String("password-file", "", "password file")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("backup requires root (sudo)")
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
	fmt.Printf("Backup written: %s (version %s, revision %d).\n", *out, m.Version, m.LastApplied)
	fmt.Println("Keep the file and the password separately, off this machine.")
	return nil
}

func cmdRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	pwFile := fs.String("password-file", "", "password file")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return exitError{2, "usage: entrypoint restore <file>"}
	}
	if !*yes {
		fmt.Fprintln(os.Stderr, "Restore replaces the gateway's current state with the backup.")
		fmt.Fprintln(os.Stderr, "All restored tokens are invalidated; you will need to issue new ones.")
		if !confirm("Continue?") {
			return exitError{1, "aborted"}
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
	fmt.Printf("Restored the backup from %s (version %s, revision %d).\n", m.CreatedAt.Local().Format(time.DateTime), m.Version, m.LastApplied)
	fmt.Printf("Invalidated %d token(s). Issue new ones: sudo entrypoint token create --service <name>\n", n)
	fmt.Println("If the IP address changed, update the DNS records (sudo entrypoint dns check).")
	cfg, _ := loadConfig()
	c := api.NewClient(cfg.APISocket)
	var svcs []api.ServiceInfo
	if err := c.Do(cx, "GET", "/v1/services", nil, &svcs); err == nil && len(svcs) > 0 {
		names := make([]string, 0, len(svcs))
		for _, s := range svcs {
			names = append(names, s.Name)
		}
		sort.Strings(names)
		fmt.Println("Services that need new tokens:", strings.Join(names, ", "))
	}
	return nil
}
