// Package app is the composition root: it wires adapters, services and
// servers together and owns the start-up and graceful shutdown sequence.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"time"

	"github.com/wmiha/vyking-3p-integration-service/internal/adapter/handler"
	"github.com/wmiha/vyking-3p-integration-service/internal/config"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/health"
)

const maxHeaderBytes = 16 << 10

// App is a fully wired service instance.
type App struct {
	cfg    *config.Config
	logger *slog.Logger
	health *health.Checker

	public   *http.Server // nil when the public API is disabled
	internal *http.Server
	// cancelInternal ends the request contexts of the internal server so
	// long-running requests (CPU profiles, traces) cannot hold up shutdown.
	cancelInternal context.CancelFunc
}

// New wires the application. It does not open listeners; Run does.
func New(cfg *config.Config, logger *slog.Logger) (*App, error) {
	a := &App{
		cfg:    cfg,
		logger: logger,
		health: health.NewChecker(cfg.Server.ReadinessTimeout),
	}

	a.internal = newHTTPServer(cfg.Server.Internal, logger, handler.NewInternalRouter(handler.InternalDeps{
		Logger:    logger,
		Readiness: a.health,
		Profiler:  profiler(cfg.Pprof),
	}))
	internalCtx, cancelInternal := context.WithCancel(context.Background())
	a.internal.BaseContext = func(net.Listener) context.Context { return internalCtx }
	a.cancelInternal = cancelInternal

	if cfg.Server.Public.Enabled {
		a.public = newHTTPServer(cfg.Server.Public, logger, handler.NewPublicRouter(handler.PublicDeps{
			Logger: logger,
		}))
	}
	return a, nil
}

// profiler enables the optional block/mutex profiles and returns the
// credentials guarding /debug/pprof, or nil when profiling is disabled.
func profiler(c config.Pprof) *handler.Credentials {
	if !c.Enabled {
		return nil
	}
	runtime.SetBlockProfileRate(c.BlockProfileRate)
	runtime.SetMutexProfileFraction(c.MutexProfileFraction)
	return &handler.Credentials{Username: c.Username, Password: c.Password}
}

func newHTTPServer(c config.HTTPServer, logger *slog.Logger, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              c.Addr,
		Handler:           h,
		ReadHeaderTimeout: c.ReadHeaderTimeout,
		ReadTimeout:       c.ReadTimeout,
		WriteTimeout:      c.WriteTimeout,
		IdleTimeout:       c.IdleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// Run serves traffic until ctx is cancelled or a server fails, then shuts
// down gracefully. It returns nil after a clean shutdown.
func (a *App) Run(ctx context.Context) error {
	servers := map[string]*http.Server{"internal": a.internal}
	if a.public != nil {
		servers["public"] = a.public
	}

	// Bind every listener before serving so a port conflict fails start-up
	// immediately instead of leaving a half-started instance behind.
	listeners := make(map[string]net.Listener, len(servers))
	for name, srv := range servers {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			return fmt.Errorf("listen %s server on %s: %w", name, srv.Addr, err)
		}
		listeners[name] = ln
	}

	errCh := make(chan error, len(servers))
	for name, srv := range servers {
		go func() {
			if err := srv.Serve(listeners[name]); !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s server: %w", name, err)
			}
		}()
		a.logger.Info("http server listening", "server", name, "addr", listeners[name].Addr().String())
	}

	var runErr error
	select {
	case <-ctx.Done():
		a.logger.Info("graceful shutdown started")
	case runErr = <-errCh:
		a.logger.Error("server failed, shutting down", "error", runErr)
	}
	return errors.Join(runErr, a.shutdown())
}

// shutdown stops the instance in dependency order:
//  1. readiness turns unavailable and we wait drain_delay so the load
//     balancer stops sending new requests;
//  2. the public server stops accepting connections and drains in-flight
//     requests;
//  3. the internal server (probes, metrics) closes last so the instance stays
//     observable until the very end.
//
// Everything shares one deadline: server.shutdown_timeout.
func (a *App) shutdown() error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.Server.ShutdownTimeout)
	defer cancel()

	a.health.SetShuttingDown()
	if d := a.cfg.Server.DrainDelay; d > 0 {
		a.logger.Info("draining: readiness reports unavailable", "delay", d.String())
		select {
		case <-time.After(d):
		case <-ctx.Done():
		}
	}

	var errs []error
	if a.public != nil {
		if err := a.public.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("public server shutdown: %w", err))
		} else {
			a.logger.Info("public server stopped")
		}
	}
	// Requests on the internal server are operational (probes, metrics,
	// profiles): cut them short instead of waiting for them.
	a.cancelInternal()
	if err := a.internal.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("internal server shutdown: %w", err))
	} else {
		a.logger.Info("internal server stopped")
	}

	err := errors.Join(errs...)
	a.logger.Info("shutdown complete", "duration_ms", time.Since(start).Milliseconds(), "clean", err == nil)
	return err
}
