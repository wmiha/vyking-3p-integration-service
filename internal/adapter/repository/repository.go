// Package repository is the MySQL adapter. Queries target the fixed schema
// given by the task; lookup tables are LEFT JOINed because the schema has no
// foreign keys and a missing reference row must never hide a transaction.
package repository

import (
	"fmt"
	"strings"

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

// placeholders returns "?, ?, ?" for n bound values.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
