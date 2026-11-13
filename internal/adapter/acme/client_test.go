package acme

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
	"github.com/wmiha/vyking-3p-integration-service/internal/service/contactsync"
)

// scripted replies with the next response of a script, repeating the last.
type scripted struct {
	mu        sync.Mutex
	responses []response
	requests  []recorded
}

type response struct {
	status int
	body   string
	header map[string]string
	delay  time.Duration
}

type recorded struct {
	method, uri, auth, contentType, requestID string
	body                                      map[string]any
}

func (s *scripted) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)

	s.mu.Lock()
	s.requests = append(s.requests, recorded{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"),
		r.Header.Get("Content-Type"), r.Header.Get("X-Request-ID"), body})
	res := s.responses[min(len(s.requests), len(s.responses))-1]
	s.mu.Unlock()

	if res.delay > 0 {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(res.delay):
		}
	}
	for k, v := range res.header {
		w.Header().Set(k, v)
	}
	w.WriteHeader(res.status)
	_, _ = w.Write([]byte(res.body))
}

func (s *scripted) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func newClient(t *testing.T, responses ...response) (*Client, *scripted) {
	t.Helper()
	srv := &scripted{responses: responses}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return New(Config{
		BaseURL:           ts.URL,
		RequestTimeout:    200 * time.Millisecond,
		MaxAttempts:       3,
		BaseBackoff:       time.Millisecond,
		MaxBackoff:        5 * time.Millisecond,
		DefaultRetryAfter: time.Second,
		RatePerMinute:     60_000,
		Burst:             100,
		BreakerThreshold:  2,
		BreakerCooldown:   300 * time.Millisecond,
		AuthCooldown:      time.Hour,
		APIKeys:           map[domain.BrandID]string{1: "key-brand-1", 2: "key-brand-2"},
	}, nil, slog.New(slog.DiscardHandler)), srv
}

var jane = domain.Contact{
	ExternalID: "1001", Email: "jane+vip@example.com", Name: "Jane Doe", Phone: "+4712345678", Country: "NO",
	Brand: "Vyking Casino", Currency: "EUR", RegisteredAt: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC), Active: true,
}

func TestCreateSendsContractPayload(t *testing.T) {
	c, srv := newClient(t, response{status: http.StatusCreated, body: `{"id":"c-1"}`})

	id, err := c.Create(context.Background(), 1, jane)
	require.NoError(t, err)
	assert.Equal(t, "c-1", id)

	require.Equal(t, 1, srv.count())
	req := srv.requests[0]
	assert.Equal(t, "POST", req.method)
	assert.Equal(t, "/v2/contacts", req.uri)
	assert.Equal(t, "Bearer key-brand-1", req.auth, "each brand uses its own API key")
	assert.Equal(t, "application/json", req.contentType)
	assert.NotEmpty(t, req.requestID)
	assert.Equal(t, map[string]any{
		"external_id": "1001", "email": "jane+vip@example.com", "name": "Jane Doe",
		"phone": "+4712345678", "country": "NO",
		"attributes": map[string]any{"brand": "Vyking Casino", "currency": "EUR",
			"registered_at": "2025-01-02T03:04:05Z", "active": true},
	}, req.body)
}

func TestEmptyFieldsAreOmitted(t *testing.T) {
	c, srv := newClient(t, response{status: http.StatusCreated, body: `{"id":"c-1"}`})
	_, err := c.Create(context.Background(), 1, domain.Contact{ExternalID: "7", Brand: "B", Currency: "EUR"})
	require.NoError(t, err)
	assert.NotContains(t, srv.requests[0].body, "email")
	assert.NotContains(t, srv.requests[0].body, "name")
	assert.Equal(t, false, srv.requests[0].body["attributes"].(map[string]any)["active"])
}

func TestUpdateAndLookupRequests(t *testing.T) {
	c, srv := newClient(t, response{status: http.StatusOK, body: `{"id":"c/1"}`})
	ctx := context.Background()

	require.NoError(t, c.Update(ctx, 2, "c/1", jane))
	id, err := c.FindByEmail(ctx, 2, "jane+vip@example.com")
	require.NoError(t, err)
	assert.Equal(t, "c/1", id)

	assert.Equal(t, "PATCH", srv.requests[0].method)
	assert.Equal(t, "/v2/contacts/c%2F1", srv.requests[0].uri, "ids are path-escaped")
	assert.Equal(t, "Bearer key-brand-2", srv.requests[0].auth)
	assert.Equal(t, "GET", srv.requests[1].method)
	assert.Equal(t, "/v2/contacts?email=jane%2Bvip%40example.com", srv.requests[1].uri, "emails are query-escaped")
}

func TestOutcomes(t *testing.T) {
	tests := []struct {
		name         string
		op           string // create, update or lookup
		responses    []response
		wantID       string
		wantErr      error
		wantRequests int
	}{
		{name: "created", op: "create", responses: []response{{status: 201, body: `{"id":"c-1"}`}}, wantID: "c-1", wantRequests: 1},
		{name: "conflict is not retried", op: "create", responses: []response{{status: 409, body: `{"error":"exists"}`}},
			wantErr: contactsync.ErrContactConflict, wantRequests: 1},
		{name: "invalid payload is not retried", op: "create", responses: []response{{status: 422, body: `{"error":"phone"}`}},
			wantErr: contactsync.ErrContactRejected, wantRequests: 1},
		{name: "other 4xx is a rejection", op: "update", responses: []response{{status: 400}},
			wantErr: contactsync.ErrContactRejected, wantRequests: 1},
		{name: "unknown contact on update", op: "update", responses: []response{{status: 404}},
			wantErr: contactsync.ErrContactNotFound, wantRequests: 1},
		{name: "lookup 404", op: "lookup", responses: []response{{status: 404}},
			wantErr: contactsync.ErrContactNotFound, wantRequests: 1},
		{name: "lookup without id", op: "lookup", responses: []response{{status: 200, body: `{}`}},
			wantErr: contactsync.ErrContactNotFound, wantRequests: 1},
		{name: "5xx retried until success", op: "create",
			responses: []response{{status: 503}, {status: 502}, {status: 201, body: `{"id":"c-2"}`}}, wantID: "c-2", wantRequests: 3},
		{name: "5xx exhausts attempts", op: "create", responses: []response{{status: 500}},
			wantErr: contactsync.ErrPartnerUnavailable, wantRequests: 3},
		{name: "timeout is retried", op: "update",
			responses: []response{{status: 200, delay: time.Second}, {status: 200, body: `{"id":"x"}`}}, wantRequests: 2},
		{name: "success without id is treated as unavailable", op: "create", responses: []response{{status: 201, body: `ok`}},
			wantErr: contactsync.ErrPartnerUnavailable, wantRequests: 3},
		{name: "refused key", op: "create", responses: []response{{status: 401}},
			wantErr: contactsync.ErrPartnerUnauthorized, wantRequests: 1},
		{name: "forbidden key", op: "lookup", responses: []response{{status: 403}},
			wantErr: contactsync.ErrPartnerUnauthorized, wantRequests: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, srv := newClient(t, tt.responses...)
			ctx := context.Background()

			var (
				id  string
				err error
			)
			switch tt.op {
			case "create":
				id, err = c.Create(ctx, 1, jane)
			case "update":
				err = c.Update(ctx, 1, "c-1", jane)
			case "lookup":
				id, err = c.FindByEmail(ctx, 1, jane.Email)
			}
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantID, id)
			}
			assert.Equal(t, tt.wantRequests, srv.count())
		})
	}
}

func TestRetryAfterPausesTheBrand(t *testing.T) {
	c, srv := newClient(t,
		response{status: 429, header: map[string]string{"Retry-After": "1"}},
		response{status: 201, body: `{"id":"c-1"}`},
	)
	start := time.Now()
	id, err := c.Create(context.Background(), 1, jane)
	require.NoError(t, err)
	assert.Equal(t, "c-1", id)
	assert.Equal(t, 2, srv.count())
	assert.GreaterOrEqual(t, time.Since(start), time.Second, "waited for Retry-After")
}

func TestCircuitBreaker(t *testing.T) {
	c, srv := newClient(t, response{status: 503})
	ctx := context.Background()

	// Two failed calls (3 attempts each) reach the threshold.
	for range 2 {
		_, err := c.Create(ctx, 1, jane)
		require.ErrorIs(t, err, contactsync.ErrPartnerUnavailable)
	}
	require.Equal(t, 6, srv.count())

	_, err := c.Create(ctx, 1, jane)
	assert.ErrorIs(t, err, contactsync.ErrPartnerUnavailable)
	assert.ErrorContains(t, err, "circuit open")
	assert.Equal(t, 6, srv.count(), "an open circuit fails fast without calling Acme")

	_, err = c.Create(ctx, 2, jane)
	assert.ErrorIs(t, err, contactsync.ErrPartnerUnavailable)
	assert.Equal(t, 9, srv.count(), "other brands are not affected")

	time.Sleep(350 * time.Millisecond)
	_, _ = c.Create(ctx, 1, jane)
	assert.Equal(t, 12, srv.count(), "calls resume after the cooldown")
}

func TestRefusedKeyPausesBrand(t *testing.T) {
	c, srv := newClient(t, response{status: 401})
	ctx := context.Background()

	_, err := c.Create(ctx, 1, jane)
	require.ErrorIs(t, err, contactsync.ErrPartnerUnauthorized)
	_, err = c.Create(ctx, 1, jane)
	assert.ErrorIs(t, err, contactsync.ErrPartnerUnavailable, "circuit open for the auth cooldown")
	assert.Equal(t, 1, srv.count())
}

func TestUnconfiguredBrand(t *testing.T) {
	c, srv := newClient(t, response{status: 201, body: `{"id":"c-1"}`})
	_, err := c.Create(context.Background(), 3, jane)
	assert.ErrorIs(t, err, contactsync.ErrPartnerUnauthorized)
	assert.ErrorContains(t, err, "no API key configured")
	assert.Equal(t, 0, srv.count())
}

func TestPersistentRateLimitGivesUp(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()
	c := New(Config{BaseURL: ts.URL, RequestTimeout: time.Second, MaxAttempts: 3, RatePerMinute: 60_000, Burst: 100,
		BreakerThreshold: 5, APIKeys: map[domain.BrandID]string{1: "k"}}, nil, slog.New(slog.DiscardHandler))

	_, err := c.Create(context.Background(), 1, jane)
	assert.ErrorIs(t, err, contactsync.ErrPartnerUnavailable)
	assert.Equal(t, int32(maxThrottledRetries+1), hits.Load())
}

func TestRetryAfterParsing(t *testing.T) {
	def := 60 * time.Second
	tests := []struct {
		header string
		want   time.Duration
		approx bool
	}{
		{"", def, false},
		{"5", 5 * time.Second, false},
		{" 7 ", 7 * time.Second, false},
		{"0", 0, false},
		{"-1", def, false},
		{"soon", def, false},
		{time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat), 30 * time.Second, true},
		{time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.header, func(t *testing.T) {
			got := retryAfter(tt.header, def)
			if tt.approx {
				assert.InDelta(t, tt.want.Seconds(), got.Seconds(), 1.5)
				return
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

type recordingObserver struct {
	mu       sync.Mutex
	requests []string
	events   []string
}

func (o *recordingObserver) RequestDone(brand domain.BrandID, op string, status int, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.requests = append(o.requests, fmt.Sprintf("%d %s %d", brand, op, status))
}

func (o *recordingObserver) Event(brand domain.BrandID, event string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, fmt.Sprintf("%d %s", brand, event))
}

func TestObserver(t *testing.T) {
	srv := &scripted{responses: []response{
		{status: 429, header: map[string]string{"Retry-After": "0"}},
		{status: 503},
		{status: 201, body: `{"id":"c-1"}`},
		{status: 401},
	}}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	obs := &recordingObserver{}
	c := New(Config{BaseURL: ts.URL, RequestTimeout: time.Second, MaxAttempts: 3, BaseBackoff: time.Millisecond,
		MaxBackoff: time.Millisecond, RatePerMinute: 60_000, Burst: 100, BreakerThreshold: 5, AuthCooldown: time.Hour,
		APIKeys: map[domain.BrandID]string{1: "k"}, Observer: obs}, nil, slog.New(slog.DiscardHandler))

	_, err := c.Create(context.Background(), 1, jane)
	require.NoError(t, err)
	_, err = c.Create(context.Background(), 1, jane)
	require.ErrorIs(t, err, contactsync.ErrPartnerUnauthorized)

	assert.Equal(t, []string{"1 create 429", "1 create 503", "1 create 201", "1 create 401"}, obs.requests)
	assert.Equal(t, []string{"1 rate_limited", "1 key_refused"}, obs.events)
}
