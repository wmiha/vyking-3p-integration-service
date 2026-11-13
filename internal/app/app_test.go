package app

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/config"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/database"
)

// fakeDB stands in for MySQL: connections open instantly and pings succeed
// until down is set, which simulates an outage. Statements can be prepared
// (but not executed) unless prepareFails is set.
type fakeDB struct {
	down         atomic.Bool
	prepareFails atomic.Bool
}

func (f *fakeDB) Connect(context.Context) (driver.Conn, error) {
	if f.down.Load() {
		return nil, errors.New("connection refused")
	}
	return fakeConn{f}, nil
}
func (f *fakeDB) Driver() driver.Driver { return nil }

type fakeConn struct{ db *fakeDB }

func (c fakeConn) Prepare(string) (driver.Stmt, error) {
	if c.db.prepareFails.Load() {
		return nil, errors.New("SELECT command denied to user")
	}
	return fakeStmt{}, nil
}
func (c fakeConn) Close() error              { return nil }
func (c fakeConn) Begin() (driver.Tx, error) { return nil, errors.New("not supported") }
func (c fakeConn) Ping(context.Context) error {
	if c.db.down.Load() {
		return errors.New("connection refused")
	}
	return nil
}

type fakeStmt struct{}

func (fakeStmt) Close() error  { return nil }
func (fakeStmt) NumInput() int { return -1 }
func (fakeStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("not supported")
}
func (fakeStmt) Query([]driver.Value) (driver.Rows, error) {
	return nil, errors.New("not supported")
}

// useFakeDB routes every pool the app opens to db.
func useFakeDB(t *testing.T, db *fakeDB) {
	t.Helper()
	orig := openDB
	openDB = func(database.Config) (*sql.DB, error) { return sql.OpenDB(db), nil }
	t.Cleanup(func() { openDB = orig })
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func testConfig(t *testing.T, publicEnabled bool, drain time.Duration) *config.Config {
	t.Helper()
	srv := func(addr string, enabled bool) config.HTTPServer {
		return config.HTTPServer{
			Enabled: enabled, Addr: addr,
			ReadHeaderTimeout: time.Second, ReadTimeout: time.Second,
			WriteTimeout: time.Second, IdleTimeout: time.Second,
		}
	}
	return &config.Config{
		App: config.App{Name: "test", Env: config.EnvLocal},
		Log: config.Log{Level: slog.LevelInfo, Format: "text"},
		Server: config.Server{
			ShutdownTimeout:  5 * time.Second,
			DrainDelay:       drain,
			ReadinessTimeout: time.Second,
			Public:           srv(freeAddr(t), publicEnabled),
			Internal:         srv(freeAddr(t), true),
		},
		Auth:       config.Auth{Issuer: "i", Audience: "a", Keys: []config.AuthKey{{ID: "k", Secret: "test-secret-0123456789-0123456789"}}},
		PartnerAPI: config.PartnerAPI{DefaultLimit: 20, MaxLimit: 100},
		Database: config.Database{
			StartupTimeout: 2 * time.Second,
			QueryTimeout:   time.Second,
			Primary:        config.DBConn{Host: "primary", Port: 3306, Timezone: "UTC", Pool: config.DBPool{MaxOpenConns: 2}},
			Replica:        config.DBConn{Host: "replica", Port: 3306, Timezone: "UTC", Pool: config.DBPool{MaxOpenConns: 2}},
		},
	}
}

func start(t *testing.T, cfg *config.Config) (cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	return startWith(t, cfg, &fakeDB{})
}

func startWith(t *testing.T, cfg *config.Config, db *fakeDB) (cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	useFakeDB(t, db)
	a, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler), "test")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- a.Run(ctx) }()

	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + cfg.Server.Internal.Addr + "/health")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond)
	return cancel, errCh
}

func get(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func TestRunServesAndShutsDownCleanly(t *testing.T) {
	cfg := testConfig(t, true, 0)
	cancel, done := start(t, cfg)

	status, body := get(t, "http://"+cfg.Server.Internal.Addr+"/ready")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "ok", body["status"])

	status, _ = get(t, "http://"+cfg.Server.Public.Addr+"/v1/unknown")
	assert.Equal(t, http.StatusNotFound, status)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	_, err := http.Get("http://" + cfg.Server.Internal.Addr + "/health")
	assert.Error(t, err, "listeners must be closed after shutdown")
}

func TestReadinessFailsDuringDrain(t *testing.T) {
	cfg := testConfig(t, true, 500*time.Millisecond)
	cancel, done := start(t, cfg)

	cancel()
	// During the drain delay the servers keep running but readiness fails so
	// the load balancer stops routing new traffic.
	require.Eventually(t, func() bool {
		status, body := get(t, "http://"+cfg.Server.Internal.Addr+"/ready")
		return status == http.StatusServiceUnavailable && body["status"] == "shutting_down"
	}, 400*time.Millisecond, 10*time.Millisecond)

	status, _ := get(t, "http://"+cfg.Server.Public.Addr+"/v1/unknown")
	assert.Equal(t, http.StatusNotFound, status, "public server still serves while draining")

	require.NoError(t, <-done)
}

func TestPublicServerCanBeDisabled(t *testing.T) {
	cfg := testConfig(t, false, 0)
	cancel, done := start(t, cfg)
	defer func() { cancel(); require.NoError(t, <-done) }()

	_, err := http.Get("http://" + cfg.Server.Public.Addr + "/")
	assert.Error(t, err, "public listener must not be opened when disabled")
}

func TestRunFailsWhenPortIsTaken(t *testing.T) {
	cfg := testConfig(t, true, 0)
	ln, err := net.Listen("tcp", cfg.Server.Public.Addr)
	require.NoError(t, err)
	defer ln.Close()

	useFakeDB(t, &fakeDB{})
	a, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler), "test")
	require.NoError(t, err)
	err = a.Run(context.Background())
	require.ErrorContains(t, err, "listen public server")

	// The internal listener bound before the failure must have been released.
	ln2, err := net.Listen("tcp", cfg.Server.Internal.Addr)
	require.NoError(t, err)
	ln2.Close()
}

// TestProfileOutlivesWriteTimeout proves that long CPU profiles work even
// though they exceed the internal server's write timeout: pprof extends its
// own deadline through http.ResponseController, which must reach the
// connection through our middleware's response writer wrapper.
func TestProfileOutlivesWriteTimeout(t *testing.T) {
	cfg := testConfig(t, false, 0)
	cfg.Server.Internal.WriteTimeout = time.Second
	cfg.Pprof = config.Pprof{Enabled: true, Username: "ops", Password: "pprof-secret-1"}
	cancel, done := start(t, cfg)
	defer func() { cancel(); require.NoError(t, <-done) }()

	req, err := http.NewRequest(http.MethodGet, "http://"+cfg.Server.Internal.Addr+"/debug/pprof/profile?seconds=2", nil)
	require.NoError(t, err)
	req.SetBasicAuth("ops", "pprof-secret-1")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotEmpty(t, body, "expected a CPU profile")
}

// TestLongProfileDoesNotBlockShutdown ensures an in-flight 30s CPU profile is
// cut short at shutdown instead of holding the process until the shutdown
// timeout expires.
func TestLongProfileDoesNotBlockShutdown(t *testing.T) {
	cfg := testConfig(t, false, 0)
	cfg.Pprof = config.Pprof{Enabled: true, Username: "ops", Password: "pprof-secret-1"}
	cancel, done := start(t, cfg)

	type result struct {
		status int
		body   []byte
		err    error
	}
	profile := make(chan result, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, "http://"+cfg.Server.Internal.Addr+"/debug/pprof/profile?seconds=30", nil)
		req.SetBasicAuth("ops", "pprof-secret-1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			profile <- result{err: err}
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		profile <- result{status: resp.StatusCode, body: body, err: err}
	}()
	time.Sleep(500 * time.Millisecond) // let the profile start

	start := time.Now()
	cancel()
	require.NoError(t, <-done, "shutdown must stay clean")
	assert.Less(t, time.Since(start), 3*time.Second, "profile must not hold the shutdown")

	res := <-profile
	require.NoError(t, res.err)
	assert.Equal(t, http.StatusOK, res.status)
	assert.NotEmpty(t, res.body, "the partial profile is still delivered")
}

func TestReadinessReflectsDatabase(t *testing.T) {
	cfg := testConfig(t, false, 0)
	db := &fakeDB{}
	cancel, done := startWith(t, cfg, db)
	defer func() { cancel(); require.NoError(t, <-done) }()

	ready := "http://" + cfg.Server.Internal.Addr + "/ready"
	status, body := get(t, ready)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, map[string]any{"database_primary": "ok"}, body["checks"])

	db.down.Store(true)
	status, body = get(t, ready)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, "unavailable", body["status"])
	assert.Equal(t, map[string]any{"database_primary": "connection refused"}, body["checks"])

	status, _ = get(t, "http://"+cfg.Server.Internal.Addr+"/health")
	assert.Equal(t, http.StatusOK, status, "liveness must not depend on the database")

	db.down.Store(false)
	status, _ = get(t, ready)
	assert.Equal(t, http.StatusOK, status, "readiness recovers with the database")
}

func TestReplicaIsCheckedWhenEnabled(t *testing.T) {
	cfg := testConfig(t, false, 0)
	cfg.Database.Replica.Enabled = true
	cancel, done := start(t, cfg)
	defer func() { cancel(); require.NoError(t, <-done) }()

	_, body := get(t, "http://"+cfg.Server.Internal.Addr+"/ready")
	assert.Equal(t, map[string]any{"database_primary": "ok", "database_replica": "ok"}, body["checks"])
}

func TestStartupFailsWithoutDatabase(t *testing.T) {
	cfg := testConfig(t, true, 0)
	cfg.Database.StartupTimeout = 300 * time.Millisecond
	db := &fakeDB{}
	db.down.Store(true)
	useFakeDB(t, db)

	_, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler), "test")
	require.Error(t, err)
	assert.ErrorContains(t, err, "primary database: database not reachable")

	_, err = http.Get("http://" + cfg.Server.Internal.Addr + "/health")
	assert.Error(t, err, "no listener may be opened before the database is reachable")
}

// TestStartupFailsWhenStatementsCannotBePrepared shows that a broken query or
// a missing grant stops the start-up instead of failing the first request.
func TestStartupFailsWhenStatementsCannotBePrepared(t *testing.T) {
	cfg := testConfig(t, true, 0)
	db := &fakeDB{}
	db.prepareFails.Store(true)
	useFakeDB(t, db)

	_, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler), "test")
	require.Error(t, err)
	assert.ErrorContains(t, err, "customers repository: prepare select customers by email")
}

func TestCacheIsAReadinessDependency(t *testing.T) {
	cfg := testConfig(t, true, 0)
	cfg.Cache = config.Cache{Enabled: true, TTL: time.Second, MaxEntries: 10, SweepInterval: time.Second}
	cancel, done := start(t, cfg)
	defer func() { cancel(); require.NoError(t, <-done, "the cache sweeper stops at shutdown") }()

	_, body := get(t, "http://"+cfg.Server.Internal.Addr+"/ready")
	assert.Equal(t, map[string]any{"database_primary": "ok", "cache": "ok"}, body["checks"])
}
