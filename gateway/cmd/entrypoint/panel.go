package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/k18k/entrypoint/gateway/internal/config"
	"github.com/k18k/entrypoint/gateway/internal/panel"
)

func panelConfig() (config.Config, error) {
	cfg, err := loadConfig()
	if err != nil {
		return cfg, err
	}
	if !cfg.Panel.Enabled() {
		return cfg, exitError{1, "the panel is not set up; reinstall with: sudo entrypoint install --panel-name panel.example.com"}
	}
	if os.Geteuid() != 0 {
		return cfg, exitError{1, "managing panel passkeys requires root (sudo)"}
	}
	return cfg, nil
}

// cmdPanelRegister prints a one-time link that adds a passkey to the panel.
func cmdPanelRegister(args []string) error {
	fs := flag.NewFlagSet("panel register", flag.ContinueOnError)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := panelConfig()
	if err != nil {
		return err
	}
	tok, exp, err := panel.NewRegistration(cfg)
	if err != nil {
		return err
	}
	// The token is in the fragment: browsers never send it to the server or in a Referer.
	fmt.Printf("Open within %d seconds (until %s) to add a passkey, e.g. in a password manager or on a security key:\n\n", int(panel.RegistrationTTL.Seconds()), exp.Local().Format("15:04:05"))
	fmt.Printf("  https://%s/register#%s\n\n", cfg.Panel.Name, tok)
	fmt.Println("The link works once. Adding a passkey signs out every open session.")
	return nil
}

func cmdPanelKeys(args []string) error {
	if len(args) > 0 && args[0] == "rm" {
		if len(args) != 2 {
			return exitError{2, "usage: entrypoint panel keys rm <id>"}
		}
		cfg, err := panelConfig()
		if err != nil {
			return err
		}
		if err := panel.RemoveKey(cfg, args[1]); err != nil {
			return err
		}
		fmt.Printf("Removed passkey %s; every panel session is signed out.\n", args[1])
		return nil
	}
	fs := flag.NewFlagSet("panel keys", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := panelConfig()
	if err != nil {
		return err
	}
	keys, err := panel.LoadKeys(cfg)
	if err != nil {
		return err
	}
	type row struct {
		ID        string     `json:"id"`
		Name      string     `json:"name"`
		CreatedAt time.Time  `json:"createdAt"`
		LastUsed  *time.Time `json:"lastUsed,omitempty"`
	}
	rows := []row{}
	for _, k := range keys.Keys {
		rows = append(rows, row{k.ID, k.Name, k.CreatedAt, k.LastUsed})
	}
	if *asJSON {
		return printJSON(rows)
	}
	if len(rows) == 0 {
		fmt.Println("No passkeys. Add one: sudo entrypoint panel register")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tADDED\tLAST USED")
	for _, r := range rows {
		last := "never"
		if r.LastUsed != nil {
			last = r.LastUsed.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.ID, r.Name, r.CreatedAt.Local().Format("2006-01-02 15:04"), last)
	}
	return w.Flush()
}
