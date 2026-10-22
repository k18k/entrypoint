package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/k18k/entrypoint/gateway/internal/api"
)

func cmdVPNKey(args []string) error {
	fs := flag.NewFlagSet("vpn key", flag.ContinueOnError)
	reusable := fs.Bool("reusable", false, "the key can join more than one machine")
	exp := fs.Duration("expiration", time.Hour, "how long the key is valid")
	bundle := fs.Bool("bundle", false, "print only ENTRYPOINT_VPN for the connector")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	c, err := client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	var k api.VPNKey
	if err := c.Do(cx, "POST", "/v1/vpn/keys", api.VPNKeyCreate{Reusable: *reusable, Expiration: exp.String()}, &k); err != nil {
		return err
	}
	if *bundle {
		fmt.Println(k.Bundle)
		return nil
	}
	// The key is shown once, like a tunnel token.
	use := "single use"
	if k.Reusable {
		use = "reusable"
	}
	fmt.Printf("Key valid until %s (%s). Join a machine with the Tailscale client:\n\n", k.Expiration.Format(time.RFC3339), use)
	fmt.Printf("  tailscale up --login-server %s --auth-key %s\n\n", k.Server, k.Key)
	fmt.Printf("Or publish a local port with the connector (no Tailscale needed):\n\n")
	fmt.Printf("  ENTRYPOINT_VPN=%s entrypoint-connector vpn --name <name> --expose <port>\n\n", k.Bundle)
	fmt.Println("Then route to it: entrypoint route add <host> --to <node address>:<port> (see: entrypoint vpn nodes)")
	return nil
}

func cmdVPNNodes(args []string) error {
	fs := flag.NewFlagSet("vpn nodes", flag.ContinueOnError)
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
	var v api.VPN
	if err := c.Do(cx, "GET", "/v1/vpn", nil, &v); err != nil {
		return err
	}
	if *asJSON {
		return printJSON(v)
	}
	if !v.Enabled {
		return exitError{1, "the VPN is not set up; reinstall with: sudo entrypoint install --vpn-name vpn.example.com"}
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tADDRESSES\tSTATE")
	for _, n := range v.Nodes {
		state := "online"
		if !n.Online {
			state = "offline"
			if n.LastSeen != nil {
				state += ", last seen " + n.LastSeen.Format(time.RFC3339)
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
	if _, err := strconv.ParseUint(pos[0], 10, 64); err != nil {
		return exitError{2, "the node ID is a number; see: entrypoint vpn nodes"}
	}
	c, err := client()
	if err != nil {
		return err
	}
	cx, cancel := ctx()
	defer cancel()
	if err := c.Do(cx, "DELETE", "/v1/vpn/nodes/"+pos[0], nil, nil); err != nil {
		return err
	}
	fmt.Printf("Removed node %s from the VPN.\n", pos[0])
	return nil
}
