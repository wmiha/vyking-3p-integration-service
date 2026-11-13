package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wmiha/vyking-3p-integration-service/internal/service/contactsync"
)

// ErrLockLost reports that a held named lock is gone.
var ErrLockLost = errors.New("named lock is no longer held")

// Locker elects a leader with MySQL named locks (GET_LOCK). A named lock
// belongs to the database session that took it, so a lease pins one pool
// connection for its whole lifetime; the lock disappears when that session
// ends, including when the process dies.
type Locker struct {
	db *sql.DB
	// sessionTimeout is applied as the session's wait_timeout: if the
	// leader vanishes without closing its connection (network partition),
	// MySQL drops the session and frees the lock after this long.
	sessionTimeout time.Duration
	// checkEvery throttles Lease.Held round trips.
	checkEvery time.Duration
}

func NewLocker(primary *sql.DB, sessionTimeout, checkEvery time.Duration) *Locker {
	return &Locker{db: primary, sessionTimeout: sessionTimeout, checkEvery: checkEvery}
}

// TryLock takes the named lock without waiting.
func (l *Locker) TryLock(ctx context.Context, name string) (contactsync.Lease, bool, error) {
	conn, err := l.db.Conn(ctx)
	if err != nil {
		return nil, false, wrap("acquire lock connection", err)
	}

	stmt := fmt.Sprintf("SET SESSION wait_timeout = %d", int(l.sessionTimeout.Seconds()))
	if _, err := conn.ExecContext(ctx, stmt); err != nil {
		discard(conn)
		return nil, false, wrap("configure lock session", err)
	}
	var got sql.Null[int64]
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", name).Scan(&got); err != nil {
		discard(conn)
		return nil, false, wrap("get lock", err)
	}
	if !got.Valid || got.V != 1 {
		discard(conn)
		return nil, false, nil
	}
	return &lease{conn: conn, name: name, checkEvery: l.checkEvery, checked: time.Now()}, true, nil
}

type lease struct {
	conn       *sql.Conn
	name       string
	checkEvery time.Duration

	mu      sync.Mutex
	checked time.Time
}

// Held verifies, at most every checkEvery, that this session still owns the
// lock. A dropped connection or a server restart is detected here.
func (l *lease) Held(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if time.Since(l.checked) < l.checkEvery {
		return nil
	}
	var owner sql.Null[int64]
	if err := l.conn.QueryRowContext(ctx, "SELECT IS_USED_LOCK(?) = CONNECTION_ID()", l.name).Scan(&owner); err != nil {
		return fmt.Errorf("%w: %w", ErrLockLost, err)
	}
	if !owner.Valid || owner.V != 1 {
		return ErrLockLost
	}
	l.checked = time.Now()
	return nil
}

// Release frees the lock and discards the connection, so neither the lock
// nor the session settings can leak into the pool.
func (l *lease) Release(ctx context.Context) error {
	_, err := l.conn.ExecContext(ctx, "DO RELEASE_LOCK(?)", l.name)
	discard(l.conn)
	if err != nil {
		return wrap("release lock", err)
	}
	return nil
}

// discard closes conn's underlying connection instead of returning it to
// the pool.
func discard(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}
