package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/database"
)

// Sync implements the contact sync's persistence on the primary database.
// It must never read from a replica: a lagging customer snapshot could be
// pushed to Acme Support and its queue entry marked processed, silently
// losing the newer change.
type Sync struct {
	db        *sql.DB
	timeout   time.Duration
	txRetries int
}

func NewSync(primary *sql.DB, queryTimeout time.Duration, txRetries int) *Sync {
	return &Sync{db: primary, timeout: queryTimeout, txRetries: txRetries}
}

// Outstanding returns the oldest perBrand outstanding entries of every brand
// in one statement, so a large backlog of one brand cannot starve the
// others. Entries of missing customers come back with brand 0 (LEFT JOIN)
// and are reported, never dropped.
//
// Without an index on SYQ_dProcessed this scans sync_queue; NOTES.md
// recommends (SYQ_dProcessed, SYQ_iID) plus retention of processed rows.
func (r *Sync) Outstanding(ctx context.Context, perBrand int, exclude []domain.CustomerID) ([]domain.QueueEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	excludeClause := ""
	args := make([]any, 0, len(exclude)+1)
	if len(exclude) > 0 {
		excludeClause = ` AND q.SYQ_CUS_iID NOT IN (` + placeholders(len(exclude)) + `)`
		for _, id := range exclude {
			args = append(args, uint32(id))
		}
	}
	args = append(args, perBrand)

	rows, err := r.db.QueryContext(ctx, `
		SELECT SYQ_iID, SYQ_CUS_iID, brand, SYQ_dQueued
		FROM (
			SELECT q.SYQ_iID, q.SYQ_CUS_iID, COALESCE(c.CUS_BRD_iID, 0) AS brand, q.SYQ_dQueued,
			       ROW_NUMBER() OVER (PARTITION BY COALESCE(c.CUS_BRD_iID, 0) ORDER BY q.SYQ_iID) AS rn
			FROM sync_queue q
			LEFT JOIN customers c ON c.CUS_iID = q.SYQ_CUS_iID
			WHERE q.SYQ_dProcessed IS NULL`+excludeClause+`
		) ranked
		WHERE rn <= ?
		ORDER BY brand, SYQ_iID`, args...)
	if err != nil {
		return nil, wrap("fetch outstanding sync entries", err)
	}
	defer rows.Close()

	var out []domain.QueueEntry
	for rows.Next() {
		var e domain.QueueEntry
		if err := rows.Scan(&e.ID, &e.Customer, &e.Brand, &e.QueuedAt); err != nil {
			return nil, wrap("scan sync entry", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, wrap("iterate sync entries", err)
	}
	return out, nil
}

// LoadForSync reads the customer's current state and stored contact id.
func (r *Sync) LoadForSync(ctx context.Context, id domain.CustomerID) (domain.SyncCustomer, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	var (
		c                                         domain.SyncCustomer
		email, first, last, phone, country, cName sql.Null[string]
		contactID                                 sql.Null[string]
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT c.CUS_iID, c.CUS_BRD_iID, COALESCE(b.BRD_cName, ''), c.CUS_cEmail, c.CUS_cFirstName,
		       c.CUS_cLastName, c.CUS_cPhone, c.CUS_cCountry, cur.CUR_cAlpha3, c.CUS_dCreated,
		       c.CUS_bActive, pc.PCO_cContactId
		FROM customers c
		LEFT JOIN brands b ON b.BRD_iID = c.CUS_BRD_iID
		LEFT JOIN currencies cur ON cur.CUR_iID = c.CUS_CUR_iID
		LEFT JOIN partner_contacts pc ON pc.PCO_CUS_iID = c.CUS_iID
		WHERE c.CUS_iID = ?`, uint32(id)).
		Scan(&c.ID, &c.Brand, &c.BrandName, &email, &first, &last, &phone, &country, &cName,
			&c.RegisteredAt, &c.Active, &contactID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SyncCustomer{}, domain.ErrCustomerNotFound
	}
	if err != nil {
		return domain.SyncCustomer{}, wrap("load customer for sync", err)
	}
	c.Email, c.FirstName, c.LastName, c.Phone, c.Country = email.V, first.V, last.V, phone.V, country.V
	c.Currency, c.ContactID = cName.V, contactID.V
	return c, nil
}

// RecordSynced stores the contact id and marks exactly the given entries
// processed, atomically: either both happen or neither does. Entries that
// another run processed meanwhile are left untouched (and not counted).
func (r *Sync) RecordSynced(ctx context.Context, id domain.CustomerID, contactID string, entries []uint64) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	var marked int64
	err := database.WithTx(ctx, r.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO partner_contacts (PCO_CUS_iID, PCO_cContactId, PCO_dSynced)
			VALUES (?, ?, UTC_TIMESTAMP()) AS new
			ON DUPLICATE KEY UPDATE PCO_cContactId = new.PCO_cContactId, PCO_dSynced = new.PCO_dSynced`,
			uint32(id), contactID); err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		args := make([]any, 0, len(entries)+1)
		args = append(args, uint32(id))
		for _, e := range entries {
			args = append(args, e)
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE sync_queue SET SYQ_dProcessed = UTC_TIMESTAMP()
			WHERE SYQ_CUS_iID = ? AND SYQ_iID IN (`+placeholders(len(entries))+`) AND SYQ_dProcessed IS NULL`,
			args...)
		if err != nil {
			return err
		}
		marked, err = res.RowsAffected()
		return err
	}, database.Retries(r.txRetries))
	if err != nil {
		return 0, wrap("record sync", err)
	}
	return marked, nil
}
