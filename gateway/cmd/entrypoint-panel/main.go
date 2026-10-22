// entrypoint-panel: the web panel. It listens on loopback behind Caddy and talks to entrypointd
// through its API socket.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/k18k/entrypoint/gateway/internal/config"
	"github.com/k18k/entrypoint/gateway/internal/panel"
	"github.com/k18k/entrypoint/gateway/internal/redact"
	"github.com/k18k/entrypoint/gateway/internal/version"
)

func main() {
	cfgPath := flag.String("config", config.DefaultConfigPath, "configuration file")
	flag.Parse()
	log := slog.New(redact.Handler{Handler: slog.NewTextHandler(os.Stderr, nil)})
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "entrypoint-panel:", err)
		os.Exit(1)
	}
	if !cfg.Panel.Enabled() {
		fmt.Fprintln(os.Stderr, "entrypoint-panel: the panel is not set up (entrypoint install --panel-name)")
		os.Exit(1)
	}
	srv := &http.Server{
		Addr:              cfg.Panel.Listen,
		Handler:           panel.New(cfg, panel.UI(), log).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("entrypoint-panel running", "listen", cfg.Panel.Listen, "name", cfg.Panel.Name, "version", version.Version)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("panel stopped", "error", err)
		os.Exit(1)
	}
}
