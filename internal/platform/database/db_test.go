package database

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDriverConfig(t *testing.T) {
	cfg := Config{
		Host: "mysql", Port: 3306, Name: "vyking", User: "app", Password: "p@ss:word/#",
		Timezone:       "UTC",
		ConnectTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 11 * time.Second,
	}

	dsn, err := driverConfig(cfg)
	require.NoError(t, err)
	assert.Equal(t, "tcp", dsn.Net)
	assert.Equal(t, "mysql:3306", dsn.Addr)
	assert.Equal(t, "vyking", dsn.DBName)
	assert.Equal(t, "app", dsn.User)
	assert.Equal(t, "p@ss:word/#", dsn.Passwd, "passwords with DSN syntax must survive unescaped")
	assert.True(t, dsn.ParseTime)
	assert.Equal(t, time.UTC, dsn.Loc)
	assert.Equal(t, 5*time.Second, dsn.Timeout)
	assert.Equal(t, 10*time.Second, dsn.ReadTimeout)
	assert.Equal(t, 11*time.Second, dsn.WriteTimeout)

	cfg.Host = "::1"
	dsn, err = driverConfig(cfg)
	require.NoError(t, err)
	assert.Equal(t, "[::1]:3306", dsn.Addr)

	cfg.Timezone = "Mars/Olympus"
	_, err = driverConfig(cfg)
	assert.ErrorContains(t, err, `database timezone "Mars/Olympus"`)
}

func TestOpenAppliesPoolSettings(t *testing.T) {
	db, err := Open(Config{
		Host: "127.0.0.1", Port: 1, Name: "x", User: "x", Timezone: "UTC",
		MaxOpenConns: 7, MaxIdleConns: 3, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Second,
	})
	require.NoError(t, err)
	defer db.Close()
	assert.Equal(t, 7, db.Stats().MaxOpenConnections)
}

func TestWaitReadyGivesUp(t *testing.T) {
	// Port 1 on localhost refuses connections immediately.
	db, err := Open(Config{Host: "127.0.0.1", Port: 1, Name: "x", User: "x", Timezone: "UTC",
		ConnectTimeout: 200 * time.Millisecond, MaxOpenConns: 1})
	require.NoError(t, err)
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err = WaitReady(ctx, db, 50*time.Millisecond)
	require.Error(t, err)
	assert.ErrorContains(t, err, "database not reachable")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}
