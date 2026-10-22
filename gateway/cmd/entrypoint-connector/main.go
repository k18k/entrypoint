// entrypoint-connector: the tunnel connector around the official frpc binary, and a
// userspace node of the gateway's VPN.
//
//	ENTRYPOINT_CONNECT=... entrypoint-connector tunnel --service api --upstream http://127.0.0.1:3000
//	ENTRYPOINT_VPN=... entrypoint-connector vpn --name db --expose 5432
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
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
		fmt.Fprintln(os.Stderr, "usage: entrypoint-connector tunnel|vpn|rebundle|version [options]")
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	if strings.HasPrefix(cmd, "-") {
		cmd, args = "tunnel", os.Args[1:] // the image's ENTRYPOINT used to include "tunnel"
	}
	switch cmd {
	case "tunnel":
		os.Exit(tunnel(args))
	case "vpn":
		os.Exit(vpn(args))
	case "rebundle":
		os.Exit(rebundle(args))
	case "version", "--version":
		fmt.Println(version.Version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		os.Exit(2)
	}
}

func fail(msg string) int {
	fmt.Fprintln(os.Stderr, "entrypoint-connector: "+redact.String(msg))
	return connector.ExitError
}

func tunnel(args []string) int {
	fs := flag.NewFlagSet("tunnel", flag.ContinueOnError)
	service := fs.String("service", "", "service (default: from the connection bundle)")
	upstream := fs.String("upstream", "", "local app address, e.g. http://127.0.0.1:3000")
	health := fs.String("health", "", "HTTP health check path, e.g. /health (default: TCP)")
	interval := fs.Int("health-interval", 2, "health check interval in seconds")
	maxFail := fs.Int("health-max-failed", 2, "failed checks before the replica is withdrawn")
	drain := fs.Duration("drain-timeout", 30*time.Second, "maximum drain time")
	replica := fs.String("replica", "", "replica name (default: from the host name)")
	frpc := fs.String("frpc", "", "path to frpc (default: next to the connector or ENTRYPOINT_FRPC)")
	parentPipe := fs.Bool("parent-pipe", false, "exit when standard input closes (the parent process died)")
	insecure := fs.Bool("insecure-dev-skip-verify", false, "DEV ONLY: do not verify the gateway certificate")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	raw := os.Getenv("ENTRYPOINT_CONNECT")
	if raw == "" {
		return fail("ENTRYPOINT_CONNECT is not set (get a bundle with: sudo entrypoint token create --service <name>)")
	}
	b, err := token.DecodeBundle(raw)
	if err != nil {
		return fail(err.Error())
	}
	if *upstream == "" {
		return fail("pass --upstream, e.g. http://127.0.0.1:3000")
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
		fmt.Fprintln(os.Stderr, "entrypoint-connector: second signal, exiting now")
		cancel()
	}()
	return c.Run(ctx, cmds)
}

// rebundle replaces the CA list in a connection bundle (CA rotation without issuing a new token).
func rebundle(args []string) int {
	fs := flag.NewFlagSet("rebundle", flag.ContinueOnError)
	caFile := fs.String("ca", "", "PEM file with trusted CAs (from: sudo entrypoint ca status --json)")
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
		return fail("the file contains no certificates")
	}
	b.CAs = cas
	enc, err := b.Encode()
	if err != nil {
		return fail(err.Error())
	}
	fmt.Println(enc)
	return 0
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// vpn joins the gateway's VPN as a userspace node and serves --expose and --forward.
func vpn(args []string) int {
	fs := flag.NewFlagSet("vpn", flag.ContinueOnError)
	name := fs.String("name", "", "node name in the VPN, e.g. vatro-postgres (other machines connect to <name>:<port>)")
	stateDir := fs.String("state-dir", "", "node identity directory (default: <user config dir>/entrypoint/vpn-<name>)")
	var expose, forward multi
	fs.Var(&expose, "expose", "publish a local port in the VPN: <port>[=<host:port>], e.g. 5432 or 5432=127.0.0.1:5432; repeatable")
	fs.Var(&forward, "forward", "reach a VPN service locally: [<listen host:port>=]<name:port>, e.g. vatro-postgres:5432; repeatable")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	raw := os.Getenv("ENTRYPOINT_VPN")
	if raw == "" {
		return fail("ENTRYPOINT_VPN is not set (get a bundle in the panel, VPN > Add service, or with: sudo entrypoint vpn key --bundle)")
	}
	b, err := token.DecodeVPNBundle(raw)
	if err != nil {
		return fail(err.Error())
	}
	if !nodeName.MatchString(*name) {
		return fail("pass --name: lowercase letters, digits and dashes, e.g. vatro-postgres")
	}
	o := connector.VPNOptions{Bundle: b, Name: *name, StateDir: *stateDir,
		Log: slog.New(redact.Handler{Handler: slog.NewTextHandler(os.Stderr, nil)})}
	if o.StateDir == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return fail("pass --state-dir: " + err.Error())
		}
		o.StateDir = filepath.Join(dir, "entrypoint", "vpn-"+*name)
	}
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		return fail(err.Error())
	}
	for _, e := range expose {
		x, err := connector.ParseExpose(e)
		if err != nil {
			return fail(err.Error())
		}
		o.Expose = append(o.Expose, x)
	}
	for _, f := range forward {
		x, err := connector.ParseForward(f)
		if err != nil {
			return fail(err.Error())
		}
		o.Forward = append(o.Forward, x)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := connector.RunVPN(ctx, o); err != nil {
		return fail(err.Error())
	}
	return 0
}

var nodeName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
