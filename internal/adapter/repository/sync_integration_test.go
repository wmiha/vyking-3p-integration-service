//go:build integration

package repository

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/database"
)

// openIT connects to the integration database. Repository calls run as the
// least-privilege app user, which proves its grants suffice; root sets up
// fixtures and checks results.
func openIT(t *testing.T, user, password string, conns int) *sql.DB {
	t.Helper()
	port, _ := strconv.Atoi(env("IT_DB_PORT", "33306"))
	db, err := database.Open(database.Config{
		Host: env("IT_DB_HOST", "127.0.0.1"), Port: port, Name: env("IT_DB_NAME", "vyking"),
		User: user, Password: password, Timezone: "UTC",
		ConnectTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second,
		MaxOpenConns: conns, MaxIdleConns: conns,
	})
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, database.WaitReady(ctx, db, time.Second))
	return db
}

func rootDB(t *testing.T) *sql.DB {
	return openIT(t, "root", env("IT_DB_ROOT_PASSWORD", "it-root-only"), 2)
}

func appDB(t *testing.T) *sql.DB {
	return openIT(t, "app", env("IT_DB_APP_PASSWORD", "it-app-only"), 5)
}

func mustExec(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		_, err := db.Exec(s)
		require.NoError(t, err, s)
	}
}

func resetSyncTables(t *testing.T, root *sql.DB) {
	t.Helper()
	mustExec(t, root, "DELETE FROM partner_contacts", "DELETE FROM sync_queue", "DELETE FROM customers", "DELETE FROM brands")
	mustExec(t, root,
		`INSERT INTO brands VALUES (1, 'Vyking Casino'), (2, 'Vyking Sports')`,
		`INSERT INTO customers (CUS_iID, CUS_BRD_iID, CUS_cUserName, CUS_cEmail, CUS_cFirstName, CUS_cLastName,
			CUS_cPhone, CUS_cCountry, CUS_CUR_iID, CUS_dCreated, CUS_bActive) VALUES
			(1, 1, 'jane', 'jane@example.com', 'Jane', 'Doe', '+4712345678', 'NO', 1, '2025-01-02 03:04:05', 1),
			(2, 1, NULL, NULL, NULL, NULL, NULL, NULL, 3, '2025-01-01 00:00:00', 0),
			(3, 1, NULL, 'c3@example.com', NULL, NULL, NULL, NULL, 1, '2025-01-01 00:00:00', 1),
			(4, 1, NULL, 'c4@example.com', NULL, NULL, NULL, NULL, 1, '2025-01-01 00:00:00', 1),
			(7, 2, NULL, 'c7@example.com', NULL, NULL, NULL, NULL, 2, '2025-01-01 00:00:00', 1)`,
	)
}

func outstanding(t *testing.T, root *sql.DB) []uint64 {
	t.Helper()
	rows, err := root.Query("SELECT SYQ_iID FROM sync_queue WHERE SYQ_dProcessed IS NULL ORDER BY SYQ_iID")
	require.NoError(t, err)
	defer rows.Close()
	var ids []uint64
	for rows.Next() {
		var id uint64
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	return ids
}

func TestOutstandingIsFairPerBrand(t *testing.T) {
	root, app := rootDB(t), appDB(t)
	resetSyncTables(t, root)
	mustExec(t, root, `INSERT INTO sync_queue VALUES
		(1, 1, '2026-11-01 10:00:00', NULL),
		(2, 2, '2026-11-01 10:00:01', NULL),
		(3, 1, '2026-11-01 10:00:02', NULL),
		(4, 3, '2026-11-01 10:00:03', NULL),
		(5, 4, '2026-11-01 10:00:04', NULL),
		(6, 7, '2026-11-01 10:00:05', NULL),
		(7, 99, '2026-11-01 10:00:06', NULL),
		(8, 1, '2026-11-01 09:00:00', '2026-11-01 09:01:00')`)

	repo := newSync(t, app)

	type got struct {
		id       uint64
		customer domain.CustomerID
		brand    domain.BrandID
	}
	tests := []struct {
		name    string
		exclude []domain.CustomerID
		want    []got
	}{
		{
			name:    "backing-off customer excluded",
			exclude: []domain.CustomerID{2},
			want: []got{
				{7, 99, 0},                      // orphan: customer does not exist, surfaced with brand 0
				{1, 1, 1}, {3, 1, 1}, {4, 3, 1}, // brand 1: oldest three, customer 2 excluded, entry 8 processed
				{6, 7, 2}, // brand 2 is not starved by brand 1's backlog
			},
		},
		{
			name:    "nothing excluded",
			exclude: nil,
			want:    []got{{7, 99, 0}, {1, 1, 1}, {2, 2, 1}, {3, 1, 1}, {6, 7, 2}},
		},
		{
			name:    "several customers excluded",
			exclude: []domain.CustomerID{1, 2, 7},
			want:    []got{{7, 99, 0}, {4, 3, 1}, {5, 4, 1}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries, err := repo.Outstanding(context.Background(), 3, tt.exclude)
			require.NoError(t, err)

			var ids []got
			for _, e := range entries {
				ids = append(ids, got{e.ID, e.Customer, e.Brand})
				assert.False(t, e.QueuedAt.IsZero())
			}
			assert.Equal(t, tt.want, ids)
		})
	}
}

// newSync prepares a Sync repository on db for the duration of the test.
func newSync(t *testing.T, db *sql.DB) *Sync {
	t.Helper()
	repo, err := NewSync(context.Background(), db, 5*time.Second, 3)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, repo.Close()) })
	return repo
}

func TestLoadForSync(t *testing.T) {
	root, app := rootDB(t), appDB(t)
	resetSyncTables(t, root)
	mustExec(t, root, `INSERT INTO partner_contacts VALUES (1, 'ct-1', UTC_TIMESTAMP())`)
	repo := newSync(t, app)
	ctx := context.Background()

	c, err := repo.LoadForSync(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, domain.SyncCustomer{
		ID: 1, Brand: 1, BrandName: "Vyking Casino", Email: "jane@example.com", FirstName: "Jane", LastName: "Doe",
		Phone: "+4712345678", Country: "NO", Currency: "EUR",
		RegisteredAt: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC), Active: true, ContactID: "ct-1",
	}, c)

	c, err = repo.LoadForSync(ctx, 2)
	require.NoError(t, err)
	assert.Equal(t, domain.SyncCustomer{ID: 2, Brand: 1, BrandName: "Vyking Casino", Currency: "JPY",
		RegisteredAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}, c, "NULL columns become empty values")

	_, err = repo.LoadForSync(ctx, 99)
	assert.ErrorIs(t, err, domain.ErrCustomerNotFound)
}

func TestRecordSyncedIsAtomic(t *testing.T) {
	root, app := rootDB(t), appDB(t)
	resetSyncTables(t, root)
	mustExec(t, root, `INSERT INTO sync_queue VALUES
		(1, 1, '2026-11-01 10:00:00', NULL),
		(2, 1, '2026-11-01 10:00:01', NULL),
		(3, 3, '2026-11-01 10:00:02', NULL)`)
	repo := newSync(t, app)
	ctx := context.Background()

	// PCO_cContactId is VARCHAR(64): the insert fails, so nothing may change.
	_, err := repo.RecordSynced(ctx, 1, strings.Repeat("x", 65), []uint64{1, 2})
	require.Error(t, err)
	assert.Equal(t, []uint64{1, 2, 3}, outstanding(t, root), "no entry marked without a stored contact id")
	var n int
	require.NoError(t, root.QueryRow("SELECT COUNT(*) FROM partner_contacts").Scan(&n))
	assert.Zero(t, n)

	marked, err := repo.RecordSynced(ctx, 1, "ct-1", []uint64{1})
	require.NoError(t, err)
	assert.Equal(t, int64(1), marked)
	assert.Equal(t, []uint64{2, 3}, outstanding(t, root), "only the given entries are marked")

	marked, err = repo.RecordSynced(ctx, 1, "ct-2", []uint64{1, 2, 3})
	require.NoError(t, err)
	assert.Equal(t, int64(1), marked, "entry 1 was processed already; entry 3 belongs to another customer")
	assert.Equal(t, []uint64{3}, outstanding(t, root))

	var contactID string
	require.NoError(t, root.QueryRow("SELECT PCO_cContactId FROM partner_contacts WHERE PCO_CUS_iID = 1").Scan(&contactID))
	assert.Equal(t, "ct-2", contactID, "the contact id is upserted")
}

func TestLockerElectsOneLeader(t *testing.T) {
	root, app := rootDB(t), appDB(t)
	ctx := context.Background()
	locker := NewLocker(app, 10*time.Second, 0) // check the lock on every Held call

	lease, ok, err := locker.TryLock(ctx, "it-sync-lock")
	require.NoError(t, err)
	require.True(t, ok)

	_, ok, err = locker.TryLock(ctx, "it-sync-lock")
	require.NoError(t, err)
	assert.False(t, ok, "a second contender does not get the lock")
	require.NoError(t, lease.Held(ctx))

	// The leader's session dies (crash, network partition killed by the server).
	var connID int64
	require.NoError(t, root.QueryRow("SELECT IS_USED_LOCK('it-sync-lock')").Scan(&connID))
	mustExec(t, root, "KILL "+strconv.FormatInt(connID, 10))

	assert.ErrorIs(t, lease.Held(ctx), ErrLockLost, "the old leader notices it lost the lock")
	_ = lease.Release(ctx)

	next, ok, err := locker.TryLock(ctx, "it-sync-lock")
	require.NoError(t, err)
	require.True(t, ok, "another contender takes over")
	require.NoError(t, next.Release(ctx))

	again, ok, err := locker.TryLock(ctx, "it-sync-lock")
	require.NoError(t, err)
	require.True(t, ok, "a released lock is free")
	require.NoError(t, again.Release(ctx))
	assert.Zero(t, app.Stats().InUse, "lock connections are not leaked")
}
