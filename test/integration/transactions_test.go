//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/auth"
	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
)

// seedTransactions creates the partner API fixtures: the same email in two
// brands, currencies with 0/2/3/NULL/unknown minor units, missing reference
// rows (no foreign keys) and a customer with ties on TRX_dCreated.
func seedTransactions(t *testing.T) {
	t.Helper()
	resetData(t)
	exec(t,
		`INSERT INTO brands VALUES (1, 'Vyking Casino'), (2, 'Vyking Sports'), (3, 'Vyking Poker')`,
		`INSERT INTO payment_methods VALUES (1, 'PAYIN CARD')`,
		`INSERT INTO currencies VALUES (6, 'XTS', NULL)`,
		`INSERT INTO customers (CUS_iID, CUS_BRD_iID, CUS_cEmail, CUS_CUR_iID, CUS_dCreated, CUS_bActive) VALUES
			(1001, 1, 'jane.doe@example.com', 1, '2025-01-01 00:00:00', 1),
			(1002, 2, 'jane.doe@example.com', 2, '2025-01-01 00:00:00', 1),
			(1003, 1, 'taro@example.jp',      3, '2025-01-01 00:00:00', 1),
			(1004, 1, 'ali@example.bh',       4, '2025-01-01 00:00:00', 1),
			(1005, 2, 'sam@example.com',      5, '2025-01-01 00:00:00', 1),
			(1006, 1, 'xts@example.com',      6, '2025-01-01 00:00:00', 0),
			(1009, 1, 'heavy@example.com',    1, '2025-01-01 00:00:00', 1)`,
		`INSERT INTO transactions VALUES
			(1, 1001, 1001, 60, 12345, 1,    '2026-11-01 10:00:00', '2026-11-01 10:01:00', '2026-11-01 11:00:00'),
			(2, 1001, 2001, 71, 500,   1,    '2026-11-01 10:05:00', NULL,                  '2026-11-01 10:06:00'),
			(3, 1001, 1001, 82, NULL,  NULL, '2026-11-01 10:10:00', NULL,                  '2026-11-01 10:20:00'),
			(4, 1001, 1001, 90, 700,   1,    '2026-11-01 09:00:00', '2026-11-01 09:01:00', '2026-11-02 09:00:00'),
			(5, 1001, 4001, 10, 100,   99,   '2026-11-01 08:00:00', NULL,                  '2026-11-01 08:00:00'),
			(6, 1002, 1001, 60, 999,   1,    '2026-11-01 10:00:00', NULL,                  '2026-11-01 10:00:00'),
			(7, 1003, 1001, 60, 1000,  1,    '2026-11-01 10:00:00', NULL,                  '2026-11-01 10:00:00'),
			(8, 1004, 1001, 60, 1234,  1,    '2026-11-01 10:00:00', NULL,                  '2026-11-01 10:00:00'),
			(9, 1005, 1001, 60, 1234,  1,    '2026-11-01 10:00:00', NULL,                  '2026-11-01 10:00:00'),
			(10, 1006, 1001, 60, 1234, 1,    '2026-11-01 10:00:00', NULL,                  '2026-11-01 10:00:00')`,
	)

	// 25 transactions for the heavy customer, three per timestamp, so pages
	// must break ties on the id.
	var rows []string
	for id := 101; id <= 125; id++ {
		created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(id/3) * time.Minute)
		rows = append(rows, fmt.Sprintf("(%d, 1009, 1001, 60, 100, 1, '%s', NULL, '%[2]s')", id, created.Format(time.DateTime)))
	}
	exec(t, "INSERT INTO transactions VALUES "+strings.Join(rows, ", "))
}

func token(t *testing.T, mutate func(*auth.Claims)) string {
	t.Helper()
	now := time.Now()
	c := auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "vyking-integration", Audience: jwt.ClaimStrings{"vyking-partner-api"}, Subject: "acme-support",
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
		Brands: []domain.BrandID{1},
		Scope:  domain.ScopeTransactionsRead,
	}
	if mutate != nil {
		mutate(&c)
	}
	tok, err := auth.Sign(c, auth.Key{ID: "it-1", Secret: []byte(testJWTSecret)})
	require.NoError(t, err)
	return tok
}

func brands(ids ...domain.BrandID) func(*auth.Claims) {
	return func(c *auth.Claims) { c.Brands = ids }
}

type apiClient struct {
	t    *testing.T
	base string
}

func (c apiClient) get(tok string, query url.Values) (int, map[string]any) {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.base+"/v1/transactions?"+query.Encode(), nil)
	require.NoError(c.t, err)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(c.t, err)
	require.NotEmpty(c.t, resp.Header.Get("X-Request-ID"))

	var body map[string]any
	require.NoError(c.t, json.Unmarshal(raw, &body), string(raw))
	return resp.StatusCode, body
}

func TestTransactionsEndToEnd(t *testing.T) {
	seedTransactions(t)
	cfg := serviceConfig(t)
	startService(t, cfg)
	api := apiClient{t: t, base: "http://" + cfg.Server.Public.Addr}

	t.Run("customer and mapped transactions", func(t *testing.T) {
		status, body := api.get(token(t, nil), url.Values{"email": {"jane.doe@example.com"}})
		require.Equal(t, http.StatusOK, status, body)

		assert.Equal(t, map[string]any{"id": "1001", "brand_id": float64(1), "brand": "Vyking Casino",
			"currency": "EUR", "active": true}, body["customer"])

		txs := body["transactions"].([]any)
		require.Len(t, txs, 5)
		var ids []string
		for _, tx := range txs {
			ids = append(ids, tx.(map[string]any)["id"].(string))
		}
		assert.Equal(t, []string{"3", "2", "1", "4", "5"}, ids, "newest first")

		settled := txs[2].(map[string]any)
		assert.Equal(t, map[string]any{
			"id": "1", "type": "payin", "amount": 123.45, "currency": "EUR",
			"status": "completed", "status_detail": "settled", "failure_reason": nil,
			"payment_method": "PAYIN CARD", "created_at": "2026-11-01T10:00:00Z",
			"booked_at": "2026-11-01T10:01:00Z", "settled_at": "2026-11-01T11:00:00Z",
			"updated_at": "2026-11-01T11:00:00Z",
		}, settled)

		declined := txs[1].(map[string]any)
		assert.Equal(t, "failed", declined["status"])
		assert.Equal(t, "declined", declined["failure_reason"], "risk declines are not disclosed")
		assert.Equal(t, "payout", declined["type"])

		timedOut := txs[0].(map[string]any)
		assert.Equal(t, "cancelled", timedOut["status"])
		assert.Equal(t, "timed_out", timedOut["failure_reason"])
		assert.Nil(t, timedOut["amount"], "NULL amount stays null")
		assert.Nil(t, timedOut["payment_method"])

		reversed := txs[3].(map[string]any)
		assert.Equal(t, "completed", reversed["status"])
		assert.Equal(t, "reversed", reversed["status_detail"])
		assert.Nil(t, reversed["settled_at"], "settlement time unknown after a reversal")

		orphan := txs[4].(map[string]any)
		assert.Equal(t, "unknown", orphan["type"], "missing transaction type row does not hide the transaction")
		assert.Nil(t, orphan["payment_method"], "missing payment method row")

		assert.Equal(t, map[string]any{"limit": float64(20), "has_more": false, "next_cursor": nil}, body["page"])
	})

	t.Run("email lookup is case-insensitive", func(t *testing.T) {
		status, body := api.get(token(t, nil), url.Values{"email": {"Jane.Doe@Example.COM"}})
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, "1001", body["customer"].(map[string]any)["id"])

		// The column collation ignores accents too; that must not disclose
		// another customer's data.
		status, body = api.get(token(t, nil), url.Values{"email": {"jané.doe@example.com"}})
		assert.Equal(t, http.StatusNotFound, status)
		assert.Equal(t, "customer_not_found", body["error"].(map[string]any)["code"])
	})

	t.Run("status filter", func(t *testing.T) {
		status, body := api.get(token(t, nil), url.Values{"email": {"jane.doe@example.com"}, "status": {"failed,cancelled"}})
		require.Equal(t, http.StatusOK, status)
		var got []string
		for _, tx := range body["transactions"].([]any) {
			got = append(got, tx.(map[string]any)["status"].(string))
		}
		assert.Equal(t, []string{"cancelled", "failed"}, got)
	})

	t.Run("amounts per currency", func(t *testing.T) {
		for email, want := range map[string]any{
			"taro@example.jp": float64(1000), // JPY, 0 decimals
			"ali@example.bh":  1.234,         // BHD, 3 decimals
			"xts@example.com": nil,           // unknown minor units: never guess
		} {
			status, body := api.get(token(t, nil), url.Values{"email": {email}})
			require.Equal(t, http.StatusOK, status, email)
			assert.Equal(t, want, body["transactions"].([]any)[0].(map[string]any)["amount"], email)
		}
		status, body := api.get(token(t, brands(2)), url.Values{"email": {"sam@example.com"}})
		require.Equal(t, http.StatusOK, status)
		assert.Equal(t, 12.34, body["transactions"].([]any)[0].(map[string]any)["amount"], "USD falls back to ISO 4217")
	})

	t.Run("brand scoping", func(t *testing.T) {
		tests := []struct {
			name       string
			tok        string
			query      url.Values
			wantStatus int
			wantCode   string
			wantID     string
		}{
			{"other brand is not found", token(t, brands(2)), url.Values{"email": {"taro@example.jp"}}, 404, "customer_not_found", ""},
			{"same email, one brand", token(t, brands(2)), url.Values{"email": {"jane.doe@example.com"}}, 200, "", "1002"},
			{"same email, two brands", token(t, brands(1, 2)), url.Values{"email": {"jane.doe@example.com"}}, 409, "customer_ambiguous", ""},
			{"disambiguated by brand_id", token(t, brands(1, 2)), url.Values{"email": {"jane.doe@example.com"}, "brand_id": {"2"}}, 200, "", "1002"},
			{"brand_id outside the token", token(t, brands(1)), url.Values{"email": {"jane.doe@example.com"}, "brand_id": {"2"}}, 403, "brand_forbidden", ""},
			{"unknown email", token(t, nil), url.Values{"email": {"nobody@example.com"}}, 404, "customer_not_found", ""},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				status, body := api.get(tt.tok, tt.query)
				require.Equal(t, tt.wantStatus, status, body)
				if tt.wantCode != "" {
					errBody := body["error"].(map[string]any)
					assert.Equal(t, tt.wantCode, errBody["code"])
					if tt.wantCode == "customer_ambiguous" {
						assert.Equal(t, map[string]any{"brand_ids": []any{float64(1), float64(2)}}, errBody["details"])
					}
					return
				}
				assert.Equal(t, tt.wantID, body["customer"].(map[string]any)["id"])
			})
		}
	})

	t.Run("authentication", func(t *testing.T) {
		expired := token(t, func(c *auth.Claims) {
			c.IssuedAt = jwt.NewNumericDate(time.Now().Add(-2 * time.Hour))
			c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))
		})
		for tok, want := range map[string]string{
			"":                 "unauthorized",
			expired:            "token_expired",
			"garbage":          "token_malformed",
			token(t, brands()): "token_invalid",
			token(t, func(c *auth.Claims) { c.Scope = "" }): "insufficient_scope",
		} {
			status, body := api.get(tok, url.Values{"email": {"jane.doe@example.com"}})
			assert.Contains(t, []int{401, 403}, status)
			assert.Equal(t, want, body["error"].(map[string]any)["code"])
		}
	})

	t.Run("pagination breaks ties on the id", func(t *testing.T) {
		var (
			seen   []string
			cursor string
			pages  int
		)
		for {
			q := url.Values{"email": {"heavy@example.com"}, "limit": {"4"}}
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			status, body := api.get(token(t, nil), q)
			require.Equal(t, http.StatusOK, status, body)
			pages++
			for _, tx := range body["transactions"].([]any) {
				seen = append(seen, tx.(map[string]any)["id"].(string))
			}
			page := body["page"].(map[string]any)
			if page["has_more"] == false {
				assert.Nil(t, page["next_cursor"])
				break
			}
			cursor = page["next_cursor"].(string)
		}

		assert.Equal(t, 7, pages)
		require.Len(t, seen, 25)
		for i, id := range seen {
			assert.Equal(t, fmt.Sprint(125-i), id, "every transaction exactly once, newest first")
		}

		// A cursor cannot be replayed against a different query.
		status, body := api.get(token(t, nil), url.Values{"email": {"heavy@example.com"}, "limit": {"4"}})
		require.Equal(t, http.StatusOK, status)
		next := body["page"].(map[string]any)["next_cursor"].(string)
		status, body = api.get(token(t, nil), url.Values{"email": {"heavy@example.com"}, "status": {"pending"}, "cursor": {next}})
		assert.Equal(t, http.StatusBadRequest, status)
		assert.Equal(t, "invalid_cursor", body["error"].(map[string]any)["code"])
	})
}
