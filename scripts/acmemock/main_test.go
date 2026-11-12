package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/adapter/acme"
	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
	"github.com/wmiha/vyking-3p-integration-service/internal/service/contactsync"
)

// TestClientAgainstMock is a contract test between our Acme Support client
// and the mock used by the compose stack.
func TestClientAgainstMock(t *testing.T) {
	m := newMock([]string{"key-1", "key-2"}, 600, 3, 0, 0, 0, slog.New(slog.DiscardHandler))
	ts := httptest.NewServer(m.routes())
	defer ts.Close()

	client := acme.New(acme.Config{
		BaseURL: ts.URL, RequestTimeout: time.Second, MaxAttempts: 2, BaseBackoff: time.Millisecond,
		MaxBackoff: time.Millisecond, DefaultRetryAfter: time.Second, RatePerMinute: 6000, Burst: 100,
		BreakerThreshold: 100, BreakerCooldown: time.Second, AuthCooldown: time.Second,
		APIKeys: map[domain.BrandID]string{1: "key-1", 2: "key-2", 9: "unknown-key"},
	}, nil, slog.New(slog.DiscardHandler))
	ctx := context.Background()
	jane := domain.Contact{ExternalID: "1001", Email: "jane@example.com", Name: "Jane", Country: "NO",
		Brand: "Casino", Currency: "EUR", RegisteredAt: time.Now(), Active: true}

	id, err := client.Create(ctx, 1, jane)
	require.NoError(t, err)

	_, err = client.Create(ctx, 1, jane)
	assert.ErrorIs(t, err, contactsync.ErrContactConflict, "external_id is unique per account")

	found, err := client.FindByEmail(ctx, 1, "JANE@example.com")
	require.NoError(t, err)
	assert.Equal(t, id, found)

	jane.Name = "Jane Doe"
	require.NoError(t, client.Update(ctx, 1, id, jane))
	assert.ErrorIs(t, client.Update(ctx, 1, "ct_missing", jane), contactsync.ErrContactNotFound)

	_, err = client.FindByEmail(ctx, 2, "jane@example.com")
	assert.ErrorIs(t, err, contactsync.ErrContactNotFound, "accounts are isolated per API key")
	_, err = client.Create(ctx, 2, jane)
	assert.NoError(t, err, "the same external_id may exist in another brand's account")

	bad := jane
	bad.Country = "Norway"
	_, err = client.Create(ctx, 1, bad)
	assert.ErrorIs(t, err, contactsync.ErrContactRejected)

	_, err = client.Create(ctx, 9, jane)
	assert.ErrorIs(t, err, contactsync.ErrPartnerUnauthorized)

	// Burst of 3 per key at 600/min: the client keeps going by honouring
	// Retry-After instead of failing.
	for i := range 6 {
		c := jane
		c.ExternalID = "x" + string(rune('a'+i))
		_, err := client.Create(ctx, 2, c)
		require.NoError(t, err)
	}
	assert.Positive(t, m.stats["POST 429"], "the mock rate-limited the client")

	resp, err := http.Get(ts.URL + "/_debug/contacts")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
