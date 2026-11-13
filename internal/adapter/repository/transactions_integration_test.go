//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/database"
	"github.com/wmiha/vyking-3p-integration-service/internal/service/transactions"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// singleConnection opens a root pool with exactly one connection, so the
// session status counters read by a test belong to its own queries.
func singleConnection(t *testing.T) *sql.DB {
	t.Helper()
	port, _ := strconv.Atoi(env("IT_DB_PORT", "33306"))
	db, err := database.Open(database.Config{
		Host: env("IT_DB_HOST", "127.0.0.1"), Port: port, Name: env("IT_DB_NAME", "vyking"),
		User: "root", Password: env("IT_DB_ROOT_PASSWORD", "it-root-only"), Timezone: "UTC",
		ConnectTimeout: 5 * time.Second, ReadTimeout: time.Minute, WriteTimeout: time.Minute,
		MaxOpenConns: 1, MaxIdleConns: 1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	require.NoError(t, database.WaitReady(context.Background(), db, time.Second))
	return db
}

// sessionStatus reads the named session status counters.
func sessionStatus(t *testing.T, db *sql.DB, names ...string) map[string]int {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		"SHOW SESSION STATUS WHERE Variable_name IN ('"+strings.Join(names, "', '")+"')")
	require.NoError(t, err)
	defer rows.Close()
	status := map[string]int{}
	for rows.Next() {
		var name string
		var value int
		require.NoError(t, rows.Scan(&name, &value))
		status[name] = value
	}
	require.NoError(t, rows.Err())
	return status
}

// TestListPageCostIsProportionalToThePage guards the query plan: reading a
// page must walk the index for about `limit` rows, never the customer's whole
// history (which a hash join with the lookup tables used to cause).
func TestListPageCostIsProportionalToThePage(t *testing.T) {
	db := singleConnection(t)
	ctx := context.Background()

	const customer, history = 7777, 3000
	cleanup := func() {
		_, err := db.ExecContext(ctx, "DELETE FROM transactions WHERE TRX_CUS_iID = ?", customer)
		require.NoError(t, err)
	}
	cleanup()
	defer cleanup()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for start := 0; start < history; start += 500 {
		var rows []string
		for i := start; i < start+500; i++ {
			status := []int{60, 70, 10}[i%3]
			rows = append(rows, fmt.Sprintf("(%d, %d, 1001, %d, 100, 1, '%s', NULL, '%[4]s')",
				900000+i, customer, status, base.Add(time.Duration(i)*time.Minute).Format(time.DateTime)))
		}
		_, err := db.ExecContext(ctx, "INSERT INTO transactions VALUES "+strings.Join(rows, ","))
		require.NoError(t, err)
	}

	repo, err := NewTransactions(ctx, db, 10*time.Second)
	require.NoError(t, err)
	defer repo.Close()

	cursor := &transactions.Position{CreatedUnix: base.Add(1500 * time.Minute).Unix(), ID: 901500}
	tests := []struct {
		name  string
		query transactions.PageQuery
	}{
		{"first page", transactions.PageQuery{Customer: customer, Limit: 21}},
		{"status filter", transactions.PageQuery{Customer: customer, Limit: 21, StatusIDs: []uint32{70}}},
		{"keyset page", transactions.PageQuery{Customer: customer, Limit: 21, After: cursor}},
		{"keyset page with status filter", transactions.PageQuery{Customer: customer, Limit: 21,
			StatusIDs: []uint32{60, 61}, After: cursor}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, "FLUSH STATUS")
			require.NoError(t, err)

			page, err := repo.ListPage(ctx, tt.query)
			require.NoError(t, err)
			require.Len(t, page, 21)
			for _, tx := range page {
				if len(tt.query.StatusIDs) > 0 {
					assert.Contains(t, tt.query.StatusIDs, tx.StatusID, "status filter applied")
				}
				if tt.query.After != nil {
					assert.Less(t, tx.ID, tt.query.After.ID, "page starts after the cursor")
				}
			}

			status := sessionStatus(t, db, "Handler_read_prev", "Handler_read_next", "Handler_read_rnd_next", "Sort_rows")
			read := status["Handler_read_prev"] + status["Handler_read_next"]
			// A third of the rows match the status filter, so it reads ~3x the page.
			assert.Less(t, read, 100, "rows read from the index: %v", status)
			assert.LessOrEqual(t, status["Sort_rows"], 21, "only the page itself may be sorted: %v", status)
			assert.Less(t, status["Handler_read_rnd_next"], 100, "no table scans: %v", status)
		})
	}
}

// TestStatementsArePreparedOnce proves the partner API's queries are prepared
// when the repositories are created and only executed afterwards.
func TestStatementsArePreparedOnce(t *testing.T) {
	db := singleConnection(t)
	ctx := context.Background()

	customers, err := NewCustomers(ctx, db, 5*time.Second)
	require.NoError(t, err)
	defer customers.Close()
	txs, err := NewTransactions(ctx, db, 5*time.Second)
	require.NoError(t, err)
	defer txs.Close()

	_, err = db.ExecContext(ctx, "FLUSH STATUS")
	require.NoError(t, err)

	const calls = 3
	for range calls {
		_, err := customers.FindByEmail(ctx, "nobody@example.com", []domain.BrandID{1, 2})
		require.NoError(t, err)
		_, err = txs.ListPage(ctx, transactions.PageQuery{Customer: 1, Limit: 5})
		require.NoError(t, err)
		_, err = txs.ListPage(ctx, transactions.PageQuery{Customer: 1, Limit: 5, StatusIDs: []uint32{60},
			After: &transactions.Position{CreatedUnix: time.Now().Unix(), ID: 1}})
		require.NoError(t, err)
	}

	status := sessionStatus(t, db, "Com_stmt_prepare", "Com_stmt_execute")
	assert.Zero(t, status["Com_stmt_prepare"], "no statement is prepared per call: %v", status)
	assert.Equal(t, 3*calls, status["Com_stmt_execute"], "every call executes a prepared statement: %v", status)
}
