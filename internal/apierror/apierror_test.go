package apierror

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/platform/requestid"
)

type response struct {
	Error struct {
		Code      Code           `json:"code"`
		Message   string         `json:"message"`
		RequestID string         `json:"request_id"`
		Details   map[string]any `json:"details"`
	} `json:"error"`
}

func TestWrite(t *testing.T) {
	dbErr := errors.New("dial tcp 10.0.0.7:3306: connection refused")

	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    Code
		wantMessage string
		wantDetails map[string]any
	}{
		{
			name:        "api error",
			err:         New(http.StatusNotFound, CodeCustomerNotFound, "customer not found"),
			wantStatus:  http.StatusNotFound,
			wantCode:    CodeCustomerNotFound,
			wantMessage: "customer not found",
		},
		{
			name:        "invalid parameter carries details",
			err:         InvalidParameter("limit", "limit must be between 1 and 100"),
			wantStatus:  http.StatusBadRequest,
			wantCode:    CodeInvalidParameter,
			wantMessage: "limit must be between 1 and 100",
			wantDetails: map[string]any{"parameter": "limit"},
		},
		{
			name:        "wrapped api error is unwrapped",
			err:         fmt.Errorf("lookup: %w", New(http.StatusConflict, CodeCustomerAmbiguous, "ambiguous")),
			wantStatus:  http.StatusConflict,
			wantCode:    CodeCustomerAmbiguous,
			wantMessage: "ambiguous",
		},
		{
			name:        "plain error becomes internal without leaking",
			err:         dbErr,
			wantStatus:  http.StatusInternalServerError,
			wantCode:    CodeInternal,
			wantMessage: "internal server error",
		},
		{
			name:        "unavailable hides cause",
			err:         Unavailable(dbErr),
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    CodeServiceUnavailable,
			wantMessage: "service temporarily unavailable, retry later",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/transactions", nil)
			req = req.WithContext(requestid.NewContext(req.Context(), "req-123"))
			rec := httptest.NewRecorder()

			Write(rec, req, tt.err)

			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
			assert.NotContains(t, rec.Body.String(), "10.0.0.7", "internal cause must never be serialized")

			var got response
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			assert.Equal(t, tt.wantCode, got.Error.Code)
			assert.Equal(t, tt.wantMessage, got.Error.Message)
			assert.Equal(t, "req-123", got.Error.RequestID)
			assert.Equal(t, tt.wantDetails, got.Error.Details)
		})
	}
}

func TestErrorCopiesDoNotMutateOriginal(t *testing.T) {
	base := New(http.StatusBadRequest, CodeInvalidParameter, "bad")
	withDetails := base.WithDetails(map[string]any{"parameter": "email"})
	withMore := withDetails.WithDetails(map[string]any{"max": 190})
	cause := errors.New("boom")
	withCause := base.WithCause(cause)

	assert.Nil(t, base.Details)
	assert.Nil(t, base.Err)
	assert.Equal(t, map[string]any{"parameter": "email"}, withDetails.Details)
	assert.Equal(t, map[string]any{"parameter": "email", "max": 190}, withMore.Details)
	assert.ErrorIs(t, withCause, cause)
	assert.Equal(t, "invalid_parameter (400): bad: boom", withCause.Error())
}
