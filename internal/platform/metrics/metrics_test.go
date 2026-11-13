package metrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
	"github.com/wmiha/vyking-3p-integration-service/internal/service/contactsync"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	return string(body)
}

func TestMetricsExposition(t *testing.T) {
	m := New("v1.2.3")

	m.ObserveRequest("public", "GET /v1/transactions", 200, 20*time.Millisecond)
	m.ObserveRequest("public", "", 404, time.Millisecond)
	m.AuthFailed("token_expired")
	m.PartnerLookup("ok")
	m.CacheResult(true)
	m.CacheResult(false)
	m.RequestDone(1, "create", 201, time.Second)
	m.RequestDone(1, "create", 0, 35*time.Second)
	m.Event(2, "rate_limited")
	m.CustomerSynced(1, contactsync.OutcomeCreated)

	out := scrape(t, m)
	for _, want := range []string{
		`vyking_build_info{version="v1.2.3"} 1`,
		`vyking_http_requests_total{code="200",route="GET /v1/transactions",server="public"} 1`,
		`vyking_http_requests_total{code="404",route="unmatched",server="public"} 1`,
		`vyking_http_request_duration_seconds_bucket{route="GET /v1/transactions",server="public",le="0.025"} 1`,
		`vyking_auth_failures_total{code="token_expired"} 1`,
		`vyking_partner_lookups_total{outcome="ok"} 1`,
		`vyking_cache_requests_total{result="hit"} 1`,
		`vyking_cache_requests_total{result="miss"} 1`,
		`vyking_acme_requests_total{brand="1",op="create",status="201"} 1`,
		`vyking_acme_requests_total{brand="1",op="create",status="error"} 1`,
		`vyking_acme_events_total{brand="2",event="rate_limited"} 1`,
		`vyking_sync_customers_total{brand="1",outcome="created"} 1`,
		`go_goroutines`,
	} {
		assert.Contains(t, out, want)
	}
}

func TestCycleGauges(t *testing.T) {
	m := New("test")
	finished := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	m.CycleCompleted(contactsync.RunSummary{
		Leader: true, Fetched: 7, Orphans: 1, Unconfigured: 2, BackingOff: 3,
		StartedAt: finished.Add(-2 * time.Second), FinishedAt: finished,
		OldestQueued: map[domain.BrandID]time.Time{1: finished.Add(-90 * time.Second), 2: finished.Add(-time.Second)},
	})

	out := scrape(t, m)
	for _, want := range []string{
		`vyking_sync_cycles_total{leader="true",result="ok"} 1`,
		`vyking_sync_entries_fetched 7`,
		`vyking_sync_orphan_entries 1`,
		`vyking_sync_unconfigured_entries 2`,
		`vyking_sync_customers_backing_off 3`,
		`vyking_sync_oldest_outstanding_age_seconds{brand="1"} 90`,
		`vyking_sync_oldest_outstanding_age_seconds{brand="2"} 1`,
		`vyking_sync_cycle_duration_seconds_count 1`,
	} {
		assert.Contains(t, out, want)
	}

	// Leadership moved to another replica: this one must not keep reporting
	// the queue as it looked when it was the leader.
	m.CycleCompleted(contactsync.RunSummary{Leader: false, Error: ""})
	out = scrape(t, m)
	assert.Contains(t, out, `vyking_sync_cycles_total{leader="false",result="ok"} 1`)
	assert.Contains(t, out, `vyking_sync_entries_fetched 0`)
	assert.False(t, strings.Contains(out, `vyking_sync_oldest_outstanding_age_seconds{`), "stale per-brand ages cleared")

	m.CycleCompleted(contactsync.RunSummary{Leader: true, Error: "fetch queue: timeout"})
	assert.Contains(t, scrape(t, m), `vyking_sync_cycles_total{leader="true",result="error"} 1`)
}
