// Package database owns MySQL connection pools and the transaction helper
// every multi-statement write must go through.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Config describes one MySQL connection pool.
type Config struct {
	Host     string
	Port     int
	Name     string
	User     string
	Password string
	// Timezone is the location DATETIME columns are interpreted in. The
	// schema stores zone-less DATETIMEs; we assume they are UTC.
	Timezone string

	ConnectTimeout time.Duration
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration

	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// Open creates a connection pool. Connections are established lazily; use
// WaitReady to verify connectivity at start-up.
func Open(cfg Config) (*sql.DB, error) {
	dsn, err := driverConfig(cfg)
	if err != nil {
		return nil, err
	}
	connector, err := mysql.NewConnector(dsn)
	if err != nil {
		return nil, fmt.Errorf("mysql connector: %w", err)
	}

	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	return db, nil
}

func driverConfig(cfg Config) (*mysql.Config, error) {
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return nil, fmt.Errorf("database timezone %q: %w", cfg.Timezone, err)
	}

	dsn := mysql.NewConfig()
	dsn.Net = "tcp"
	dsn.Addr = net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dsn.DBName = cfg.Name
	dsn.User = cfg.User
	dsn.Passwd = cfg.Password
	dsn.ParseTime = true
	dsn.Loc = loc
	dsn.Timeout = cfg.ConnectTimeout
	dsn.ReadTimeout = cfg.ReadTimeout
	dsn.WriteTimeout = cfg.WriteTimeout
	return dsn, nil
}

// WaitReady pings db until it answers or ctx ends, so a service starting
// alongside its database (compose, rolling deploys) does not fail on the
// first attempt.
func WaitReady(ctx context.Context, db *sql.DB, interval time.Duration) error {
	for {
		pingCtx, cancel := context.WithTimeout(ctx, interval)
		err := db.PingContext(pingCtx)
		cancel()
		if err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("database not reachable: %w (%w)", err, ctx.Err())
		case <-time.After(interval):
		}
	}
}

// SetDriverLogger routes the MySQL driver's own diagnostics (e.g. "closing
// bad idle connection") through slog instead of the standard library logger,
// so every line respects the configured log format.
func SetDriverLogger(logger *slog.Logger) error {
	return mysql.SetLogger(slog.NewLogLogger(logger.Handler(), slog.LevelWarn))
}
