package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/k18k/entrypoint/gateway/internal/config"
	"github.com/k18k/entrypoint/gateway/internal/install"
)

func vpnConfig() (config.Config, error) {
	cfg, err := loadConfig()
	if err != nil {
		return cfg, err
	}
	if !cfg.VPN.Enabled() {
		return cfg, exitError{1, "the VPN is not set up; reinstall with: sudo entrypoint install --vpn-name vpn.example.com"}
	}
	return cfg, nil
}

func cmdVPNKey(args []string) error {
	fs := flag.NewFlagSet("vpn key", flag.ContinueOnError)
	reusable := fs.Bool("reusable", false, "the key can join more than one machine")
	exp := fs.Duration("expiration", time.Hour, "how long the key is valid")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := vpnConfig()
	if err != nil {
		return err
	}
	key, err := install.PreAuthKey(cfg, exp.String(), *reusable)
	if err != nil {
		return err
	}
	// The key is shown once, like a tunnel token.
	fmt.Printf("Join a machine (valid for %s%s):\n\n", exp, map[bool]string{true: ", reusable", false: ", single use"}[*reusable])
	fmt.Printf("  tailscale up --login-server https://%s --auth-key %s\n\n", cfg.VPN.Name, key)
	fmt.Println("Then route to it: entrypoint route add <host> --to <node address>:<port> (see: entrypoint vpn nodes)")
	return nil
}

type vpnNode struct {
	ID        uint64   `json:"id"`
	Name      string   `json:"given_name"`
	Addresses []string `json:"ip_addresses"`
	Online    bool     `json:"online"`
	LastSeen  struct {
		Seconds int64 `json:"seconds"`
	} `json:"last_seen"`
}

func cmdVPNNodes(args []string) error {
	fs := flag.NewFlagSet("vpn nodes", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := vpnConfig()
	if err != nil {
		return err
	}
	out, err := install.Headscale(cfg, "nodes", "list", "-o", "json")
	if err != nil {
		return err
	}
	var nodes []vpnNode
	if err := json.Unmarshal(out, &nodes); err != nil {
		return fmt.Errorf("headscale nodes list: %w", err)
	}
	if *asJSON {
		return printJSON(nodes)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tADDRESSES\tSTATE")
	for _, n := range nodes {
		state := "online"
		if !n.Online {
			state = "offline"
			if n.LastSeen.Seconds > 0 {
				state += ", last seen " + time.Unix(n.LastSeen.Seconds, 0).UTC().Format(time.RFC3339)
			}
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", n.ID, n.Name, strings.Join(n.Addresses, " "), state)
	}
	return w.Flush()
}

func cmdVPNRemove(args []string) error {
	fs := flag.NewFlagSet("vpn rm", flag.ContinueOnError)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return exitError{2, "usage: entrypoint vpn rm <id>"}
	}
	id, err := strconv.ParseUint(pos[0], 10, 64)
	if err != nil {
		return exitError{2, "the node ID is a number; see: entrypoint vpn nodes"}
	}
	cfg, err := vpnConfig()
	if err != nil {
		return err
	}
	if _, err := install.Headscale(cfg, "nodes", "delete", "-i", strconv.FormatUint(id, 10), "-o", "json"); err != nil {
		return err
	}
	fmt.Printf("Removed node %d from the VPN.\n", id)
	return nil
}
