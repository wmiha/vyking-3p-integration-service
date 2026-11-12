package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/wmiha/vyking-3p-integration-service/internal/apierror"
)

func TestBasicAuth(t *testing.T) {
	tests := []struct {
		name       string
		setAuth    bool
		user, pass string
		wantStatus int
	}{
		{name: "valid credentials", setAuth: true, user: "ops", pass: "s3cret", wantStatus: http.StatusOK},
		{name: "missing header", wantStatus: http.StatusUnauthorized},
		{name: "wrong password", setAuth: true, user: "ops", pass: "guess", wantStatus: http.StatusUnauthorized},
		{name: "wrong user", setAuth: true, user: "admin", pass: "s3cret", wantStatus: http.StatusUnauthorized},
		{name: "password prefix", setAuth: true, user: "ops", pass: "s3cre", wantStatus: http.StatusUnauthorized},
		{name: "empty credentials", setAuth: true, wantStatus: http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := BasicAuth("pprof", "ops", "s3cret")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
			if tt.setAuth {
				req.SetBasicAuth(tt.user, tt.pass)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			assert.Equal(t, tt.wantStatus, rec.Code)
			if tt.wantStatus == http.StatusUnauthorized {
				assert.Equal(t, `Basic realm="pprof", charset="UTF-8"`, rec.Header().Get("WWW-Authenticate"))
				assert.Equal(t, apierror.CodeUnauthorized, decodeError(t, rec.Body).Code)
			}
		})
	}
}
