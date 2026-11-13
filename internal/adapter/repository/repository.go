// Package repository is the MySQL adapter. Queries target the fixed schema
// given by the task; lookup tables are LEFT JOINed because the schema has no
// foreign keys and a missing reference row must never hide a transaction.
//
// Every repository keeps its SQL as named constants at the top of its file
// and prepares them once, when it is created. database/sql then re-prepares a
// statement transparently on each pooled connection that executes it, so the
// server parses a query once per connection instead of once per call, and a
// broken query or a missing grant fails the start-up instead of a request.
//
// Lists of ids are bound as one JSON array parameter and expanded with
// JSON_TABLE, which keeps the statement text fixed whatever the list length.
// MySQL materializes the expanded list once per execution; MEMBER OF would be
// shorter but parses the JSON again for every row it filters.
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/database"
)

// wrap annotates err with the operation and marks infrastructure failures
// as domain.ErrUnavailable so the API can answer 503 instead of 500.
func wrap(op string, err error) error {
	if database.IsUnavailable(err) {
		return fmt.Errorf("%s: %w: %w", op, domain.ErrUnavailable, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// statement binds a named query to the field holding its prepared statement.
type statement struct {
	dst   **sql.Stmt
	name  string
	query string
}

// prepareAll prepares every statement on db. When one fails, the statements
// prepared so far are closed again.
func prepareAll(ctx context.Context, db *sql.DB, stmts ...statement) error {
	for i, s := range stmts {
		stmt, err := db.PrepareContext(ctx, s.query)
		if err != nil {
			for _, done := range stmts[:i] {
				_ = (*done.dst).Close()
				*done.dst = nil
			}
			return wrap("prepare "+s.name, err)
		}
		*s.dst = stmt
	}
	return nil
}

// closeAll closes prepared statements; nil statements are skipped.
func closeAll(stmts ...*sql.Stmt) error {
	var errs []error
	for _, s := range stmts {
		if s == nil {
			continue
		}
		if err := s.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// jsonIDs renders ids as a JSON array ("[1,2,3]") for a JSON_TABLE
// parameter. An empty list is "[]", never "null", so an empty list always
// means "no ids" and never depends on how MySQL treats a JSON null.
func jsonIDs[T ~uint32 | ~uint64](ids []T) string {
	b := make([]byte, 0, 2+len(ids)*8)
	b = append(b, '[')
	for i, id := range ids {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendUint(b, uint64(id), 10)
	}
	return string(append(b, ']'))
}
