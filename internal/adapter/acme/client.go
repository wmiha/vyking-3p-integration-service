// Package acme is the outbound adapter for the Acme Support contacts API:
//
//	POST  /v2/contacts            create   -> 200/201 {"id": "..."}
//	PATCH /v2/contacts/{id}       update   -> 200     {"id": "..."}
//	GET   /v2/contacts?email=...  look up  -> 200     {"id": "..."}
//
// Every brand has its own API key, and Acme Support limits each key to 60
// requests per minute (burst 10). The client therefore keeps a token bucket
// and a circuit breaker per brand, honours Retry-After on 429 and retries
// timeouts and 5xx with jittered exponential backoff.
package acme

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/ratelimit"
	"github.com/wmiha/vyking-3p-integration-service/internal/platform/requestid"
	"github.com/wmiha/vyking-3p-integration-service/internal/service/contactsync"
)

// Config configures the client. Durations and limits come from config.
type Config struct {
	BaseURL string
	// RequestTimeout bounds one HTTP attempt; Acme may take up to 30s.
	RequestTimeout time.Duration
	// MaxAttempts per call for timeouts, network errors and 5xx.
	MaxAttempts int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// DefaultRetryAfter applies to a 429 without a usable Retry-After.
	DefaultRetryAfter time.Duration

	RatePerMinute int
	Burst         int

	// BreakerThreshold consecutive failed calls open a brand's circuit for
	// BreakerCooldown; a refused API key opens it for AuthCooldown.
	BreakerThreshold int
	BreakerCooldown  time.Duration
	AuthCooldown     time.Duration

	// APIKeys holds each brand's API key. Brands without a key cannot sync.
	APIKeys map[domain.BrandID]string
}

// maxThrottledRetries bounds how often one call waits for Retry-After.
const maxThrottledRetries = 10

// Client talks to Acme Support on behalf of every configured brand.
type Client struct {
	cfg    Config
	http   *http.Client
	logger *slog.Logger
	brands map[domain.BrandID]*brandState
}

type brandState struct {
	key     string
	limiter *ratelimit.Limiter

	mu        sync.Mutex
	failures  int
	openUntil time.Time
}

// New returns a client. httpClient may be nil for a default one; its own
// timeout is not used, every attempt is bounded by cfg.RequestTimeout.
func New(cfg Config, httpClient *http.Client, logger *slog.Logger) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConnsPerHost: 32,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		}}
	}
	brands := make(map[domain.BrandID]*brandState, len(cfg.APIKeys))
	for id, key := range cfg.APIKeys {
		brands[id] = &brandState{key: key, limiter: ratelimit.PerMinute(cfg.RatePerMinute, cfg.Burst)}
	}
	return &Client{cfg: cfg, http: httpClient, logger: logger, brands: brands}
}

// contactJSON is the wire format of a contact.
type contactJSON struct {
	ExternalID string     `json:"external_id"`
	Email      string     `json:"email,omitempty"`
	Name       string     `json:"name,omitempty"`
	Phone      string     `json:"phone,omitempty"`
	Country    string     `json:"country,omitempty"`
	Attributes attributes `json:"attributes"`
}

type attributes struct {
	Brand        string `json:"brand"`
	Currency     string `json:"currency"`
	RegisteredAt string `json:"registered_at"`
	Active       bool   `json:"active"`
}

func toJSON(c domain.Contact) contactJSON {
	return contactJSON{
		ExternalID: c.ExternalID,
		Email:      c.Email,
		Name:       c.Name,
		Phone:      c.Phone,
		Country:    c.Country,
		Attributes: attributes{
			Brand:        c.Brand,
			Currency:     c.Currency,
			RegisteredAt: c.RegisteredAt.UTC().Format(time.RFC3339),
			Active:       c.Active,
		},
	}
}

type idResponse struct {
	ID string `json:"id"`
}

// Create creates a contact and returns its id. A contact with the same
// external_id yields contactsync.ErrContactConflict.
func (c *Client) Create(ctx context.Context, brand domain.BrandID, contact domain.Contact) (string, error) {
	return c.call(ctx, brand, "create", http.MethodPost, "/v2/contacts", toJSON(contact))
}

// Update replaces the contact's data. An unknown id yields
// contactsync.ErrContactNotFound.
func (c *Client) Update(ctx context.Context, brand domain.BrandID, contactID string, contact domain.Contact) error {
	_, err := c.call(ctx, brand, "update", http.MethodPatch, "/v2/contacts/"+url.PathEscape(contactID), toJSON(contact))
	return err
}

// FindByEmail returns the id of the contact with email, or
// contactsync.ErrContactNotFound. The contract does not document the
// "not found" answer; we accept 404 as well as 200 without an id.
func (c *Client) FindByEmail(ctx context.Context, brand domain.BrandID, email string) (string, error) {
	return c.call(ctx, brand, "lookup", http.MethodGet, "/v2/contacts?email="+url.QueryEscape(email), nil)
}

// call performs one logical API call with rate limiting, retries and the
// circuit breaker, returning the "id" of a successful response.
func (c *Client) call(ctx context.Context, brand domain.BrandID, op, method, path string, body any) (string, error) {
	state, ok := c.brands[brand]
	if !ok {
		return "", fmt.Errorf("%s: brand %d has no API key configured: %w", op, brand, contactsync.ErrPartnerUnauthorized)
	}
	if until, open := state.open(); open {
		return "", fmt.Errorf("%s: circuit open for brand %d until %s: %w", op, brand, until.Format(time.RFC3339), contactsync.ErrPartnerUnavailable)
	}

	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return "", fmt.Errorf("%s: encode body: %w", op, err)
		}
	}

	var lastErr error
	throttled := 0
	for attempt := 1; ; {
		if err := state.limiter.Wait(ctx); err != nil {
			return "", fmt.Errorf("%s: %w: %w", op, contactsync.ErrPartnerUnavailable, err)
		}

		res := c.attempt(ctx, state, method, path, payload)
		c.logger.DebugContext(ctx, "acme request", "brand_id", brand, "op", op, "status", res.status,
			"attempt", attempt, "duration_ms", res.duration.Milliseconds(), "acme_request_id", res.requestID)

		switch {
		case res.err == nil:
			state.succeeded()
			return res.id, nil

		case res.status == http.StatusTooManyRequests:
			if throttled++; throttled > maxThrottledRetries {
				return "", fmt.Errorf("%s: still rate limited after %d waits: %w", op, maxThrottledRetries, contactsync.ErrPartnerUnavailable)
			}
			// Not counted as an attempt: the server asked us to wait, which
			// the limiter now enforces for every worker of this brand.
			state.limiter.PauseUntil(time.Now().Add(res.retryAfter))
			c.logger.WarnContext(ctx, "acme rate limit hit, pausing brand",
				"brand_id", brand, "op", op, "retry_after", res.retryAfter.String())
			continue

		case errors.Is(res.err, contactsync.ErrPartnerUnauthorized):
			state.trip(c.cfg.AuthCooldown)
			c.logger.ErrorContext(ctx, "acme refused the brand's API key, pausing brand",
				"brand_id", brand, "op", op, "status", res.status, "cooldown", c.cfg.AuthCooldown.String())
			return "", fmt.Errorf("%s: %w", op, res.err)

		case !errors.Is(res.err, contactsync.ErrPartnerUnavailable):
			// Conflict, not found, rejected: a definitive answer about this
			// contact, not a sign of an unhealthy partner.
			state.succeeded()
			return "", fmt.Errorf("%s: %w", op, res.err)
		}

		lastErr = res.err
		if attempt >= c.cfg.MaxAttempts || ctx.Err() != nil {
			break
		}
		backoff := c.backoff(attempt)
		c.logger.WarnContext(ctx, "acme call failed, retrying", "brand_id", brand, "op", op,
			"attempt", attempt, "backoff", backoff.String(), "error", res.err.Error())
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		attempt++
	}

	if state.failed(c.cfg.BreakerThreshold, c.cfg.BreakerCooldown) {
		c.logger.ErrorContext(ctx, "acme unavailable, circuit opened", "brand_id", brand,
			"cooldown", c.cfg.BreakerCooldown.String())
	}
	return "", fmt.Errorf("%s: %w", op, lastErr)
}

type attemptResult struct {
	id         string
	status     int
	retryAfter time.Duration
	requestID  string
	duration   time.Duration
	err        error
}

func (c *Client) attempt(ctx context.Context, state *brandState, method, path string, payload []byte) attemptResult {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer cancel()

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.cfg.BaseURL, "/")+path, body)
	if err != nil {
		return attemptResult{err: fmt.Errorf("build request: %w", err)}
	}
	reqID := requestid.New()
	req.Header.Set("Authorization", "Bearer "+state.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set(requestid.Header, reqID)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	start := time.Now()
	resp, err := c.http.Do(req)
	res := attemptResult{requestID: reqID, duration: time.Since(start)}
	if err != nil {
		res.err = fmt.Errorf("%w: %w", contactsync.ErrPartnerUnavailable, err)
		return res
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	res.status = resp.StatusCode
	res.duration = time.Since(start)
	if err != nil {
		res.err = fmt.Errorf("%w: read response: %w", contactsync.ErrPartnerUnavailable, err)
		return res
	}

	switch code := resp.StatusCode; {
	case code == http.StatusOK || code == http.StatusCreated:
		var r idResponse
		if err := json.Unmarshal(raw, &r); err != nil || r.ID == "" {
			if method == http.MethodGet {
				res.err = fmt.Errorf("%w: lookup returned no id", contactsync.ErrContactNotFound)
				return res
			}
			// The call may have taken effect; a retry resolves it through
			// the conflict/lookup path.
			res.err = fmt.Errorf("%w: response without contact id (status %d)", contactsync.ErrPartnerUnavailable, code)
			return res
		}
		res.id = r.ID
	case code == http.StatusTooManyRequests:
		res.retryAfter = retryAfter(resp.Header.Get("Retry-After"), c.cfg.DefaultRetryAfter)
		res.err = errors.New("rate limited")
	case code == http.StatusConflict:
		res.err = fmt.Errorf("%w: %s", contactsync.ErrContactConflict, snippet(raw))
	case code == http.StatusNotFound:
		res.err = fmt.Errorf("%w: %s", contactsync.ErrContactNotFound, snippet(raw))
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		res.err = fmt.Errorf("%w: status %d", contactsync.ErrPartnerUnauthorized, code)
	case code >= 500 || code == http.StatusRequestTimeout:
		res.err = fmt.Errorf("%w: status %d: %s", contactsync.ErrPartnerUnavailable, code, snippet(raw))
	default: // 422 and any other 4xx: the request itself is wrong
		res.err = fmt.Errorf("%w: status %d: %s", contactsync.ErrContactRejected, code, snippet(raw))
	}
	return res
}

// backoff returns a full-jitter exponential delay for the given attempt.
func (c *Client) backoff(attempt int) time.Duration {
	d := min(c.cfg.BaseBackoff<<(attempt-1), c.cfg.MaxBackoff)
	return d/2 + rand.N(d/2+1)
}

// retryAfter parses a Retry-After header given in seconds or as an HTTP date.
func retryAfter(v string, def time.Duration) time.Duration {
	if v == "" {
		return def
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0)
	}
	return def
}

// snippet returns a short, single-line excerpt of an error body for logs.
func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// open reports whether the brand's circuit is open.
func (s *brandState) open() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openUntil, time.Now().Before(s.openUntil)
}

func (s *brandState) succeeded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = 0
}

// failed records a failed call and opens the circuit after threshold
// consecutive failures. It reports whether the circuit was opened.
func (s *brandState) failed(threshold int, cooldown time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures++
	if s.failures < threshold {
		return false
	}
	s.failures = 0
	s.openUntil = time.Now().Add(cooldown)
	return true
}

// trip opens the circuit immediately.
func (s *brandState) trip(cooldown time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = 0
	s.openUntil = time.Now().Add(cooldown)
}
