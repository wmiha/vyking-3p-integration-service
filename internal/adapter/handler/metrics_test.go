package handler

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
	"github.com/wmiha/vyking-3p-integration-service/internal/service/transactions"
)

type recordingObserver struct {
	mu       sync.Mutex
	requests []string
	auth     []string
	lookups  []string
}

func (o *recordingObserver) ObserveRequest(server, route string, status int, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.requests = append(o.requests, fmt.Sprintf("%s|%s|%d", server, route, status))
}

func (o *recordingObserver) AuthFailed(code string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.auth = append(o.auth, code)
}

func (o *recordingObserver) PartnerLookup(outcome string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.lookups = append(o.lookups, outcome)
}

func TestPublicRouterReportsMetrics(t *testing.T) {
	obs := &recordingObserver{}
	finder := &fakeFinder{res: transactions.Result{Customer: domain.Customer{ID: 1, Brand: 1, Currency: "EUR"}}}
	h := NewPublicRouter(PublicDeps{
		Logger: discardLogger(),
		Verifier: fakeVerifier{"good": {p: domain.Principal{Subject: "acme", Brands: []domain.BrandID{1},
			Scopes: []string{domain.ScopeTransactionsRead}}}},
		Transactions: finder, Limits: testLimits, Requests: obs, Partner: obs,
	})

	get(t, h, "/v1/transactions?email=j@x.io", "good")
	get(t, h, "/v1/transactions?email=j@x.io&limit=0", "good")
	get(t, h, "/v1/transactions?email=j@x.io", "")
	finder.err = domain.ErrCustomerNotFound
	get(t, h, "/v1/transactions?email=j@x.io", "good")
	get(t, h, "/v1/customers/42/secrets", "good")

	assert.Equal(t, []string{
		"public|GET /v1/transactions|200",
		"public|GET /v1/transactions|400",
		"public|GET /v1/transactions|401",
		"public|GET /v1/transactions|404",
		"public||404", // unmatched: never the raw path
	}, obs.requests)
	assert.Equal(t, []string{"unauthorized"}, obs.auth)
	assert.Equal(t, []string{"ok", "invalid_parameter", "customer_not_found"}, obs.lookups)
}

func TestInternalRouterServesMetrics(t *testing.T) {
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("vyking_up 1\n")) })
	h := NewInternalRouter(InternalDeps{Logger: discardLogger(), Readiness: fakeReadiness{}, Metrics: metrics})
	rec := get(t, h, "/metrics", "")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "vyking_up 1\n", rec.Body.String())

	rec = get(t, NewPublicRouter(PublicDeps{Logger: discardLogger()}), "/metrics", "")
	assert.Equal(t, http.StatusNotFound, rec.Code, "metrics are internal only")
}
