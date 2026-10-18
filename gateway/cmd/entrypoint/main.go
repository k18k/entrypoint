// entrypoint: CLI administracyjne bramy.
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

const usage = `entrypoint — brama publikowania usług (v%s)

Użycie:
  entrypoint install --gateway-name <nazwa> --email <e-mail> [opcje]
  entrypoint status [--json]
  entrypoint service add <nazwa> [--health <ścieżka>]
  entrypoint service rm <nazwa>
  entrypoint service list [--json]
  entrypoint route add <host>[/prefiks] (--to <ip:port> | --service <nazwa>) [opcje]
  entrypoint route list [--json]
  entrypoint route rm <id>
  entrypoint token create --service <nazwa> [--label <opis>] [--connect-only]
  entrypoint token list [--json]
  entrypoint token revoke <id> [--now]
  entrypoint dns check
  entrypoint ca status | entrypoint ca rotate start|switch|finish
  entrypoint revision list | entrypoint revision show <n>
  entrypoint backup [-o <plik>]
  entrypoint restore <plik>
  entrypoint logs <entrypointd|caddy|frps> [-f] [-n <linie>]
  entrypoint update
  entrypoint uninstall [--purge] [--yes]
  entrypoint version

Polecenia administracyjne wymagają sudo. Każde polecenie zmieniające konfigurację
tworzy rewizję i wypisuje jej numer oraz wynik stosowania.
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
				fmt.Fprintln(os.Stderr, "błąd:", redact.String(ee.msg))
			}
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, "błąd:", redact.String(err.Error()))
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
	return exitError{2, fmt.Sprintf("nieznane polecenie %q; zobacz: entrypoint help", cmd)}
}

func sub(args []string, cmds map[string]func([]string) error) error {
	if len(args) == 0 {
		names := make([]string, 0, len(cmds))
		for k := range cmds {
			names = append(names, k)
		}
		return exitError{2, "brak podpolecenia; dostępne: " + strings.Join(names, ", ")}
	}
	f, ok := cmds[args[0]]
	if !ok {
		return exitError{2, fmt.Sprintf("nieznane podpolecenie %q", args[0])}
	}
	return f(args[1:])
}

// parse obsługuje flagi przemieszane z argumentami pozycyjnymi.
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
			return c, errors.New("brama nie jest zainstalowana (brak " + configPath() + "); uruchom: sudo entrypoint install")
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

// printChange wypisuje wynik polecenia zmieniającego konfigurację.
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
			fmt.Printf("Rewizja %d: zastosowana (%s).\n", res.Revision.Num, res.Revision.Summary)
		} else {
			fmt.Printf("Rewizja %d: NIE zastosowana, status %s.\n", res.Revision.Num, res.Revision.Status)
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
	return fs.Int64("base-rev", -1, "rewizja bazowa (odrzuć zmianę, jeśli powstała nowsza)")
}

func baseRev(v *int64) *int64 {
	if *v < 0 {
		return nil
	}
	return v
}

func confirm(prompt string) bool {
	fmt.Fprint(os.Stderr, prompt+" [t/N] ")
	var s string
	fmt.Scanln(&s)
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "t" || s == "tak" || s == "y" || s == "yes"
}
