package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/apierror"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/requestid"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestRequestID(t *testing.T) {
	tests := []struct {
		name     string
		inbound  string
		wantSame bool
	}{
		{name: "generated when missing"},
		{name: "reused when valid", inbound: "acme-req-42", wantSame: true},
		{name: "replaced when unsafe", inbound: "bad id\r\nX-Injected: 1"},
		{name: "replaced when too long", inbound: strings.Repeat("a", 65)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ctxID string
			h := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctxID = requestid.FromContext(r.Context())
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.inbound != "" {
				req.Header.Set(requestid.Header, tt.inbound)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			got := rec.Header().Get(requestid.Header)
			assert.Equal(t, got, ctxID, "header and context must carry the same id")
			if tt.wantSame {
				assert.Equal(t, tt.inbound, got)
				return
			}
			_, err := uuid.Parse(got)
			assert.NoError(t, err, "expected a generated UUID, got %q", got)
		})
	}
}

func TestRecover(t *testing.T) {
	t.Run("panic becomes internal error", func(t *testing.T) {
		h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }),
			RequestID(), Recover(discardLogger()))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		body := decodeError(t, rec.Body)
		assert.Equal(t, apierror.CodeInternal, body.Code)
		assert.Equal(t, rec.Header().Get(requestid.Header), body.RequestID)
		assert.NotContains(t, rec.Body.String(), "boom")
	})

	t.Run("abort handler is re-panicked", func(t *testing.T) {
		h := Recover(discardLogger())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic(http.ErrAbortHandler)
		}))
		assert.PanicsWithValue(t, http.ErrAbortHandler, func() {
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		})
	})
}

func TestAccessLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/things/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("short and stout"))
	})
	mux.HandleFunc("GET /health", func(http.ResponseWriter, *http.Request) {})
	h := AccessLog(logger, "/health")(mux)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/things/7?email=jane@example.com", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/health", nil))

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 2, "successful probe requests must be logged below info level")
	assert.Contains(t, lines[1], `"status":405`, "failed probe requests stay visible")

	var rec map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &rec))
	assert.Equal(t, "GET /v1/things/{id}", rec["route"])
	assert.Equal(t, "/v1/things/7", rec["path"])
	assert.EqualValues(t, http.StatusTeapot, rec["status"])
	assert.EqualValues(t, len("short and stout"), rec["bytes"])
	assert.NotContains(t, lines[0], "jane", "query strings carry PII and must not be logged")
}

func TestChainOrder(t *testing.T) {
	var order []string
	mw := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { order = append(order, "handler") }),
		mw("outer"), mw("inner"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, []string{"outer", "inner", "handler"}, order)
}

type errorBody struct {
	Code      apierror.Code  `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
	Details   map[string]any `json:"details"`
}

func decodeError(t *testing.T, r io.Reader) errorBody {
	t.Helper()
	var env struct {
		Error errorBody `json:"error"`
	}
	require.NoError(t, json.NewDecoder(r).Decode(&env))
	return env.Error
}
