// Command acmemock is a stand-in for the Acme Support contacts API, used by
// the compose stack and end-to-end tests. It implements the documented
// contract and behaviour:
//
//   - POST /v2/contacts, PATCH /v2/contacts/{id}, GET /v2/contacts?email=
//   - one account per API key; external_id unique per account (409)
//   - 422 for invalid payloads
//   - 60 requests/minute per API key with bursts of 10, answering 429 with
//     Retry-After
//   - optional latency and 5xx fault injection
//
// Debug endpoints (not part of Acme's API) let tests inspect the state:
//
//	GET  /_debug/contacts   all contacts per account
//	GET  /_debug/stats      request counters
//	POST /_debug/reset      forget everything
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/mail"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

func main() {
	var (
		addr      = flag.String("addr", ":8081", "listen address")
		keys      = flag.String("keys", env("ACME_MOCK_KEYS", ""), "comma-separated accepted API keys (empty accepts any)")
		perMinute = flag.Int("rate", 60, "requests per minute per API key (0 disables)")
		burst     = flag.Int("burst", 10, "burst per API key")
		minDelay  = flag.Duration("latency-min", 20*time.Millisecond, "minimum response latency")
		maxDelay  = flag.Duration("latency-max", 200*time.Millisecond, "maximum response latency")
		failRate  = flag.Float64("fail-rate", 0, "probability of answering 503")
	)
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	m := newMock(splitKeys(*keys), *perMinute, *burst, *minDelay, *maxDelay, *failRate, logger)

	srv := &http.Server{Addr: *addr, Handler: m.routes(), ReadHeaderTimeout: 5 * time.Second}
	logger.Info("acme mock listening", "addr", *addr, "rate_per_minute", *perMinute, "burst", *burst, "fail_rate", *failRate)
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("acme mock stopped", "error", err)
		os.Exit(1)
	}
}

type contact struct {
	ID         string         `json:"id"`
	ExternalID string         `json:"external_id"`
	Email      string         `json:"email,omitempty"`
	Name       string         `json:"name,omitempty"`
	Phone      string         `json:"phone,omitempty"`
	Country    string         `json:"country,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
	Created    time.Time      `json:"created"`
	Updated    time.Time      `json:"updated"`
	Updates    int            `json:"updates"`
}

type mock struct {
	acceptedKeys map[string]bool
	perMinute    int
	burst        int
	minDelay     time.Duration
	maxDelay     time.Duration
	failRate     float64
	logger       *slog.Logger

	mu       sync.Mutex
	nextID   int
	accounts map[string]map[string]*contact // api key -> contact id -> contact
	buckets  map[string]*bucket
	stats    map[string]int
}

func newMock(keys []string, perMinute, burst int, minDelay, maxDelay time.Duration, failRate float64, logger *slog.Logger) *mock {
	accepted := make(map[string]bool, len(keys))
	for _, k := range keys {
		accepted[k] = true
	}
	return &mock{
		acceptedKeys: accepted, perMinute: perMinute, burst: burst,
		minDelay: minDelay, maxDelay: maxDelay, failRate: failRate, logger: logger,
		accounts: map[string]map[string]*contact{}, buckets: map[string]*bucket{}, stats: map[string]int{},
	}
}

func (m *mock) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/contacts", m.guard(m.create))
	mux.HandleFunc("PATCH /v2/contacts/{id}", m.guard(m.update))
	mux.HandleFunc("GET /v2/contacts", m.guard(m.lookup))
	mux.HandleFunc("GET /_debug/contacts", m.debugContacts)
	mux.HandleFunc("GET /_debug/stats", m.debugStats)
	mux.HandleFunc("POST /_debug/reset", m.debugReset)
	return mux
}

// guard applies authentication, rate limiting, latency and fault injection.
func (m *mock) guard(next func(w http.ResponseWriter, r *http.Request, key string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			m.count(fmt.Sprintf("%s %d", r.Method, rec.status))
			m.logger.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(), "request_id", r.Header.Get("X-Request-ID"))
		}()

		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || key == "" || (len(m.acceptedKeys) > 0 && !m.acceptedKeys[key]) {
			writeJSON(rec, http.StatusUnauthorized, map[string]string{"error": "invalid api key"})
			return
		}
		if wait, ok := m.allow(key); !ok {
			rec.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			writeJSON(rec, http.StatusTooManyRequests, map[string]string{"error": "rate limited"})
			return
		}

		delay := m.minDelay
		if m.maxDelay > m.minDelay {
			delay += rand.N(m.maxDelay - m.minDelay)
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(delay):
		}
		if m.failRate > 0 && rand.Float64() < m.failRate {
			writeJSON(rec, http.StatusServiceUnavailable, map[string]string{"error": "injected failure"})
			return
		}
		next(rec, r, key)
	}
}

var countryRe = regexp.MustCompile(`^[A-Z]{2}$`)

func validate(c *contact) string {
	switch {
	case c.ExternalID == "":
		return "external_id is required"
	case c.Email != "":
		if a, err := mail.ParseAddress(c.Email); err != nil || a.Address != c.Email {
			return "email is invalid"
		}
	}
	if c.Country != "" && !countryRe.MatchString(c.Country) {
		return "country must be ISO 3166-1 alpha-2"
	}
	if ts, ok := c.Attributes["registered_at"].(string); ok {
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			return "attributes.registered_at must be RFC 3339"
		}
	}
	return ""
}

func decode(r *http.Request) (*contact, string) {
	var c contact
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		return nil, "invalid JSON: " + err.Error()
	}
	if msg := validate(&c); msg != "" {
		return nil, msg
	}
	return &c, ""
}

func (m *mock) create(w http.ResponseWriter, r *http.Request, key string) {
	c, msg := decode(r)
	if msg != "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msg})
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	account := m.account(key)
	for _, existing := range account {
		if existing.ExternalID == c.ExternalID {
			m.stats["duplicate_create_rejected"]++
			writeJSON(w, http.StatusConflict, map[string]string{"error": "contact with this external_id already exists"})
			return
		}
	}
	m.nextID++
	c.ID = fmt.Sprintf("ct_%06d", m.nextID)
	c.Created, c.Updated = time.Now().UTC(), time.Now().UTC()
	account[c.ID] = c
	writeJSON(w, http.StatusCreated, map[string]string{"id": c.ID})
}

func (m *mock) update(w http.ResponseWriter, r *http.Request, key string) {
	c, msg := decode(r)
	if msg != "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": msg})
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	existing, ok := m.account(key)[r.PathValue("id")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "contact not found"})
		return
	}
	existing.ExternalID, existing.Email, existing.Name = c.ExternalID, c.Email, c.Name
	existing.Phone, existing.Country, existing.Attributes = c.Phone, c.Country, c.Attributes
	existing.Updated = time.Now().UTC()
	existing.Updates++
	writeJSON(w, http.StatusOK, map[string]string{"id": existing.ID})
}

func (m *mock) lookup(w http.ResponseWriter, r *http.Request, key string) {
	email := r.URL.Query().Get("email")
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.account(key) {
		if strings.EqualFold(c.Email, email) {
			writeJSON(w, http.StatusOK, map[string]string{"id": c.ID})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "contact not found"})
}

func (m *mock) account(key string) map[string]*contact {
	a, ok := m.accounts[key]
	if !ok {
		a = map[string]*contact{}
		m.accounts[key] = a
	}
	return a
}

func (m *mock) debugContacts(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][]*contact{}
	for key, account := range m.accounts {
		list := make([]*contact, 0, len(account))
		for _, c := range account {
			list = append(list, c)
		}
		slices.SortFunc(list, func(a, b *contact) int { return strings.Compare(a.ID, b.ID) })
		out[key] = list
	}
	writeJSON(w, http.StatusOK, out)
}

func (m *mock) debugStats(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	writeJSON(w, http.StatusOK, m.stats)
}

func (m *mock) debugReset(w http.ResponseWriter, _ *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accounts, m.buckets, m.stats, m.nextID = map[string]map[string]*contact{}, map[string]*bucket{}, map[string]int{}, 0
	w.WriteHeader(http.StatusNoContent)
}

func (m *mock) count(k string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stats[k]++
}

// bucket is a non-blocking token bucket per API key.
type bucket struct {
	tokens float64
	last   time.Time
}

// allow takes a token for key or reports how long until one is available.
func (m *mock) allow(key string) (time.Duration, bool) {
	if m.perMinute <= 0 {
		return 0, true
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	rate := float64(m.perMinute) / 60
	now := time.Now()
	b, ok := m.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(m.burst), last: now}
		m.buckets[key] = b
	}
	b.tokens = min(float64(m.burst), b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return 0, true
	}
	return time.Duration((1 - b.tokens) / rate * float64(time.Second)), false
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func splitKeys(s string) []string {
	var out []string
	for k := range strings.SplitSeq(s, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
