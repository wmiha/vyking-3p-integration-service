package repository

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
	"github.com/wmiha/vyking-3p-integration-service/internal/service/transactions"
)

// Transactions reads customer transactions for the partner API.
type Transactions struct {
	db      *sql.DB
	timeout time.Duration
}

func NewTransactions(db *sql.DB, timeout time.Duration) *Transactions {
	return &Transactions{db: db, timeout: timeout}
}

// ListPage returns a page of a customer's transactions, newest first, using
// keyset pagination on (TRX_dCreated, TRX_iID).
//
// The page is selected in a derived table on transactions alone, so MySQL
// walks the existing (TRX_CUS_iID, TRX_dCreated) index backwards (InnoDB
// appends the primary key) and stops after `limit` rows: the cost is
// proportional to the page size, not to the customer's history. Lookup names
// are joined afterwards onto those few rows. Joining them directly lets the
// optimizer hash-join the tiny lookup tables, which loses the index order
// and sorts the customer's whole history on every page (measured: ~0.9s per
// page at 200k transactions).
//
// A status filter is applied while walking the index range; NOTES.md
// discusses an index for customers whose history is mostly filtered out.
func (r *Transactions) ListPage(ctx context.Context, q transactions.PageQuery) ([]domain.Transaction, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	query, args := listPageQuery(q)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("list transactions", err)
	}
	defer rows.Close()

	out := make([]domain.Transaction, 0, q.Limit)
	for rows.Next() {
		var (
			t      domain.Transaction
			amount sql.Null[int64]
			method sql.Null[string]
			booked sql.Null[time.Time]
		)
		if err := rows.Scan(&t.ID, &t.Type, &t.StatusID, &amount, &method, &t.CreatedAt, &booked, &t.ChangedAt); err != nil {
			return nil, wrap("scan transaction", err)
		}
		if amount.Valid {
			t.AmountMinor = &amount.V
		}
		if method.Valid {
			t.PaymentMethod = &method.V
		}
		if booked.Valid {
			t.BookedAt = &booked.V
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("iterate transactions", err)
	}
	return out, nil
}

func listPageQuery(q transactions.PageQuery) (string, []any) {
	var where strings.Builder
	where.WriteString(`TRX_CUS_iID = ?`)
	args := []any{uint32(q.Customer)}

	if len(q.StatusIDs) > 0 {
		where.WriteString(` AND TRX_STA_iID IN (` + placeholders(len(q.StatusIDs)) + `)`)
		for _, id := range q.StatusIDs {
			args = append(args, id)
		}
	}
	if q.After != nil {
		created := time.Unix(q.After.CreatedUnix, 0).UTC()
		where.WriteString(` AND (TRX_dCreated < ? OR (TRX_dCreated = ? AND TRX_iID < ?))`)
		args = append(args, created, created, q.After.ID)
	}
	args = append(args, q.Limit)

	return `
		SELECT t.TRX_iID, COALESCE(ty.TYP_cName, ''), t.TRX_STA_iID, t.TRX_iAmount, pm.PMT_cName,
		       t.TRX_dCreated, t.TRX_dBooked, t.TRX_dChanged
		FROM (
			SELECT TRX_iID, TRX_TYP_iID, TRX_STA_iID, TRX_iAmount, TRX_PMT_iID,
			       TRX_dCreated, TRX_dBooked, TRX_dChanged
			FROM transactions
			WHERE ` + where.String() + `
			ORDER BY TRX_dCreated DESC, TRX_iID DESC
			LIMIT ?
		) t
		LEFT JOIN transaction_types ty ON ty.TYP_iID = t.TRX_TYP_iID
		LEFT JOIN payment_methods pm ON pm.PMT_iID = t.TRX_PMT_iID
		ORDER BY t.TRX_dCreated DESC, t.TRX_iID DESC`, args
}
