// entrypoint: the gateway's admin CLI.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/k18k/entrypoint/gateway/internal/api"
	"github.com/k18k/entrypoint/gateway/internal/config"
	"github.com/k18k/entrypoint/gateway/internal/redact"
	"github.com/k18k/entrypoint/gateway/internal/version"
)

const usage = `entrypoint — service publishing gateway (v%s)

Usage:
  entrypoint install --gateway-name <name> --email <e-mail> [--vpn-name <name>] [--panel-name <name>] [options]
  entrypoint status [--json]
  entrypoint service add <name> [--health <path>]
  entrypoint service rm <name>
  entrypoint service list [--json]
  entrypoint route add <host>[/prefix] (--to <ip:port> | --service <name>) [options]
  entrypoint route list [--json]
  entrypoint route rm <id>
  entrypoint token create --service <name> [--label <description>] [--connect-only]
  entrypoint token list [--json]
  entrypoint token revoke <id> [--now]
  entrypoint dns check
  entrypoint vpn key [--reusable] [--expiration <duration>] [--bundle]
  entrypoint vpn nodes [--json]
  entrypoint vpn rm <id>
  entrypoint panel register
  entrypoint panel keys [rm <id>]
  entrypoint ca status | entrypoint ca rotate start|switch|finish
  entrypoint revision list | entrypoint revision show <n>
  entrypoint backup [-o <file>]
  entrypoint restore <file>
  entrypoint logs <entrypointd|caddy|frps|headscale|tailscaled|panel> [-f] [-n <lines>]
  entrypoint update
  entrypoint uninstall [--purge] [--yes]
  entrypoint version

Admin commands need sudo. Every command that changes the configuration
creates a revision and prints its number and the apply result.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, version.Version)
		os.Exit(2)
	}
	err := dispatch(os.Args[1], os.Args[2:])
	if err != nil {
		var ee exitError
		if errors.As(err, &ee) {
			if ee.msg != "" {
				fmt.Fprintln(os.Stderr, "error:", redact.String(ee.msg))
			}
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, "error:", redact.String(err.Error()))
		os.Exit(1)
	}
}

type exitError struct {
	code int
	msg  string
}

func (e exitError) Error() string { return e.msg }

func dispatch(cmd string, args []string) error {
	switch cmd {
	case "install":
		return cmdInstall(args)
	case "uninstall":
		return cmdUninstall(args)
	case "status":
		return cmdStatus(args)
	case "service":
		return sub(args, map[string]func([]string) error{"add": cmdServiceAdd, "rm": cmdServiceRm, "list": cmdServiceList})
	case "route":
		return sub(args, map[string]func([]string) error{"add": cmdRouteAdd, "rm": cmdRouteRm, "list": cmdRouteList})
	case "token":
		return sub(args, map[string]func([]string) error{"create": cmdTokenCreate, "list": cmdTokenList, "revoke": cmdTokenRevoke})
	case "vpn":
		return sub(args, map[string]func([]string) error{"key": cmdVPNKey, "nodes": cmdVPNNodes, "rm": cmdVPNRemove})
	case "panel":
		if len(args) > 0 && args[0] == "--register" {
			args[0] = "register"
		}
		return sub(args, map[string]func([]string) error{"register": cmdPanelRegister, "keys": cmdPanelKeys})
	case "dns":
		return sub(args, map[string]func([]string) error{"check": cmdDNSCheck})
	case "ca":
		return sub(args, map[string]func([]string) error{"status": cmdCAStatus, "rotate": cmdCARotate})
	case "revision":
		return sub(args, map[string]func([]string) error{"list": cmdRevisionList, "show": cmdRevisionShow})
	case "backup":
		return cmdBackup(args)
	case "restore":
		return cmdRestore(args)
	case "logs":
		return cmdLogs(args)
	case "update":
		return cmdUpdate(args)
	case "version", "--version", "-v":
		fmt.Println(version.Version)
		return nil
	case "help", "--help", "-h":
		fmt.Printf(usage, version.Version)
		return nil
	}
	return exitError{2, fmt.Sprintf("unknown command %q; see: entrypoint help", cmd)}
}

func sub(args []string, cmds map[string]func([]string) error) error {
	if len(args) == 0 {
		names := make([]string, 0, len(cmds))
		for k := range cmds {
			names = append(names, k)
		}
		return exitError{2, "missing subcommand; available: " + strings.Join(names, ", ")}
	}
	f, ok := cmds[args[0]]
	if !ok {
		return exitError{2, fmt.Sprintf("unknown subcommand %q", args[0])}
	}
	return f(args[1:])
}

// parse handles flags mixed with positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(os.Stderr)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, exitError{2, ""}
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func configPath() string {
	if p := os.Getenv("ENTRYPOINT_CONFIG"); p != "" {
		return p
	}
	return config.DefaultConfigPath
}

func loadConfig() (config.Config, error) {
	c, err := config.Load(configPath())
	if err != nil {
		if os.IsNotExist(err) {
			return c, errors.New("the gateway is not installed (missing " + configPath() + "); run: sudo entrypoint install")
		}
		return c, err
	}
	return c, nil
}

func client() (*api.Client, error) {
	c, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return api.NewClient(c.APISocket), nil
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Minute)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// printChange prints the result of a command that changes the configuration.
func printChange(res *api.ChangeResult, err error, asJSON bool) error {
	var ae *api.Error
	if errors.As(err, &ae) && ae.Result != nil {
		res = ae.Result
	} else if err != nil {
		return err
	}
	if asJSON {
		if err := printJSON(res); err != nil {
			return err
		}
	} else if res.Revision != nil {
		if res.Applied {
			fmt.Printf("Revision %d: applied (%s).\n", res.Revision.Num, res.Revision.Summary)
		} else {
			fmt.Printf("Revision %d: NOT applied, status %s.\n", res.Revision.Num, res.Revision.Status)
		}
		for _, d := range res.Revision.Diff {
			fmt.Println("  " + d)
		}
	}
	if !res.Applied {
		return exitError{1, res.Error}
	}
	return nil
}

func baseRevFlag(fs *flag.FlagSet) *int64 {
	return fs.Int64("base-rev", -1, "base revision (reject the change if a newer one exists)")
}

func baseRev(v *int64) *int64 {
	if *v < 0 {
		return nil
	}
	return v
}

func confirm(prompt string) bool {
	fmt.Fprint(os.Stderr, prompt+" [y/N] ")
	var s string
	fmt.Scanln(&s)
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "y" || s == "yes"
}
