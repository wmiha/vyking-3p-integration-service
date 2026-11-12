package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testOptions() options {
	return options{
		customers: 50, maxTx: 10, queueShare: 0.5, seed: 7,
		now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	assert.Equal(t, generate(testOptions()), generate(testOptions()))

	other := testOptions()
	other.seed = 8
	assert.NotEqual(t, generate(testOptions()).transactions, generate(other).transactions)
}

func TestGenerateCoversEdgeCases(t *testing.T) {
	ds := generate(testOptions())

	byCustomer := map[int][]row{}
	for _, tx := range ds.transactions {
		byCustomer[tx[1].(int)] = append(byCustomer[tx[1].(int)], tx)
	}

	t.Run("every fixture sees every status", func(t *testing.T) {
		for _, c := range fixtures(testOptions().now) {
			seen := map[int]bool{}
			for _, tx := range byCustomer[c.id] {
				seen[tx[3].(int)] = true
			}
			for _, s := range statuses {
				assert.True(t, seen[s], "customer %d misses status %d", c.id, s)
			}
		}
	})

	t.Run("heavy customer can be paged", func(t *testing.T) {
		assert.Len(t, byCustomer[1009], 250)
	})

	t.Run("same email in two brands", func(t *testing.T) {
		brandsByEmail := map[any][]any{}
		for _, c := range ds.customers {
			if c[3] != nil {
				brandsByEmail[c[3]] = append(brandsByEmail[c[3]], c[1])
			}
		}
		assert.ElementsMatch(t, []any{1, 2}, brandsByEmail["jane.doe@example.com"])
	})

	t.Run("unique ids", func(t *testing.T) {
		for name, rows := range map[string][]row{"customers": ds.customers, "transactions": ds.transactions, "sync_queue": ds.syncQueue} {
			ids := map[any]bool{}
			for _, r := range rows {
				require.False(t, ids[r[0]], "%s id %v duplicated", name, r[0])
				ids[r[0]] = true
			}
		}
	})

	t.Run("queue has duplicates and history", func(t *testing.T) {
		outstanding := map[any]int{}
		processed := 0
		for _, q := range ds.syncQueue {
			if q[3] == nil {
				outstanding[q[1]]++
			} else {
				processed++
			}
		}
		dupes := 0
		for _, n := range outstanding {
			if n > 1 {
				dupes++
			}
		}
		assert.Positive(t, dupes, "some customers need several outstanding entries")
		assert.Positive(t, processed, "some entries must already be processed")
		for _, c := range fixtures(testOptions().now) {
			assert.Positive(t, outstanding[c.id], "fixture %d must be queued", c.id)
		}
	})

	t.Run("nullable columns are exercised", func(t *testing.T) {
		var nullAmount, nullMethod, nullBooked bool
		for _, tx := range ds.transactions {
			nullAmount = nullAmount || tx[4] == nil
			nullMethod = nullMethod || tx[5] == nil
			nullBooked = nullBooked || tx[7] == nil
		}
		assert.True(t, nullAmount && nullMethod && nullBooked)
	})
}
