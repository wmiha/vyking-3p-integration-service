//go:build integration

package repository

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/platform/database"
	"github.com/wmiha/vyking-3p-integration-service/internal/service/transactions"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// TestListPageCostIsProportionalToThePage guards the query plan: reading a
// page must walk the index for about `limit` rows, never the customer's whole
// history (which a hash join with the lookup tables used to cause).
func TestListPageCostIsProportionalToThePage(t *testing.T) {
	port, _ := strconv.Atoi(env("IT_DB_PORT", "33306"))
	// One connection, so the session status counters belong to our queries.
	db, err := database.Open(database.Config{
		Host: env("IT_DB_HOST", "127.0.0.1"), Port: port, Name: env("IT_DB_NAME", "vyking"),
		User: "root", Password: env("IT_DB_ROOT_PASSWORD", "it-root-only"), Timezone: "UTC",
		ConnectTimeout: 5 * time.Second, ReadTimeout: time.Minute, WriteTimeout: time.Minute,
		MaxOpenConns: 1, MaxIdleConns: 1,
	})
	require.NoError(t, err)
	defer db.Close()
	ctx := context.Background()
	require.NoError(t, database.WaitReady(ctx, db, time.Second))

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

	repo := NewTransactions(db, 10*time.Second)
	tests := []struct {
		name  string
		query transactions.PageQuery
	}{
		{"first page", transactions.PageQuery{Customer: customer, Limit: 21}},
		{"status filter", transactions.PageQuery{Customer: customer, Limit: 21, StatusIDs: []uint32{70}}},
		{"keyset page", transactions.PageQuery{Customer: customer, Limit: 21,
			After: &transactions.Position{CreatedUnix: base.Add(1500 * time.Minute).Unix(), ID: 901500}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, "FLUSH STATUS")
			require.NoError(t, err)

			page, err := repo.ListPage(ctx, tt.query)
			require.NoError(t, err)
			require.Len(t, page, 21)

			status := map[string]int{}
			rows, err := db.QueryContext(ctx, "SHOW SESSION STATUS WHERE Variable_name IN "+
				"('Handler_read_prev', 'Handler_read_next', 'Handler_read_rnd_next', 'Sort_rows')")
			require.NoError(t, err)
			for rows.Next() {
				var name string
				var value int
				require.NoError(t, rows.Scan(&name, &value))
				status[name] = value
			}
			require.NoError(t, rows.Close())

			read := status["Handler_read_prev"] + status["Handler_read_next"]
			// A third of the rows match the status filter, so it reads ~3x the page.
			assert.Less(t, read, 100, "rows read from the index: %v", status)
			assert.LessOrEqual(t, status["Sort_rows"], 21, "only the page itself may be sorted: %v", status)
			assert.Less(t, status["Handler_read_rnd_next"], 100, "no table scans: %v", status)
		})
	}
}
