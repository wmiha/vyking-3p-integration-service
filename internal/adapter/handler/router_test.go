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

func TestProfilerRoutes(t *testing.T) {
	creds := &Credentials{Username: "ops", Password: "pprof-secret-1"}

	tests := []struct {
		name       string
		profiler   *Credentials
		method     string
		path       string
		user, pass string
		wantStatus int
		wantCode   apierror.Code
	}{
		{name: "disabled", method: http.MethodGet, path: "/debug/pprof/", user: "ops", pass: "pprof-secret-1",
			wantStatus: http.StatusNotFound, wantCode: apierror.CodeNotFound},
		{name: "no credentials", profiler: creds, method: http.MethodGet, path: "/debug/pprof/",
			wantStatus: http.StatusUnauthorized, wantCode: apierror.CodeUnauthorized},
		{name: "wrong credentials", profiler: creds, method: http.MethodGet, path: "/debug/pprof/heap", user: "ops", pass: "nope",
			wantStatus: http.StatusUnauthorized, wantCode: apierror.CodeUnauthorized},
		{name: "index", profiler: creds, method: http.MethodGet, path: "/debug/pprof/", user: "ops", pass: "pprof-secret-1",
			wantStatus: http.StatusOK},
		{name: "named profile", profiler: creds, method: http.MethodGet, path: "/debug/pprof/goroutine?debug=1", user: "ops", pass: "pprof-secret-1",
			wantStatus: http.StatusOK},
		{name: "cmdline", profiler: creds, method: http.MethodGet, path: "/debug/pprof/cmdline", user: "ops", pass: "pprof-secret-1",
			wantStatus: http.StatusOK},
		{name: "symbol post", profiler: creds, method: http.MethodPost, path: "/debug/pprof/symbol", user: "ops", pass: "pprof-secret-1",
			wantStatus: http.StatusOK},
		{name: "unsupported method", profiler: creds, method: http.MethodDelete, path: "/debug/pprof/heap", user: "ops", pass: "pprof-secret-1",
			wantStatus: http.StatusMethodNotAllowed, wantCode: apierror.CodeMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewInternalRouter(InternalDeps{Logger: discardLogger(), Readiness: fakeReadiness{}, Profiler: tt.profiler})
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.user != "" {
				req.SetBasicAuth(tt.user, tt.pass)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			assert.Equal(t, tt.wantStatus, rec.Code)
			if tt.wantCode != "" {
				assert.Equal(t, tt.wantCode, decodeError(t, rec.Body).Code)
			} else {
				assert.NotEmpty(t, rec.Body.Len())
			}
		})
	}
}

// TestProfilerPrefixRequiresAuthBeforeRouting ensures unauthenticated callers
// cannot discover profiler routes through 404/405 answers or redirects.
func TestProfilerPrefixRequiresAuthBeforeRouting(t *testing.T) {
	h := NewInternalRouter(InternalDeps{
		Logger: discardLogger(), Readiness: fakeReadiness{},
		Profiler: &Credentials{Username: "ops", Password: "pprof-secret-1"},
	})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/debug/pprof"},           // would redirect
		{http.MethodDelete, "/debug/pprof/heap"},   // would be 405
		{http.MethodGet, "/debug/pprof/nope/deep"}, // would be 404
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Empty(t, rec.Header().Get("Allow"))
			assert.Empty(t, rec.Header().Get("Location"))
		})
	}
}
