package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/config"
)

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
	}
}

func start(t *testing.T, cfg *config.Config) (cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	a, err := New(cfg, slog.New(slog.DiscardHandler))
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

	a, err := New(cfg, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	err = a.Run(context.Background())
	require.ErrorContains(t, err, "listen public server")

	// The internal listener bound before the failure must have been released.
	ln2, err := net.Listen("tcp", cfg.Server.Internal.Addr)
	require.NoError(t, err)
	ln2.Close()
}
