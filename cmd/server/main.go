// Command server runs the Acme Support integration service.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/wmiha/vyking-3p-integration-service/internal/config"
)

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	slog.Info("configuration loaded", "app", cfg.App.Name, "env", cfg.App.Env, "version", version)
}
