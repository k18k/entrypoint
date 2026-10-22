package connector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"tailscale.com/envknob"
	"tailscale.com/tsnet"

	"github.com/k18k/entrypoint/gateway/internal/token"
)

// Expose publishes a local TCP address on a port of the connector's VPN node.
type Expose struct {
	Port  int    // on the VPN
	Local string // host:port on this machine
}

// Forward listens locally and connects to an address in the VPN.
type Forward struct {
	Listen string // host:port on this machine
	Target string // name:port or address:port in the VPN
}

// VPNOptions configure the connector as a VPN node (userspace, no root or TUN device).
type VPNOptions struct {
	Bundle   token.VPNBundle
	Name     string // node name; other machines reach it as <name>
	StateDir string // node identity; keep it so restarts don't need a new key
	Expose   []Expose
	Forward  []Forward
	Log      *slog.Logger
}

// ParseExpose parses "5432=127.0.0.1:5432" or "5432" (the same port on 127.0.0.1).
func ParseExpose(s string) (Expose, error) {
	p, local, ok := strings.Cut(s, "=")
	port, err := strconv.Atoi(p)
	if err != nil || port < 1 || port > 65535 {
		return Expose{}, fmt.Errorf("--expose %q: expected <port>[=<host:port>]", s)
	}
	if !ok {
		local = "127.0.0.1:" + p
	}
	if _, _, err := net.SplitHostPort(local); err != nil {
		return Expose{}, fmt.Errorf("--expose %q: %v", s, err)
	}
	return Expose{Port: port, Local: local}, nil
}

// ParseForward parses "127.0.0.1:5432=db:5432" or "db:5432" (listen on 127.0.0.1, same port).
func ParseForward(s string) (Forward, error) {
	listen, target, ok := strings.Cut(s, "=")
	if !ok {
		target = listen
		_, port, err := net.SplitHostPort(target)
		if err != nil {
			return Forward{}, fmt.Errorf("--forward %q: expected [<listen host:port>=]<name:port>", s)
		}
		listen = "127.0.0.1:" + port
	}
	for _, a := range []string{listen, target} {
		if _, _, err := net.SplitHostPort(a); err != nil {
			return Forward{}, fmt.Errorf("--forward %q: %v", s, err)
		}
	}
	return Forward{Listen: listen, Target: target}, nil
}

// RunVPN joins the VPN and serves the exposed and forwarded ports until ctx ends.
func RunVPN(ctx context.Context, o VPNOptions) error {
	if len(o.Expose) == 0 && len(o.Forward) == 0 {
		return errors.New("nothing to do: pass --expose and/or --forward")
	}
	// Logs stay on this machine, never with Tailscale Inc.
	envknob.SetNoLogsNoSupport()
	s := &tsnet.Server{
		Hostname:   o.Name,
		Dir:        o.StateDir,
		ControlURL: o.Bundle.Server,
		AuthKey:    o.Bundle.Key,
		Logf:       func(string, ...any) {},
		UserLogf:   func(f string, a ...any) { o.Log.Info(fmt.Sprintf(f, a...)) },
	}
	defer s.Close()
	st, err := s.Up(ctx)
	if err != nil {
		return fmt.Errorf("joining the VPN at %s: %w", o.Bundle.Server, err)
	}
	var addrs []string
	for _, ip := range st.TailscaleIPs {
		addrs = append(addrs, ip.String())
	}
	o.Log.Info("joined the VPN", "name", o.Name, "addresses", strings.Join(addrs, " "))

	var lns []net.Listener
	defer func() {
		for _, ln := range lns {
			ln.Close()
		}
	}()
	var wg sync.WaitGroup
	for _, e := range o.Expose {
		ln, err := s.Listen("tcp", fmt.Sprintf(":%d", e.Port))
		if err != nil {
			return fmt.Errorf("--expose %d: %w", e.Port, err)
		}
		lns = append(lns, ln)
		o.Log.Info("exposing", "vpn", fmt.Sprintf("%s:%d", o.Name, e.Port), "local", e.Local)
		local := e.Local
		wg.Add(1)
		go func() {
			defer wg.Done()
			serve(ln, func(ctx context.Context) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", local)
			}, o.Log)
		}()
	}
	for _, f := range o.Forward {
		ln, err := net.Listen("tcp", f.Listen)
		if err != nil {
			return fmt.Errorf("--forward %s: %w", f.Listen, err)
		}
		lns = append(lns, ln)
		o.Log.Info("forwarding", "local", f.Listen, "vpn", f.Target)
		target := f.Target
		wg.Add(1)
		go func() {
			defer wg.Done()
			serve(ln, func(ctx context.Context) (net.Conn, error) { return s.Dial(ctx, "tcp", target) }, o.Log)
		}()
	}
	<-ctx.Done()
	for _, ln := range lns {
		ln.Close()
	}
	wg.Wait()
	return nil
}

// serve accepts connections and pipes each one to a new connection from dial.
func serve(ln net.Listener, dial func(context.Context) (net.Conn, error), log *slog.Logger) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			up, err := dial(context.Background())
			if err != nil {
				log.Warn("connection failed", "from", c.RemoteAddr().String(), "error", err)
				return
			}
			defer up.Close()
			done := make(chan struct{}, 2)
			pipe := func(dst, src net.Conn) {
				io.Copy(dst, src)
				if cw, ok := dst.(interface{ CloseWrite() error }); ok {
					cw.CloseWrite()
				}
				done <- struct{}{}
			}
			go pipe(up, c)
			go pipe(c, up)
			<-done
			<-done
		}()
	}
}
