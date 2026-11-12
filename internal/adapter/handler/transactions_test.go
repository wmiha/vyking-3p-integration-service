package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/apierror"
	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
	"github.com/wmiha/vyking-3p-integration-service/internal/service/transactions"
)

var testLimits = PageLimits{Default: 20, Max: 100}

func TestParseTransactionsQuery(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		want      transactions.Query
		wantParam string // parameter named in the error details
		wantCode  apierror.Code
	}{
		{name: "defaults", query: "email=jane@example.com",
			want: transactions.Query{Email: "jane@example.com", Limit: 20}},
		{name: "everything", query: "email=+jane@example.com+&status=failed,cancelled&status=failed&limit=5&brand_id=2&cursor=abc",
			want: transactions.Query{Email: "jane@example.com", Limit: 5, Brand: 2, Cursor: "abc",
				Statuses: []domain.Status{domain.StatusFailed, domain.StatusCancelled}}},
		{name: "empty status items ignored", query: "email=j@x.io&status=,pending,",
			want: transactions.Query{Email: "j@x.io", Limit: 20, Statuses: []domain.Status{domain.StatusPending}}},
		{name: "missing email", query: "", wantParam: "email", wantCode: apierror.CodeInvalidParameter},
		{name: "invalid email", query: "email=not-an-email", wantParam: "email", wantCode: apierror.CodeInvalidParameter},
		{name: "display name form", query: "email=" + url.QueryEscape("Jane <jane@example.com>"), wantParam: "email", wantCode: apierror.CodeInvalidParameter},
		{name: "email too long", query: "email=" + strings.Repeat("a", 180) + "@example.com", wantParam: "email", wantCode: apierror.CodeInvalidParameter},
		{name: "unknown status", query: "email=j@x.io&status=settled", wantParam: "status", wantCode: apierror.CodeInvalidParameter},
		{name: "limit zero", query: "email=j@x.io&limit=0", wantParam: "limit", wantCode: apierror.CodeInvalidParameter},
		{name: "limit above max", query: "email=j@x.io&limit=101", wantParam: "limit", wantCode: apierror.CodeInvalidParameter},
		{name: "limit not a number", query: "email=j@x.io&limit=ten", wantParam: "limit", wantCode: apierror.CodeInvalidParameter},
		{name: "brand zero", query: "email=j@x.io&brand_id=0", wantParam: "brand_id", wantCode: apierror.CodeInvalidParameter},
		{name: "brand negative", query: "email=j@x.io&brand_id=-1", wantParam: "brand_id", wantCode: apierror.CodeInvalidParameter},
		{name: "cursor too long", query: "email=j@x.io&cursor=" + strings.Repeat("a", 513), wantCode: apierror.CodeInvalidCursor},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := url.ParseQuery(tt.query)
			require.NoError(t, err)
			got, apiErr := parseTransactionsQuery(v, testLimits)
			if tt.wantCode != "" {
				require.NotNil(t, apiErr)
				assert.Equal(t, tt.wantCode, apiErr.Code)
				assert.Equal(t, http.StatusBadRequest, apiErr.Status)
				if tt.wantParam != "" {
					assert.Equal(t, tt.wantParam, apiErr.Details["parameter"])
				}
				return
			}
			require.Nil(t, apiErr)
			assert.Equal(t, tt.want, got)
		})
	}
}

type fakeFinder struct {
	res   transactions.Result
	err   error
	got   transactions.Query
	whoIs domain.Principal
}

func (f *fakeFinder) Find(_ context.Context, p domain.Principal, q transactions.Query) (transactions.Result, error) {
	f.got, f.whoIs = q, p
	return f.res, f.err
}

func publicRouter(finder TransactionFinder) http.Handler {
	return NewPublicRouter(PublicDeps{
		Logger: discardLogger(),
		Verifier: fakeVerifier{"good": {p: domain.Principal{Subject: "acme-support", Brands: []domain.BrandID{1, 2},
			Scopes: []string{domain.ScopeTransactionsRead}}}},
		Transactions: finder,
		Limits:       testLimits,
	})
}

func get(t *testing.T, h http.Handler, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Request-ID", "req-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func ptr[T any](v T) *T { return &v }

func TestListTransactionsResponse(t *testing.T) {
	created := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	finder := &fakeFinder{res: transactions.Result{
		Customer: domain.Customer{ID: 1001, Brand: 1, BrandName: "Vyking Casino", Currency: "EUR", MinorUnits: ptr[uint8](2), Active: true},
		Transactions: []domain.Transaction{
			{ID: 9007199254740993, Type: "PAYIN", StatusID: 60, AmountMinor: ptr[int64](12345), PaymentMethod: ptr("PAYIN CARD"),
				CreatedAt: created, BookedAt: ptr(created.Add(time.Minute)), ChangedAt: created.Add(time.Hour)},
			{ID: 2, Type: "PAYOUT", StatusID: 71, AmountMinor: ptr[int64](-5), CreatedAt: created, ChangedAt: created.Add(2 * time.Minute)},
			{ID: 3, Type: "", StatusID: 999, CreatedAt: created, ChangedAt: created},
		},
		NextCursor: "next-page",
	}}

	rec := get(t, publicRouter(finder), "/v1/transactions?email=jane@example.com&status=completed&limit=3", "good")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, transactions.Query{Email: "jane@example.com", Limit: 3, Statuses: []domain.Status{domain.StatusCompleted}}, finder.got)
	assert.Equal(t, "acme-support", finder.whoIs.Subject)

	want := `{
	  "request_id": "req-1",
	  "customer": {"id": "1001", "brand_id": 1, "brand": "Vyking Casino", "currency": "EUR", "active": true},
	  "transactions": [
	    {"id": "9007199254740993", "type": "payin", "amount": 123.45, "currency": "EUR", "status": "completed",
	     "status_detail": "settled", "failure_reason": null, "payment_method": "PAYIN CARD",
	     "created_at": "2026-11-01T10:00:00Z", "booked_at": "2026-11-01T10:01:00Z",
	     "settled_at": "2026-11-01T11:00:00Z", "updated_at": "2026-11-01T11:00:00Z"},
	    {"id": "2", "type": "payout", "amount": -0.05, "currency": "EUR", "status": "failed",
	     "status_detail": "declined", "failure_reason": "declined", "payment_method": null,
	     "created_at": "2026-11-01T10:00:00Z", "booked_at": null, "settled_at": null, "updated_at": "2026-11-01T10:02:00Z"},
	    {"id": "3", "type": "unknown", "amount": null, "currency": "EUR", "status": "pending",
	     "status_detail": "unknown", "failure_reason": null, "payment_method": null,
	     "created_at": "2026-11-01T10:00:00Z", "booked_at": null, "settled_at": null, "updated_at": "2026-11-01T10:00:00Z"}
	  ],
	  "page": {"limit": 3, "has_more": true, "next_cursor": "next-page"}
	}`
	assert.JSONEq(t, want, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"amount":123.45`, "amounts are JSON numbers, not strings")
}

func TestListTransactionsAmountsPerCurrency(t *testing.T) {
	tests := []struct {
		currency   string
		minorUnits *uint8
		minor      int64
		want       string
	}{
		{"JPY", ptr[uint8](0), 1000, `1000`},
		{"BHD", ptr[uint8](3), 1234, `1.234`},
		{"USD", nil, 1234, `12.34`}, // NULL in the reference data, ISO 4217 fallback
		{"XTS", nil, 1234, `null`},  // unknown: never guess
	}
	for _, tt := range tests {
		t.Run(tt.currency, func(t *testing.T) {
			finder := &fakeFinder{res: transactions.Result{
				Customer:     domain.Customer{ID: 1, Brand: 1, Currency: tt.currency, MinorUnits: tt.minorUnits},
				Transactions: []domain.Transaction{{ID: 1, Type: "PAYIN", StatusID: 60, AmountMinor: &tt.minor}},
			}}
			rec := get(t, publicRouter(finder), "/v1/transactions?email=j@x.io", "good")
			require.Equal(t, http.StatusOK, rec.Code)

			var body struct {
				Transactions []struct {
					Amount json.RawMessage `json:"amount"`
				} `json:"transactions"`
				Page pageDTO `json:"page"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, tt.want, string(body.Transactions[0].Amount))
			assert.False(t, body.Page.HasMore)
			assert.Nil(t, body.Page.NextCursor)
		})
	}
}

func TestListTransactionsErrors(t *testing.T) {
	tests := []struct {
		name        string
		token       string
		err         error
		wantStatus  int
		wantCode    apierror.Code
		wantDetails map[string]any
	}{
		{name: "no token", wantStatus: http.StatusUnauthorized, wantCode: apierror.CodeUnauthorized},
		{name: "invalid parameter before lookup", token: "good", wantStatus: http.StatusBadRequest, wantCode: apierror.CodeInvalidParameter},
		{name: "not found", token: "good", err: fmt.Errorf("find: %w", domain.ErrCustomerNotFound),
			wantStatus: http.StatusNotFound, wantCode: apierror.CodeCustomerNotFound},
		{name: "ambiguous", token: "good", err: &domain.AmbiguousCustomerError{Brands: []domain.BrandID{1, 2}},
			wantStatus: http.StatusConflict, wantCode: apierror.CodeCustomerAmbiguous,
			wantDetails: map[string]any{"brand_ids": []any{float64(1), float64(2)}}},
		{name: "brand forbidden", token: "good", err: domain.ErrBrandForbidden,
			wantStatus: http.StatusForbidden, wantCode: apierror.CodeBrandForbidden},
		{name: "invalid cursor", token: "good", err: domain.ErrInvalidCursor,
			wantStatus: http.StatusBadRequest, wantCode: apierror.CodeInvalidCursor},
		{name: "database down", token: "good", err: fmt.Errorf("list: %w: %w", domain.ErrUnavailable, errors.New("connection refused")),
			wantStatus: http.StatusServiceUnavailable, wantCode: apierror.CodeServiceUnavailable},
		{name: "bug", token: "good", err: errors.New("sql: Scan error"),
			wantStatus: http.StatusInternalServerError, wantCode: apierror.CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := "/v1/transactions?email=jane@example.com"
			if tt.name == "invalid parameter before lookup" {
				target = "/v1/transactions?email=jane@example.com&limit=1000"
			}
			rec := get(t, publicRouter(&fakeFinder{err: tt.err}), target, tt.token)

			assert.Equal(t, tt.wantStatus, rec.Code)
			body := decodeError(t, rec.Body)
			assert.Equal(t, tt.wantCode, body.Code)
			assert.Equal(t, "req-1", body.RequestID)
			if tt.wantDetails != nil {
				assert.Equal(t, tt.wantDetails, body.Details)
			}
			assert.NotContains(t, rec.Body.String(), "connection refused")
			assert.NotContains(t, rec.Body.String(), "Scan error")
		})
	}
}

func TestPublicRouterMethods(t *testing.T) {
	h := publicRouter(&fakeFinder{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/transactions", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, "GET, HEAD", rec.Header().Get("Allow"))
}
