package repository

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
)

const (
	// sqlSelectCustomersByEmail finds the customers with an email in a set of
	// brands. Emails are unique per brand, so at most one row per brand.
	//
	// Parameters: email, brand ids as a JSON array.
	sqlSelectCustomersByEmail = `
		SELECT c.CUS_iID, c.CUS_BRD_iID, COALESCE(b.BRD_cName, ''), COALESCE(cur.CUR_cAlpha3, ''),
		       cur.CUR_iMinorUnits, c.CUS_bActive, c.CUS_cEmail
		FROM customers c
		LEFT JOIN brands b ON b.BRD_iID = c.CUS_BRD_iID
		LEFT JOIN currencies cur ON cur.CUR_iID = c.CUS_CUR_iID
		WHERE c.CUS_cEmail = ?
		  AND c.CUS_BRD_iID IN (
		      SELECT scope.id
		      FROM JSON_TABLE(CAST(? AS JSON), '$[*]' COLUMNS (id INT UNSIGNED PATH '$')) AS scope)
		ORDER BY c.CUS_BRD_iID`
)

// Customers reads customers for the partner API.
type Customers struct {
	timeout time.Duration

	selectByEmail *sql.Stmt
}

// NewCustomers prepares the repository's statements on db; Close releases
// them.
func NewCustomers(ctx context.Context, db *sql.DB, timeout time.Duration) (*Customers, error) {
	r := &Customers{timeout: timeout}
	if err := prepareAll(ctx, db,
		statement{&r.selectByEmail, "select customers by email", sqlSelectCustomersByEmail},
	); err != nil {
		return nil, err
	}
	return r, nil
}

// Close releases the prepared statements.
func (r *Customers) Close() error {
	return closeAll(r.selectByEmail)
}

// FindByEmail returns the customers with email in any of brands, matching
// case-insensitively but otherwise exactly.
//
// The column collation (utf8mb4_0900_ai_ci) also ignores accents, so
// "jané@example.com" would match "jane@example.com" and disclose another
// customer. The indexed comparison narrows the candidates; the exact check
// happens in Go. Comparing with an explicit accent-sensitive collation in SQL
// would prevent the index from being used.
func (r *Customers) FindByEmail(ctx context.Context, email string, brands []domain.BrandID) ([]domain.Customer, error) {
	if len(brands) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	rows, err := r.selectByEmail.QueryContext(ctx, email, jsonIDs(brands))
	if err != nil {
		return nil, wrap("find customers by email", err)
	}
	defer rows.Close()

	var out []domain.Customer
	for rows.Next() {
		var (
			c      domain.Customer
			minor  sql.Null[uint8]
			stored string
		)
		if err := rows.Scan(&c.ID, &c.Brand, &c.BrandName, &c.Currency, &minor, &c.Active, &stored); err != nil {
			return nil, wrap("scan customer", err)
		}
		if !strings.EqualFold(stored, email) {
			continue // equal only under accent-insensitive collation
		}
		if minor.Valid {
			c.MinorUnits = &minor.V
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("iterate customers", err)
	}
	return out, nil
}
