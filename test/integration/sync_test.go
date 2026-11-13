//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/config"
	"github.com/wmiha/vyking-3p-integration-service/internal/service/contactsync"
)

// The Acme Support stand-in of docker-compose.test.yml.
var (
	acmeURL  = env("IT_ACME_URL", "http://127.0.0.1:18081")
	acmeKey1 = "it-acme-key-1"
	acmeKey2 = "it-acme-key-2"
)

type mockContact struct {
	ID         string         `json:"id"`
	ExternalID string         `json:"external_id"`
	Email      string         `json:"email"`
	Name       string         `json:"name"`
	Phone      string         `json:"phone"`
	Country    string         `json:"country"`
	Attributes map[string]any `json:"attributes"`
	Updates    int            `json:"updates"`
}

func resetAcme(t *testing.T) {
	t.Helper()
	resp, err := http.Post(acmeURL+"/_debug/reset", "", nil)
	require.NoError(t, err, "acme mock unreachable (run `make test-integration`)")
	resp.Body.Close()
}

// acmeContacts returns the mock's contacts per API key, by external id.
func acmeContacts(t *testing.T) map[string]map[string]mockContact {
	t.Helper()
	resp, err := http.Get(acmeURL + "/_debug/contacts")
	require.NoError(t, err)
	defer resp.Body.Close()
	var raw map[string][]mockContact
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&raw))

	out := map[string]map[string]mockContact{}
	for key, list := range raw {
		out[key] = map[string]mockContact{}
		for _, c := range list {
			_, dup := out[key][c.ExternalID]
			require.False(t, dup, "duplicate contact for external_id %s", c.ExternalID)
			out[key][c.ExternalID] = c
		}
	}
	return out
}

// createInAcme simulates a contact we created before crashing, i.e. without
// its id ever reaching partner_contacts.
func createInAcme(t *testing.T, key string, body string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, acmeURL+"/v2/contacts", bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var r struct{ ID string }
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&r))
	return r.ID
}

func syncConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := serviceConfig(t)
	cfg.Acme = config.Acme{
		BaseURL: acmeURL, RequestTimeout: 5 * time.Second, MaxAttempts: 2,
		BaseBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, DefaultRetryAfter: time.Second,
		RatePerMinute: 6000, Burst: 100, BreakerThreshold: 5, BreakerCooldown: time.Second, AuthCooldown: time.Minute,
		Brands: []config.AcmeBrand{{BrandID: 1, APIKey: acmeKey1}, {BrandID: 2, APIKey: acmeKey2}},
	}
	cfg.Sync = config.Sync{
		Enabled: true, Interval: time.Hour, BatchPerBrand: 50, WorkersPerBrand: 4, JobTimeout: 30 * time.Second,
		LockName: "it-contact-sync", LockSessionTimeout: time.Minute, LockCheckInterval: time.Second,
		BackoffInitial: time.Hour, BackoffMax: 2 * time.Hour,
	}
	return cfg
}

// waitForCycle polls the internal status endpoint until a cycle that started
// after `after` has completed, and returns its summary.
func waitForCycle(t *testing.T, internalAddr string, after time.Time) contactsync.RunSummary {
	t.Helper()
	var summary contactsync.RunSummary
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + internalAddr + "/internal/sync/status")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		var s contactsync.RunSummary
		if json.NewDecoder(resp.Body).Decode(&s) != nil || s.RunID == "" || !s.StartedAt.After(after) {
			return false
		}
		summary = s
		return true
	}, 30*time.Second, 50*time.Millisecond, "no sync cycle completed")
	return summary
}

func queueOutstanding(t *testing.T) []uint64 {
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

func storedContacts(t *testing.T) map[uint32]string {
	t.Helper()
	rows, err := root.Query("SELECT PCO_CUS_iID, PCO_cContactId FROM partner_contacts")
	require.NoError(t, err)
	defer rows.Close()
	out := map[uint32]string{}
	for rows.Next() {
		var id uint32
		var contact string
		require.NoError(t, rows.Scan(&id, &contact))
		out[id] = contact
	}
	return out
}

func TestContactSyncEndToEnd(t *testing.T) {
	resetData(t)
	resetAcme(t)
	exec(t,
		`INSERT INTO brands VALUES (1, 'Vyking Casino'), (2, 'Vyking Sports'), (3, 'Vyking Poker')`,
		`INSERT INTO customers (CUS_iID, CUS_BRD_iID, CUS_cUserName, CUS_cEmail, CUS_cFirstName, CUS_cLastName,
			CUS_cPhone, CUS_cCountry, CUS_CUR_iID, CUS_dCreated, CUS_bActive) VALUES
			(1, 1, 'jane', 'jane@example.com', 'Jane', 'Doe', '+4712345678', 'NO', 1, '2025-01-02 03:04:05', 1),
			(2, 1, NULL, 'two@example.com', NULL, NULL, NULL, NULL, 3, '2025-01-01 00:00:00', 0),
			(3, 2, NULL, 'three@example.com', 'Tri', NULL, NULL, 'GB', 2, '2025-01-01 00:00:00', 1),
			(4, 3, NULL, 'poker@example.com', NULL, NULL, NULL, NULL, 1, '2025-01-01 00:00:00', 1),
			(5, 1, NULL, 'five@example.com', 'Five', NULL, NULL, NULL, 1, '2025-01-01 00:00:00', 1),
			(6, 1, NULL, 'six@example.com', 'Six', NULL, NULL, NULL, 1, '2025-01-01 00:00:00', 1),
			(7, 1, NULL, 'seven@example.com', 'Seven', NULL, NULL, 'no', 1, '2025-01-01 00:00:00', 1)`,
		// Customer 1 twice (coalesced), 4 has no Acme key, 99 does not exist.
		`INSERT INTO sync_queue VALUES
			(1, 1, '2026-11-01 10:00:00', NULL), (2, 2, '2026-11-01 10:00:01', NULL),
			(3, 1, '2026-11-01 10:00:02', NULL), (4, 3, '2026-11-01 10:00:03', NULL),
			(5, 4, '2026-11-01 10:00:04', NULL), (6, 5, '2026-11-01 10:00:05', NULL),
			(7, 6, '2026-11-01 10:00:06', NULL), (8, 7, '2026-11-01 10:00:07', NULL),
			(9, 99, '2026-11-01 10:00:08', NULL)`,
		// Customer 6 has an id Acme Support no longer knows.
		`INSERT INTO partner_contacts VALUES (6, 'ct_deleted_in_acme', '2026-10-01 00:00:00')`,
	)
	// Customer 5 was created in Acme before a crash, its id never stored.
	precreated := createInAcme(t, acmeKey1, `{"external_id":"5","email":"five@example.com","name":"old"}`)

	cfg := syncConfig(t)
	start := time.Now().Add(-time.Second)
	startService(t, cfg)

	t.Run("first cycle", func(t *testing.T) {
		s := waitForCycle(t, cfg.Server.Internal.Addr, start)
		assert.True(t, s.Leader)
		assert.Equal(t, map[string]int{
			contactsync.OutcomeCreated:   4, // 1, 2, 3 and 6 (stale id recreated)
			contactsync.OutcomeRecovered: 1, // 5: 409 resolved through the email lookup
			contactsync.OutcomeRejected:  1, // 7: Acme refused country "no"
		}, s.Outcomes)
		assert.Equal(t, 1, s.Orphans)
		assert.Equal(t, 1, s.Unconfigured)
		assert.Equal(t, 1, s.BackingOff)

		assert.Equal(t, []uint64{5, 8, 9}, queueOutstanding(t),
			"unconfigured brand, rejected data and orphan stay outstanding; nothing else does")

		contacts := acmeContacts(t)
		require.Len(t, contacts[acmeKey1], 4, "1, 2, 5, 6: no duplicates")
		require.Len(t, contacts[acmeKey2], 1, "3 in brand 2's own account")

		stored := storedContacts(t)
		assert.Len(t, stored, 5)
		assert.Equal(t, precreated, stored[5], "the existing contact was adopted")
		assert.Equal(t, contacts[acmeKey1]["6"].ID, stored[6], "the stale id was replaced")
		assert.Equal(t, contacts[acmeKey1]["1"].ID, stored[1])

		jane := contacts[acmeKey1]["1"]
		assert.Equal(t, mockContact{
			ID: stored[1], ExternalID: "1", Email: "jane@example.com", Name: "Jane Doe", Phone: "+4712345678",
			Country: "NO", Attributes: map[string]any{"brand": "Vyking Casino", "currency": "EUR",
				"registered_at": "2025-01-02T03:04:05Z", "active": true},
		}, jane)
		assert.Equal(t, "five@example.com", contacts[acmeKey1]["5"].Email)
		assert.Equal(t, 1, contacts[acmeKey1]["5"].Updates, "recovered contact updated with current data")
		assert.Equal(t, false, contacts[acmeKey1]["2"].Attributes["active"])
	})

	t.Run("a change is pushed as an update, never a duplicate", func(t *testing.T) {
		exec(t,
			`UPDATE customers SET CUS_cFirstName = 'Janet' WHERE CUS_iID = 1`,
			`INSERT INTO sync_queue VALUES (10, 1, UTC_TIMESTAMP(), NULL)`,
		)
		triggered := time.Now()
		resp, err := http.Post("http://"+cfg.Server.Internal.Addr+"/internal/sync/run", "", nil)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusAccepted, resp.StatusCode)

		s := waitForCycle(t, cfg.Server.Internal.Addr, triggered)
		assert.Equal(t, map[string]int{contactsync.OutcomeUpdated: 1}, s.Outcomes,
			"customer 7 is backing off and not retried every cycle")

		contacts := acmeContacts(t)
		assert.Len(t, contacts[acmeKey1], 4)
		assert.Equal(t, "Janet Doe", contacts[acmeKey1]["1"].Name)
		assert.Equal(t, []uint64{5, 8, 9}, queueOutstanding(t))
	})
}
