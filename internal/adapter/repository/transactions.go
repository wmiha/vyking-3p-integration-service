package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
	"github.com/wmiha/vyking-3p-integration-service/internal/service/transactions"
)

// Both statements select one page of a customer's transactions, newest first,
// using keyset pagination on (TRX_dCreated, TRX_iID).
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
// The optional status filter is applied while walking the index range; NULL
// disables it. Its id list is materialized once per execution (an indexed
// temporary table), so checking a row is a lookup, not a JSON parse. NOTES.md
// discusses an index for customers whose history is mostly filtered out.
const (
	// sqlSelectTransactionsFirstPage starts at the newest transaction.
	//
	// Parameters: customer id, status ids as a JSON array or NULL (twice), limit.
	sqlSelectTransactionsFirstPage = `
		SELECT t.TRX_iID, COALESCE(ty.TYP_cName, ''), t.TRX_STA_iID, t.TRX_iAmount, pm.PMT_cName,
		       t.TRX_dCreated, t.TRX_dBooked, t.TRX_dChanged
		FROM (
			SELECT TRX_iID, TRX_TYP_iID, TRX_STA_iID, TRX_iAmount, TRX_PMT_iID,
			       TRX_dCreated, TRX_dBooked, TRX_dChanged
			FROM transactions
			WHERE TRX_CUS_iID = ?
			  AND (? IS NULL OR TRX_STA_iID IN (
			      SELECT wanted.id
			      FROM JSON_TABLE(CAST(? AS JSON), '$[*]' COLUMNS (id INT UNSIGNED PATH '$')) AS wanted))
			ORDER BY TRX_dCreated DESC, TRX_iID DESC
			LIMIT ?
		) t
		LEFT JOIN transaction_types ty ON ty.TYP_iID = t.TRX_TYP_iID
		LEFT JOIN payment_methods pm ON pm.PMT_iID = t.TRX_PMT_iID
		ORDER BY t.TRX_dCreated DESC, t.TRX_iID DESC`

	// sqlSelectTransactionsAfterCursor continues after the last transaction
	// of the previous page (exclusive).
	//
	// Parameters: customer id, status ids as a JSON array or NULL (twice),
	// cursor created (twice), cursor id, limit.
	sqlSelectTransactionsAfterCursor = `
		SELECT t.TRX_iID, COALESCE(ty.TYP_cName, ''), t.TRX_STA_iID, t.TRX_iAmount, pm.PMT_cName,
		       t.TRX_dCreated, t.TRX_dBooked, t.TRX_dChanged
		FROM (
			SELECT TRX_iID, TRX_TYP_iID, TRX_STA_iID, TRX_iAmount, TRX_PMT_iID,
			       TRX_dCreated, TRX_dBooked, TRX_dChanged
			FROM transactions
			WHERE TRX_CUS_iID = ?
			  AND (? IS NULL OR TRX_STA_iID IN (
			      SELECT wanted.id
			      FROM JSON_TABLE(CAST(? AS JSON), '$[*]' COLUMNS (id INT UNSIGNED PATH '$')) AS wanted))
			  AND (TRX_dCreated < ? OR (TRX_dCreated = ? AND TRX_iID < ?))
			ORDER BY TRX_dCreated DESC, TRX_iID DESC
			LIMIT ?
		) t
		LEFT JOIN transaction_types ty ON ty.TYP_iID = t.TRX_TYP_iID
		LEFT JOIN payment_methods pm ON pm.PMT_iID = t.TRX_PMT_iID
		ORDER BY t.TRX_dCreated DESC, t.TRX_iID DESC`
)

// Transactions reads customer transactions for the partner API.
type Transactions struct {
	timeout time.Duration

	selectFirstPage   *sql.Stmt
	selectAfterCursor *sql.Stmt
}

// NewTransactions prepares the repository's statements on db; Close releases
// them.
func NewTransactions(ctx context.Context, db *sql.DB, timeout time.Duration) (*Transactions, error) {
	r := &Transactions{timeout: timeout}
	if err := prepareAll(ctx, db,
		statement{&r.selectFirstPage, "select transactions first page", sqlSelectTransactionsFirstPage},
		statement{&r.selectAfterCursor, "select transactions after cursor", sqlSelectTransactionsAfterCursor},
	); err != nil {
		return nil, err
	}
	return r, nil
}

// Close releases the prepared statements.
func (r *Transactions) Close() error {
	return closeAll(r.selectFirstPage, r.selectAfterCursor)
}

// ListPage returns a page of a customer's transactions, newest first.
func (r *Transactions) ListPage(ctx context.Context, q transactions.PageQuery) ([]domain.Transaction, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	var statuses any // NULL: no status filter
	if len(q.StatusIDs) > 0 {
		statuses = jsonIDs(q.StatusIDs)
	}

	var (
		rows *sql.Rows
		err  error
	)
	if q.After == nil {
		rows, err = r.selectFirstPage.QueryContext(ctx, uint32(q.Customer), statuses, statuses, q.Limit)
	} else {
		created := time.Unix(q.After.CreatedUnix, 0).UTC()
		rows, err = r.selectAfterCursor.QueryContext(ctx, uint32(q.Customer), statuses, statuses,
			created, created, q.After.ID, q.Limit)
	}
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
