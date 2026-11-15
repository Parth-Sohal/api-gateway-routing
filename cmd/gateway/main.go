// Command gateway is the process entry point. It only parses flags, builds the
// logger and metrics registry, and hands both to internal/server; all request
// handling lives in packages under internal/.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/Parth-Sohal/api-gateway-routing/internal/config"
	"github.com/Parth-Sohal/api-gateway-routing/internal/metrics"
	"github.com/Parth-Sohal/api-gateway-routing/internal/observability"
	"github.com/Parth-Sohal/api-gateway-routing/internal/server"
)

// version is injected with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gateway", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "configs/gateway.yaml", "path to the YAML configuration file")
	validateOnly := fs.Bool("validate", false, "validate the configuration and exit")
	showVersion := fs.Bool("version", false, "print the version and exit")
	logLevel := fs.String("log-level", "", "override the configured log level (debug, info, warn, error)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "gateway: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}

	cfg, err := config.LoadFile(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "gateway: %v\n", err)
		return 1
	}
	if *logLevel != "" {
		level := strings.ToLower(strings.TrimSpace(*logLevel))
		switch level {
		case "debug", "info", "warn", "warning", "error":
			cfg.Log.Level = level
		default:
			fmt.Fprintf(stderr, "gateway: invalid -log-level %q (want debug, info, warn or error)\n", *logLevel)
			return 2
		}
		if err := cfg.Validate(); err != nil {
			fmt.Fprintf(stderr, "gateway: %v\n", err)
			return 1
		}
	}
	if *validateOnly {
		fmt.Fprintf(stdout, "configuration valid: %s (%d routes, %d upstreams)\n",
			*configPath, len(cfg.Routes), len(cfg.Upstreams))
		return 0
	}

	logger := observability.NewLogger(cfg.Log, version, stdout)
	registry := metrics.New(version)
	srv, err := server.New(server.Options{
		ConfigPath: *configPath,
		Config:     cfg,
		Logger:     logger,
		Metrics:    registry,
		Version:    version,
	})
	if err != nil {
		logger.Error("gateway configuration rejected", slog.String("error", err.Error()))
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		logger.Error("gateway stopped with error", slog.String("error", err.Error()))
		return 1
	}
	return 0
}
