// entrypoint-connector: konektor tunelu nad oficjalną binarką frpc.
//
//	ENTRYPOINT_CONNECT=... entrypoint-connector tunnel --service api --upstream http://127.0.0.1:3000
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/k18k/entrypoint/gateway/internal/connector"
	"github.com/k18k/entrypoint/gateway/internal/redact"
	"github.com/k18k/entrypoint/gateway/internal/token"
	"github.com/k18k/entrypoint/gateway/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "użycie: entrypoint-connector tunnel|rebundle|version [opcje]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "tunnel":
		os.Exit(tunnel(os.Args[2:]))
	case "rebundle":
		os.Exit(rebundle(os.Args[2:]))
	case "version", "--version":
		fmt.Println(version.Version)
	default:
		fmt.Fprintf(os.Stderr, "nieznane polecenie %q\n", os.Args[1])
		os.Exit(2)
	}
}

func fail(msg string) int {
	fmt.Fprintln(os.Stderr, "entrypoint-connector: "+redact.String(msg))
	return connector.ExitError
}

func tunnel(args []string) int {
	fs := flag.NewFlagSet("tunnel", flag.ContinueOnError)
	service := fs.String("service", "", "serwis (domyślnie z pakietu połączenia)")
	upstream := fs.String("upstream", "", "lokalny adres aplikacji, np. http://127.0.0.1:3000")
	health := fs.String("health", "", "ścieżka health checku HTTP, np. /health (domyślnie TCP)")
	interval := fs.Int("health-interval", 2, "interwał health checku w sekundach")
	maxFail := fs.Int("health-max-failed", 2, "liczba nieudanych sprawdzeń do wycofania repliki")
	drain := fs.Duration("drain-timeout", 30*time.Second, "maksymalny czas drainu")
	replica := fs.String("replica", "", "nazwa repliki (domyślnie z nazwy hosta)")
	frpc := fs.String("frpc", "", "ścieżka do frpc (domyślnie obok konektora albo ENTRYPOINT_FRPC)")
	parentPipe := fs.Bool("parent-pipe", false, "zakończ, gdy zamknie się standardowe wejście (proces rodzica zginął)")
	insecure := fs.Bool("insecure-dev-skip-verify", false, "TYLKO DEV: nie weryfikuj certyfikatu bramy")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	raw := os.Getenv("ENTRYPOINT_CONNECT")
	if raw == "" {
		return fail("brak zmiennej ENTRYPOINT_CONNECT (pakiet z: sudo entrypoint token create --service <nazwa>)")
	}
	b, err := token.DecodeBundle(raw)
	if err != nil {
		return fail(err.Error())
	}
	if *upstream == "" {
		return fail("podaj --upstream, np. http://127.0.0.1:3000")
	}
	c, err := connector.New(connector.Options{
		Bundle: b, Service: *service, Upstream: *upstream, Health: *health,
		HealthInterval: *interval, HealthMaxFail: *maxFail, DrainTimeout: *drain,
		Replica: *replica, FrpcPath: *frpc, ParentPipe: *parentPipe, InsecureDev: *insecure,
	})
	if err != nil {
		return fail(err.Error())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmds := make(chan connector.Command, 4)
	if *parentPipe {
		in := connector.ReadCommands(os.Stdin)
		go func() {
			for c := range in {
				cmds <- c
			}
			close(cmds)
		}()
	}
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		cmds <- connector.Command{Cmd: "drain"}
		<-sig
		fmt.Fprintln(os.Stderr, "entrypoint-connector: drugi sygnał, kończę natychmiast")
		cancel()
	}()
	return c.Run(ctx, cmds)
}

// rebundle podmienia listę CA w pakiecie połączenia (rotacja CA bez wydawania nowego tokenu).
func rebundle(args []string) int {
	fs := flag.NewFlagSet("rebundle", flag.ContinueOnError)
	caFile := fs.String("ca", "", "plik PEM z zaufanymi CA (z: sudo entrypoint ca status --json)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	b, err := token.DecodeBundle(os.Getenv("ENTRYPOINT_CONNECT"))
	if err != nil {
		return fail(err.Error())
	}
	pemData, err := os.ReadFile(*caFile)
	if err != nil {
		return fail(err.Error())
	}
	var cas []string
	for _, blk := range strings.SplitAfter(string(pemData), "-----END CERTIFICATE-----") {
		blk = strings.TrimSpace(blk)
		if strings.HasPrefix(blk, "-----BEGIN CERTIFICATE-----") {
			cas = append(cas, blk+"\n")
		}
	}
	if len(cas) == 0 {
		return fail("plik nie zawiera certyfikatów")
	}
	b.CAs = cas
	enc, err := b.Encode()
	if err != nil {
		return fail(err.Error())
	}
	fmt.Println(enc)
	return 0
}
