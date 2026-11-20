package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// Beginner starts transactions; implemented by *sql.DB and *sql.Conn.
type Beginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// retryBaseDelay is the first backoff step between retried transactions.
var retryBaseDelay = 25 * time.Millisecond

type txSettings struct {
	retries int
	opts    *sql.TxOptions
}

// TxOption customizes WithTx.
type TxOption func(*txSettings)

// Retries re-runs the whole transaction up to n extra times when MySQL aborts
// it because of a deadlock or lock wait timeout.
func Retries(n int) TxOption {
	return func(s *txSettings) { s.retries = n }
}

// Isolation sets the transaction isolation level.
func Isolation(level sql.IsolationLevel) TxOption {
	return func(s *txSettings) {
		if s.opts == nil {
			s.opts = &sql.TxOptions{}
		}
		s.opts.Isolation = level
	}
}

// WithTx runs fn inside a database transaction so that a unit of work either
// fully succeeds or leaves no trace:
//
//   - fn returns nil: the transaction is committed;
//   - fn returns an error: the transaction is rolled back and the error returned;
//   - fn panics: the transaction is rolled back and the panic propagates.
//
// With Retries, transient lock conflicts re-run the whole transaction, so fn
// must only touch the database through tx and be safe to execute again.
func WithTx(ctx context.Context, db Beginner, fn func(tx *sql.Tx) error, opts ...TxOption) error {
	var s txSettings
	for _, o := range opts {
		o(&s)
	}

	for attempt := 0; ; attempt++ {
		err := runTx(ctx, db, s.opts, fn)
		if err == nil || !IsRetryable(err) || attempt >= s.retries {
			return err
		}

		// Exponential backoff with full jitter spreads competing writers apart.
		backoff := rand.N(retryBaseDelay << attempt)
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (%w)", err, ctx.Err())
		case <-time.After(time.Duration(backoff)):
		}
	}
}

func runTx(ctx context.Context, db Beginner, opts *sql.TxOptions, fn func(*sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return fmt.Errorf("%w (rollback: %w)", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
