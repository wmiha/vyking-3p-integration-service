package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/apierror"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/health"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/requestid"
)

type fakeReadiness health.Report

func (f fakeReadiness) Check(context.Context) health.Report { return health.Report(f) }

func TestInternalRouter(t *testing.T) {
	ready := fakeReadiness{Status: health.StatusOK, Checks: map[string]string{"database": "ok"}}
	notReady := fakeReadiness{Status: health.StatusUnavailable, Checks: map[string]string{"database": "connection refused"}}

	tests := []struct {
		name       string
		readiness  fakeReadiness
		method     string
		path       string
		wantStatus int
		wantCode   apierror.Code // for error responses
		wantBody   map[string]any
		wantAllow  string
	}{
		{name: "liveness", readiness: notReady, method: http.MethodGet, path: "/health",
			wantStatus: http.StatusOK, wantBody: map[string]any{"status": "ok"}},
		{name: "liveness head", readiness: ready, method: http.MethodHead, path: "/health", wantStatus: http.StatusOK},
		{name: "ready", readiness: ready, method: http.MethodGet, path: "/ready",
			wantStatus: http.StatusOK, wantBody: map[string]any{"status": "ok", "checks": map[string]any{"database": "ok"}}},
		{name: "not ready", readiness: notReady, method: http.MethodGet, path: "/ready",
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   map[string]any{"status": "unavailable", "checks": map[string]any{"database": "connection refused"}}},
		{name: "method not allowed", readiness: ready, method: http.MethodPost, path: "/health",
			wantStatus: http.StatusMethodNotAllowed, wantCode: apierror.CodeMethodNotAllowed, wantAllow: "GET, HEAD"},
		{name: "unknown path", readiness: ready, method: http.MethodGet, path: "/nope",
			wantStatus: http.StatusNotFound, wantCode: apierror.CodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewInternalRouter(InternalDeps{Logger: discardLogger(), Readiness: tt.readiness})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.NotEmpty(t, rec.Header().Get(requestid.Header), "every response carries a request id")
			assert.Equal(t, tt.wantAllow, rec.Header().Get("Allow"))

			switch {
			case tt.wantCode != "":
				body := decodeError(t, rec.Body)
				assert.Equal(t, tt.wantCode, body.Code)
				assert.Equal(t, rec.Header().Get(requestid.Header), body.RequestID)
			case tt.wantBody != nil:
				var got map[string]any
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
				assert.Equal(t, tt.wantBody, got)
			}
		})
	}
}

func TestPublicRouterUnknownRoute(t *testing.T) {
	h := NewPublicRouter(PublicDeps{Logger: discardLogger()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	assert.Equal(t, http.StatusNotFound, rec.Code, "probes are not exposed on the public server")
	assert.Equal(t, apierror.CodeNotFound, decodeError(t, rec.Body).Code)
}
