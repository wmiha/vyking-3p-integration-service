package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recorder is a minimal database/sql driver that records transaction
// lifecycle calls, so WithTx can be tested without a database.
type recorder struct {
	mu        sync.Mutex
	events    []string
	beginErr  error
	commitErr error
}

func (r *recorder) add(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) Events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *recorder) Connect(context.Context) (driver.Conn, error) { return fakeConn{r}, nil }
func (r *recorder) Driver() driver.Driver                        { return nil }

type fakeConn struct{ rec *recorder }

func (c fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (c fakeConn) Close() error                        { return nil }
func (c fakeConn) Begin() (driver.Tx, error) {
	c.rec.add("begin")
	if c.rec.beginErr != nil {
		return nil, c.rec.beginErr
	}
	return fakeTx(c), nil
}

type fakeTx struct{ rec *recorder }

func (t fakeTx) Commit() error   { t.rec.add("commit"); return t.rec.commitErr }
func (t fakeTx) Rollback() error { t.rec.add("rollback"); return nil }

var (
	errDeadlockMySQL = &mysql.MySQLError{Number: errDeadlock, Message: "Deadlock found"}
	errBusiness      = errors.New("customer not found")
)

func TestWithTx(t *testing.T) {
	retryBaseDelay = time.Millisecond

	tests := []struct {
		name       string
		opts       []TxOption
		fnErrs     []error // error returned by fn per attempt (last one repeats)
		beginErr   error
		commitErr  error
		wantEvents []string
		wantErr    error
		wantErrMsg string
		wantCalls  int
	}{
		{
			name:       "commit on success",
			fnErrs:     []error{nil},
			wantEvents: []string{"begin", "commit"},
			wantCalls:  1,
		},
		{
			name:       "rollback on error",
			fnErrs:     []error{errBusiness},
			wantEvents: []string{"begin", "rollback"},
			wantErr:    errBusiness,
			wantCalls:  1,
		},
		{
			name:       "deadlock retried then committed",
			opts:       []TxOption{Retries(3)},
			fnErrs:     []error{errDeadlockMySQL, nil},
			wantEvents: []string{"begin", "rollback", "begin", "commit"},
			wantCalls:  2,
		},
		{
			name:       "retries exhausted",
			opts:       []TxOption{Retries(2)},
			fnErrs:     []error{errDeadlockMySQL},
			wantEvents: []string{"begin", "rollback", "begin", "rollback", "begin", "rollback"},
			wantErr:    errDeadlockMySQL,
			wantCalls:  3,
		},
		{
			name:       "deadlock without retries",
			fnErrs:     []error{errDeadlockMySQL},
			wantEvents: []string{"begin", "rollback"},
			wantErr:    errDeadlockMySQL,
			wantCalls:  1,
		},
		{
			name:       "business errors are never retried",
			opts:       []TxOption{Retries(3)},
			fnErrs:     []error{errBusiness},
			wantEvents: []string{"begin", "rollback"},
			wantErr:    errBusiness,
			wantCalls:  1,
		},
		{
			name:       "begin failure",
			beginErr:   errors.New("too many connections"),
			wantEvents: []string{"begin"},
			wantErrMsg: "begin transaction: too many connections",
			wantCalls:  0,
		},
		{
			name:       "commit failure",
			fnErrs:     []error{nil},
			commitErr:  errors.New("connection lost"),
			wantEvents: []string{"begin", "commit"},
			wantErrMsg: "commit transaction: connection lost",
			wantCalls:  1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{beginErr: tt.beginErr, commitErr: tt.commitErr}
			db := sql.OpenDB(rec)
			defer db.Close()

			calls := 0
			err := WithTx(context.Background(), db, func(tx *sql.Tx) error {
				assert.NotNil(t, tx)
				e := tt.fnErrs[min(calls, len(tt.fnErrs)-1)]
				calls++
				return e
			}, tt.opts...)

			switch {
			case tt.wantErr != nil:
				assert.ErrorIs(t, err, tt.wantErr)
			case tt.wantErrMsg != "":
				assert.EqualError(t, err, tt.wantErrMsg)
			default:
				assert.NoError(t, err)
			}
			assert.Equal(t, tt.wantEvents, rec.Events())
			assert.Equal(t, tt.wantCalls, calls)
		})
	}
}

func TestWithTxRollsBackOnPanic(t *testing.T) {
	rec := &recorder{}
	db := sql.OpenDB(rec)
	defer db.Close()

	assert.PanicsWithValue(t, "boom", func() {
		_ = WithTx(context.Background(), db, func(*sql.Tx) error { panic("boom") })
	})
	assert.Equal(t, []string{"begin", "rollback"}, rec.Events())
}

func TestWithTxStopsRetryingWhenContextEnds(t *testing.T) {
	retryBaseDelay = time.Hour // the context must win over the backoff
	defer func() { retryBaseDelay = time.Millisecond }()

	rec := &recorder{}
	db := sql.OpenDB(rec)
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := WithTx(ctx, db, func(*sql.Tx) error { return errDeadlockMySQL }, Retries(5))
	require.Error(t, err)
	assert.ErrorIs(t, err, errDeadlockMySQL)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestErrorClassification(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		retryable   bool
		unavailable bool
	}{
		{name: "nil", err: nil},
		{name: "deadlock", err: errDeadlockMySQL, retryable: true, unavailable: true},
		{name: "lock wait timeout", err: &mysql.MySQLError{Number: errLockWaitTimeout}, retryable: true, unavailable: true},
		{name: "too many connections", err: &mysql.MySQLError{Number: errTooManyConnections}, unavailable: true},
		{name: "syntax error", err: &mysql.MySQLError{Number: 1064}},
		{name: "invalid connection", err: mysql.ErrInvalidConn, unavailable: true},
		{name: "bad connection", err: driver.ErrBadConn, unavailable: true},
		{name: "query timeout", err: context.DeadlineExceeded, unavailable: true},
		{name: "client went away", err: context.Canceled},
		{name: "wrapped", err: errors.Join(errors.New("list transactions"), errDeadlockMySQL), retryable: true, unavailable: true},
		{name: "business", err: errBusiness},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.retryable, IsRetryable(tt.err))
			assert.Equal(t, tt.unavailable, IsUnavailable(tt.err))
		})
	}
}
