package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net"

	"github.com/go-sql-driver/mysql"
)

// MySQL server error numbers the service reacts to.
const (
	errTooManyConnections = 1040
	errServerShutdown     = 1053
	errLockWaitTimeout    = 1205
	errDeadlock           = 1213
)

// IsRetryable reports whether a transaction failed because of a transient
// lock conflict, in which case re-running it is safe and likely to succeed.
func IsRetryable(err error) bool {
	me, ok := errors.AsType[*mysql.MySQLError](err)
	return ok && (me.Number == errDeadlock || me.Number == errLockWaitTimeout)
}

// IsUnavailable reports whether err was caused by the database being
// unreachable, overloaded or too slow rather than by the query itself. Such
// failures are surfaced to clients as 503 so they know a retry makes sense.
func IsUnavailable(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, driver.ErrBadConn),
		errors.Is(err, mysql.ErrInvalidConn),
		errors.Is(err, sql.ErrConnDone):
		return true
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return true
	}
	if me, ok := errors.AsType[*mysql.MySQLError](err); ok {
		switch me.Number {
		case errTooManyConnections, errServerShutdown, errLockWaitTimeout, errDeadlock:
			return true
		}
	}
	return false
}
