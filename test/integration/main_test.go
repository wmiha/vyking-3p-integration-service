//go:build integration

// Package integration runs the service against a real MySQL migrated by
// Flyway (see docker-compose.test.yml and `make test-integration`).
//
// Tests write fixtures as root (the service user may not write most tables)
// and exercise the application through its HTTP API as the least-privilege
// service user, exactly like production. They run sequentially and reset the
// data they own.
package integration

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/app"
	"github.com/wmiha/vyking-3p-integration-service/internal/config"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/database"
)

// Connection settings of the throwaway database; defaults match
// docker-compose.test.yml.
var (
	dbHost        = env("IT_DB_HOST", "127.0.0.1")
	dbPort        = env("IT_DB_PORT", "33306")
	dbName        = env("IT_DB_NAME", "vyking")
	dbRootPass    = env("IT_DB_ROOT_PASSWORD", "it-root-only")
	dbAppPassword = env("IT_DB_APP_PASSWORD", "it-app-only")
)

// root has write access to every table, for fixtures and assertions.
var root *sql.DB

func TestMain(m *testing.M) {
	var err error
	root, err = database.Open(dbConfig("root", dbRootPass))
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		err = database.WaitReady(ctx, root, time.Second)
		cancel()
	}
	if err != nil {
		// Fail fast: integration tests must never silently skip in CI.
		fmt.Fprintln(os.Stderr, "integration database unavailable (run `make test-integration`):", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = root.Close()
	os.Exit(code)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func dbConfig(user, password string) database.Config {
	port, _ := strconv.Atoi(dbPort)
	return database.Config{
		Host: dbHost, Port: port, Name: dbName, User: user, Password: password, Timezone: "UTC",
		ConnectTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second,
		MaxOpenConns: 5, MaxIdleConns: 2,
	}
}

// exec runs statements as root and fails the test on error.
func exec(t *testing.T, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		_, err := root.ExecContext(context.Background(), s)
		require.NoError(t, err, s)
	}
}

// resetData empties every non-reference table.
func resetData(t *testing.T) {
	t.Helper()
	exec(t,
		"DELETE FROM partner_contacts", "DELETE FROM sync_queue", "DELETE FROM transactions",
		"DELETE FROM customers", "DELETE FROM payment_methods", "DELETE FROM brands",
		"DELETE FROM currencies WHERE CUR_iID > 5",
	)
}

const testJWTSecret = "integration-test-secret-0123456789-0123"

// serviceConfig is a complete service configuration pointing at the test
// database as the least-privilege app user, on free local ports.
func serviceConfig(t *testing.T) *config.Config {
	t.Helper()
	port, _ := strconv.Atoi(dbPort)
	srv := func(enabled bool) config.HTTPServer {
		return config.HTTPServer{Enabled: enabled, Addr: freeAddr(t),
			ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
			WriteTimeout: 15 * time.Second, IdleTimeout: time.Minute}
	}
	conn := config.DBConn{
		Host: dbHost, Port: port, Name: dbName, User: "app", Password: dbAppPassword, Timezone: "UTC",
		ConnectTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second,
		Pool: config.DBPool{MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetime: time.Minute},
	}
	return &config.Config{
		App: config.App{Name: "it", Env: config.EnvLocal},
		Log: config.Log{Level: slog.LevelInfo, Format: "text"},
		Server: config.Server{
			ShutdownTimeout: 10 * time.Second, ReadinessTimeout: 2 * time.Second,
			Public: srv(true), Internal: srv(true),
		},
		Database: config.Database{
			StartupTimeout: 10 * time.Second, QueryTimeout: 5 * time.Second, TxMaxRetries: 3,
			Primary: conn,
		},
		Auth: config.Auth{
			Issuer: "vyking-integration", Audience: "vyking-partner-api", Leeway: 30 * time.Second,
			Keys: []config.AuthKey{{ID: "it-1", Secret: testJWTSecret}},
		},
		PartnerAPI: config.PartnerAPI{DefaultLimit: 20, MaxLimit: 100},
	}
}

// startService runs the whole application until the test ends.
func startService(t *testing.T, cfg *config.Config) {
	t.Helper()
	a, err := app.New(context.Background(), cfg, slog.New(slog.DiscardHandler), "it")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done, "service must shut down cleanly")
	})

	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + cfg.Server.Internal.Addr + "/ready")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 15*time.Second, 50*time.Millisecond, "service did not become ready")
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	return ln.Addr().String()
}
