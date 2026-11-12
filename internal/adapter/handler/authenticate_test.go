package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/apierror"
	"github.com/wmiha/vyking-3p-integration-service/internal/auth"
	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
)

// fakeVerifier maps token strings to outcomes.
type fakeVerifier map[string]struct {
	p   domain.Principal
	err error
}

func (f fakeVerifier) Verify(_ context.Context, token string) (domain.Principal, error) {
	res, ok := f[token]
	if !ok {
		return domain.Principal{}, errors.New("unexpected token")
	}
	return res.p, res.err
}

func TestAuthenticate(t *testing.T) {
	reader := domain.Principal{Subject: "acme", Brands: []domain.BrandID{1}, Scopes: []string{domain.ScopeTransactionsRead}}
	verifier := fakeVerifier{
		"good":      {p: reader},
		"no-scope":  {p: domain.Principal{Subject: "acme", Brands: []domain.BrandID{1}}},
		"expired":   {err: fmt.Errorf("%w: exp", auth.ErrExpired)},
		"malformed": {err: fmt.Errorf("%w: segments", auth.ErrMalformed)},
		"forged":    {err: fmt.Errorf("%w: signature", auth.ErrInvalid)},
	}

	tests := []struct {
		name          string
		header        string
		wantStatus    int
		wantCode      apierror.Code
		wantChallenge string
	}{
		{name: "valid", header: "Bearer good", wantStatus: http.StatusOK},
		{name: "scheme is case-insensitive", header: "bearer good", wantStatus: http.StatusOK},
		{name: "missing header", wantStatus: http.StatusUnauthorized, wantCode: apierror.CodeUnauthorized,
			wantChallenge: `Bearer realm="partner-api"`},
		{name: "basic scheme", header: "Basic dXNlcjpwYXNz", wantStatus: http.StatusUnauthorized, wantCode: apierror.CodeUnauthorized,
			wantChallenge: `Bearer realm="partner-api"`},
		{name: "empty bearer", header: "Bearer ", wantStatus: http.StatusUnauthorized, wantCode: apierror.CodeUnauthorized,
			wantChallenge: `Bearer realm="partner-api"`},
		{name: "expired", header: "Bearer expired", wantStatus: http.StatusUnauthorized, wantCode: apierror.CodeTokenExpired,
			wantChallenge: `Bearer realm="partner-api", error="invalid_token", error_description="token is expired"`},
		{name: "malformed", header: "Bearer malformed", wantStatus: http.StatusUnauthorized, wantCode: apierror.CodeTokenMalformed,
			wantChallenge: `Bearer realm="partner-api", error="invalid_token", error_description="token is malformed"`},
		{name: "forged", header: "Bearer forged", wantStatus: http.StatusUnauthorized, wantCode: apierror.CodeTokenInvalid,
			wantChallenge: `Bearer realm="partner-api", error="invalid_token", error_description="token is invalid"`},
		{name: "missing scope", header: "Bearer no-scope", wantStatus: http.StatusForbidden, wantCode: apierror.CodeInsufficientScope,
			wantChallenge: `Bearer realm="partner-api", error="insufficient_scope", scope="transactions:read"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got domain.Principal
			h := Authenticate(verifier, domain.ScopeTransactionsRead, discardLogger())(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					p, ok := principalFrom(r.Context())
					require.True(t, ok)
					got = p
				}))
			req := httptest.NewRequest(http.MethodGet, "/v1/transactions", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.Equal(t, tt.wantChallenge, rec.Header().Get("WWW-Authenticate"))
			if tt.wantCode != "" {
				assert.Equal(t, tt.wantCode, decodeError(t, rec.Body).Code)
				return
			}
			assert.Equal(t, reader, got)
		})
	}
}
