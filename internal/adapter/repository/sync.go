package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/database"
)

const (
	// sqlSelectOutstandingEntriesPerBrand returns the oldest outstanding
	// entries of every brand in one statement, so a large backlog of one brand
	// cannot starve the others. Entries of missing customers come back with
	// brand 0 (LEFT JOIN) and are reported, never dropped. Customers that are
	// backing off are skipped before ranking, so they do not use up a brand's
	// share; MySQL turns the NOT EXISTS into an anti-join over the id list,
	// which it materializes once (measured: as fast as a literal NOT IN list
	// at 100k outstanding entries and 500 excluded customers, while MEMBER OF
	// re-parses the JSON for every row and was 14x slower).
	//
	// Without an index on SYQ_dProcessed this scans sync_queue; NOTES.md
	// recommends (SYQ_dProcessed, SYQ_iID) plus retention of processed rows.
	//
	// Parameters: excluded customer ids as a JSON array, entries per brand.
	sqlSelectOutstandingEntriesPerBrand = `
		SELECT SYQ_iID, SYQ_CUS_iID, brand, SYQ_dQueued
		FROM (
			SELECT q.SYQ_iID, q.SYQ_CUS_iID, COALESCE(c.CUS_BRD_iID, 0) AS brand, q.SYQ_dQueued,
			       ROW_NUMBER() OVER (PARTITION BY COALESCE(c.CUS_BRD_iID, 0) ORDER BY q.SYQ_iID) AS rn
			FROM sync_queue q
			LEFT JOIN customers c ON c.CUS_iID = q.SYQ_CUS_iID
			WHERE q.SYQ_dProcessed IS NULL
			  AND NOT EXISTS (
			      SELECT 1
			      FROM JSON_TABLE(CAST(? AS JSON), '$[*]' COLUMNS (id INT UNSIGNED PATH '$')) AS backing_off
			      WHERE backing_off.id = q.SYQ_CUS_iID)
		) ranked
		WHERE rn <= ?
		ORDER BY brand, SYQ_iID`

	// sqlSelectCustomerForSync reads a customer's current state and the Acme
	// Support contact id stored by a previous sync.
	//
	// Parameters: customer id.
	sqlSelectCustomerForSync = `
		SELECT c.CUS_iID, c.CUS_BRD_iID, COALESCE(b.BRD_cName, ''), c.CUS_cEmail, c.CUS_cFirstName,
		       c.CUS_cLastName, c.CUS_cPhone, c.CUS_cCountry, cur.CUR_cAlpha3, c.CUS_dCreated,
		       c.CUS_bActive, pc.PCO_cContactId
		FROM customers c
		LEFT JOIN brands b ON b.BRD_iID = c.CUS_BRD_iID
		LEFT JOIN currencies cur ON cur.CUR_iID = c.CUS_CUR_iID
		LEFT JOIN partner_contacts pc ON pc.PCO_CUS_iID = c.CUS_iID
		WHERE c.CUS_iID = ?`

	// sqlUpsertPartnerContact stores (or replaces) a customer's Acme Support
	// contact id.
	//
	// Parameters: customer id, contact id.
	sqlUpsertPartnerContact = `
		INSERT INTO partner_contacts (PCO_CUS_iID, PCO_cContactId, PCO_dSynced)
		VALUES (?, ?, UTC_TIMESTAMP()) AS new
		ON DUPLICATE KEY UPDATE PCO_cContactId = new.PCO_cContactId, PCO_dSynced = new.PCO_dSynced`

	// sqlMarkQueueEntryProcessed marks one outstanding entry of a customer
	// processed. It is a primary-key update, so it locks exactly that row; an
	// entry already processed by another run is left untouched.
	//
	// Parameters: entry id, customer id.
	sqlMarkQueueEntryProcessed = `
		UPDATE sync_queue SET SYQ_dProcessed = UTC_TIMESTAMP()
		WHERE SYQ_iID = ? AND SYQ_CUS_iID = ? AND SYQ_dProcessed IS NULL`
)

// Sync implements the contact sync's persistence on the primary database.
// It must never read from a replica: a lagging customer snapshot could be
// pushed to Acme Support and its queue entry marked processed, silently
// losing the newer change.
type Sync struct {
	db        *sql.DB
	timeout   time.Duration
	txRetries int

	selectOutstanding  *sql.Stmt
	selectCustomer     *sql.Stmt
	upsertContact      *sql.Stmt
	markEntryProcessed *sql.Stmt
}

// NewSync prepares the repository's statements on the primary; Close
// releases them.
func NewSync(ctx context.Context, primary *sql.DB, queryTimeout time.Duration, txRetries int) (*Sync, error) {
	r := &Sync{db: primary, timeout: queryTimeout, txRetries: txRetries}
	if err := prepareAll(ctx, primary,
		statement{&r.selectOutstanding, "select outstanding entries per brand", sqlSelectOutstandingEntriesPerBrand},
		statement{&r.selectCustomer, "select customer for sync", sqlSelectCustomerForSync},
		statement{&r.upsertContact, "upsert partner contact", sqlUpsertPartnerContact},
		statement{&r.markEntryProcessed, "mark queue entry processed", sqlMarkQueueEntryProcessed},
	); err != nil {
		return nil, err
	}
	return r, nil
}

// Close releases the prepared statements.
func (r *Sync) Close() error {
	return closeAll(r.selectOutstanding, r.selectCustomer, r.upsertContact, r.markEntryProcessed)
}

// Outstanding returns the oldest perBrand outstanding entries of every brand,
// skipping the customers in exclude.
func (r *Sync) Outstanding(ctx context.Context, perBrand int, exclude []domain.CustomerID) ([]domain.QueueEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	rows, err := r.selectOutstanding.QueryContext(ctx, jsonIDs(exclude), perBrand)
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
	err := r.selectCustomer.QueryRowContext(ctx, uint32(id)).
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
//
// Entries are marked one primary-key update at a time, which locks exactly
// those rows. A set-based update over a JSON list would leave the access path
// to the optimizer, and an UPDATE that scans sync_queue locks every row it
// reads. A customer rarely has more than a few entries per cycle.
func (r *Sync) RecordSynced(ctx context.Context, id domain.CustomerID, contactID string, entries []uint64) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	var marked int64
	err := database.WithTx(ctx, r.db, func(tx *sql.Tx) error {
		marked = 0 // the whole transaction may be retried
		if _, err := tx.StmtContext(ctx, r.upsertContact).ExecContext(ctx, uint32(id), contactID); err != nil {
			return err
		}
		mark := tx.StmtContext(ctx, r.markEntryProcessed)
		for _, entry := range entries {
			res, err := mark.ExecContext(ctx, entry, uint32(id))
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			marked += n
		}
		return nil
	}, database.Retries(r.txRetries))
	if err != nil {
		return 0, wrap("record sync", err)
	}
	return marked, nil
}
