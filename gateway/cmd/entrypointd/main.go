// entrypointd: the gateway's control plane.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/k18k/entrypoint/gateway/internal/config"
	"github.com/k18k/entrypoint/gateway/internal/daemon"
	"github.com/k18k/entrypoint/gateway/internal/redact"
	"github.com/k18k/entrypoint/gateway/internal/version"
)

func main() {
	cfgPath := flag.String("config", config.DefaultConfigPath, "configuration file")
	showVersion := flag.Bool("version", false, "print the version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Version)
		return
	}
	log := slog.New(redact.Handler{Handler: slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})})
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("configuration", "err", err)
		os.Exit(1)
	}
	d, err := daemon.New(cfg, log)
	if err != nil {
		log.Error("start", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := d.Run(ctx); err != nil {
		log.Error("entrypointd exited with an error", "err", err)
		os.Exit(1)
	}
}
