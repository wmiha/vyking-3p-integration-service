// Package app is the composition root: it wires adapters, services and
// servers together and owns the start-up and graceful shutdown sequence.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"time"

	"github.com/wmiha/vyking-3p-integration-service/internal/adapter/handler"
	"github.com/wmiha/vyking-3p-integration-service/internal/adapter/repository"
	"github.com/wmiha/vyking-3p-integration-service/internal/auth"
	"github.com/wmiha/vyking-3p-integration-service/internal/config"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/database"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/health"
	"github.com/wmiha/vyking-3p-integration-service/internal/service/transactions"
)

const (
	maxHeaderBytes = 16 << 10
	// dbRetryInterval paces connection attempts while waiting at start-up.
	dbRetryInterval = time.Second
)

// openDB opens a connection pool; replaced in tests.
var openDB = database.Open

// App is a fully wired service instance.
type App struct {
	cfg    *config.Config
	logger *slog.Logger
	health *health.Checker

	// primary serves writes and every sync read; replica serves partner API
	// reads and is the primary itself when no replica is configured.
	primary *sql.DB
	replica *sql.DB

	public   *http.Server // nil when the public API is disabled
	internal *http.Server
	// cancelInternal ends the request contexts of the internal server so
	// long-running requests (CPU profiles, traces) cannot hold up shutdown.
	cancelInternal context.CancelFunc
}

// New wires the application. It opens connection pools lazily and does not
// open listeners; Run does both.
func New(cfg *config.Config, logger *slog.Logger) (*App, error) {
	a := &App{
		cfg:    cfg,
		logger: logger,
		health: health.NewChecker(cfg.Server.ReadinessTimeout),
	}
	if err := database.SetDriverLogger(logger); err != nil {
		return nil, fmt.Errorf("mysql driver logger: %w", err)
	}
	if err := a.openDatabases(); err != nil {
		return nil, err
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
		public, err := a.partnerAPI()
		if err != nil {
			return nil, errors.Join(err, a.closeDatabases())
		}
		a.public = newHTTPServer(cfg.Server.Public, logger, public)
	}
	return a, nil
}

// partnerAPI wires the public, partner-facing API. Its reads go to the
// replica pool (the primary when no replica is configured).
func (a *App) partnerAPI() (http.Handler, error) {
	keys := make([]auth.Key, 0, len(a.cfg.Auth.Keys))
	for _, k := range a.cfg.Auth.ActiveKeys() {
		keys = append(keys, auth.Key{ID: k.ID, Secret: []byte(k.Secret)})
	}
	verifier, err := auth.NewVerifier(auth.Config{
		Issuer:   a.cfg.Auth.Issuer,
		Audience: a.cfg.Auth.Audience,
		Leeway:   a.cfg.Auth.Leeway,
		Keys:     keys,
	})
	if err != nil {
		return nil, err
	}

	timeout := a.cfg.Database.QueryTimeout
	finder := transactions.NewService(
		repository.NewCustomers(a.replica, timeout),
		repository.NewTransactions(a.replica, timeout),
	)
	return handler.NewPublicRouter(handler.PublicDeps{
		Logger:       a.logger,
		Verifier:     verifier,
		Transactions: finder,
		Limits:       handler.PageLimits{Default: a.cfg.PartnerAPI.DefaultLimit, Max: a.cfg.PartnerAPI.MaxLimit},
	}), nil
}

func (a *App) openDatabases() error {
	primary, err := openDB(dbConfig(a.cfg.Database.Primary))
	if err != nil {
		return fmt.Errorf("open primary database: %w", err)
	}
	a.primary, a.replica = primary, primary
	a.health.Register("database_primary", health.PingFunc(primary.PingContext))

	if a.cfg.Database.Replica.Enabled {
		replica, err := openDB(dbConfig(a.cfg.Database.Replica))
		if err != nil {
			_ = primary.Close()
			return fmt.Errorf("open replica database: %w", err)
		}
		a.replica = replica
		a.health.Register("database_replica", health.PingFunc(replica.PingContext))
	}
	return nil
}

func dbConfig(c config.DBConn) database.Config {
	return database.Config{
		Host:            c.Host,
		Port:            c.Port,
		Name:            c.Name,
		User:            c.User,
		Password:        c.Password,
		Timezone:        c.Timezone,
		ConnectTimeout:  c.ConnectTimeout,
		ReadTimeout:     c.ReadTimeout,
		WriteTimeout:    c.WriteTimeout,
		MaxOpenConns:    c.Pool.MaxOpenConns,
		MaxIdleConns:    c.Pool.MaxIdleConns,
		ConnMaxLifetime: c.Pool.ConnMaxLifetime,
		ConnMaxIdleTime: c.Pool.ConnMaxIdleTime,
	}
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

// Run waits for the database, serves traffic until ctx is cancelled or a
// server fails, then shuts down gracefully. It returns nil after a clean
// shutdown.
func (a *App) Run(ctx context.Context) error {
	if err := a.waitForDatabases(ctx); err != nil {
		return errors.Join(err, a.closeDatabases())
	}

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
			return errors.Join(fmt.Errorf("listen %s server on %s: %w", name, srv.Addr, err), a.closeDatabases())
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

// waitForDatabases blocks until every pool answers, so an instance never
// reports itself started without its database. It gives up after
// database.startup_timeout and lets the orchestrator restart it.
func (a *App) waitForDatabases(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.Database.StartupTimeout)
	defer cancel()

	pools := map[string]*sql.DB{"primary": a.primary}
	if a.replica != a.primary {
		pools["replica"] = a.replica
	}
	for name, db := range pools {
		start := time.Now()
		a.logger.Info("waiting for database", "pool", name, "timeout", a.cfg.Database.StartupTimeout.String())
		if err := database.WaitReady(ctx, db, dbRetryInterval); err != nil {
			return fmt.Errorf("%s database: %w", name, err)
		}
		a.logger.Info("database ready", "pool", name, "waited_ms", time.Since(start).Milliseconds())
	}
	return nil
}

// shutdown stops the instance in dependency order:
//  1. readiness turns unavailable and we wait drain_delay so the load
//     balancer stops sending new requests;
//  2. the public server stops accepting connections and drains in-flight
//     requests;
//  3. the internal server (probes, metrics) closes so the instance stays
//     observable until the very end;
//  4. connection pools close once nothing can use them anymore.
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
	if err := a.closeDatabases(); err != nil {
		errs = append(errs, err)
	}

	err := errors.Join(errs...)
	a.logger.Info("shutdown complete", "duration_ms", time.Since(start).Milliseconds(), "clean", err == nil)
	return err
}

// closeDatabases closes the pools; sql.DB.Close waits for queries that
// already started to finish.
func (a *App) closeDatabases() error {
	var errs []error
	if a.replica != a.primary {
		if err := a.replica.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close replica database: %w", err))
		}
	}
	if err := a.primary.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close primary database: %w", err))
	}
	if len(errs) == 0 {
		a.logger.Info("database pools closed")
	}
	return errors.Join(errs...)
}
