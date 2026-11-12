// Command server runs the Acme Support integration service.
//
// Usage:
//
//	server              run the service
//	server healthcheck  probe the local /health endpoint (for distroless
//	                    images, which ship without curl or a shell)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wmiha/vyking-3p-integration-service/internal/app"
	"github.com/wmiha/vyking-3p-integration-service/internal/config"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/logging"
)

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 && args[0] != "healthcheck" {
		fmt.Fprintf(os.Stderr, "unknown command %q\nusage: server [healthcheck]\n", args[0])
		return 2
	}

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	if len(args) > 0 {
		return healthcheck(cfg.Server.Internal.Addr)
	}

	logger := logging.New(os.Stdout, cfg.Log.Level, cfg.Log.Format).
		With("service", cfg.App.Name, "env", cfg.App.Env, "version", version)
	// Packages that log through slog's default (e.g. apierror) share the same
	// handler, format and service attributes.
	slog.SetDefault(logger)

	a, err := app.New(cfg, logger)
	if err != nil {
		logger.Error("start-up failed", "error", err)
		return 1
	}
	if err := a.Run(shutdownOnSignal(logger)); err != nil {
		logger.Error("service failed", "error", err)
		return 1
	}
	return 0
}

// shutdownOnSignal returns a context cancelled by the first SIGINT/SIGTERM,
// which starts the graceful shutdown. A second signal forces an immediate
// exit. This is handled explicitly instead of restoring the default signal
// disposition, which may have been inherited as "ignore" (for example by
// background jobs of non-interactive shells).
func shutdownOnSignal(logger *slog.Logger) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-sig
		logger.Info("shutdown signal received", "signal", s.String())
		cancel()

		s = <-sig
		logger.Warn("second signal received, forcing exit", "signal", s.String())
		os.Exit(1)
	}()
	return ctx
}

func healthcheck(internalAddr string) int {
	host, port, err := net.SplitHostPort(internalAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: unexpected status", resp.Status)
		return 1
	}
	return 0
}
