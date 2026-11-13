// Package metrics exposes Prometheus metrics. It is the only package that
// knows about Prometheus: services and adapters report through their own
// small observer interfaces, which *Metrics implements.
//
// Label values are bounded: routes are ServeMux patterns (never raw paths),
// brands are configured ids, outcomes and codes are fixed vocabularies.
package metrics

import (
	"database/sql"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
	"github.com/wmiha/vyking-3p-integration-service/internal/service/contactsync"
)

const namespace = "vyking"

// Metrics holds the service's collectors on a private registry.
type Metrics struct {
	registry *prometheus.Registry

	httpRequests   *prometheus.CounterVec
	httpDuration   *prometheus.HistogramVec
	authFailures   *prometheus.CounterVec
	partnerLookups *prometheus.CounterVec
	cacheRequests  *prometheus.CounterVec

	acmeRequests *prometheus.CounterVec
	acmeDuration *prometheus.HistogramVec
	acmeEvents   *prometheus.CounterVec

	syncCustomers    *prometheus.CounterVec
	syncCycles       *prometheus.CounterVec
	syncDuration     prometheus.Histogram
	syncFetched      prometheus.Gauge
	syncBackingOff   prometheus.Gauge
	syncOrphans      prometheus.Gauge
	syncUnconfigured prometheus.Gauge
	syncOldestAge    *prometheus.GaugeVec
}

// New creates and registers every collector, including the Go runtime and
// process collectors.
func New(version string) *Metrics {
	r := prometheus.NewRegistry()
	f := factory{r}
	m := &Metrics{
		registry: r,

		httpRequests: f.counterVec("http_requests_total", "HTTP requests by server, route pattern and status code.",
			"server", "route", "code"),
		httpDuration: f.histogramVec("http_request_duration_seconds", "HTTP request duration.",
			[]float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}, "server", "route"),
		authFailures: f.counterVec("auth_failures_total", "Rejected partner authentications by error code.", "code"),
		partnerLookups: f.counterVec("partner_lookups_total",
			"Partner transaction lookups by outcome (ok or the error code).", "outcome"),
		cacheRequests: f.counterVec("cache_requests_total", "Partner lookup cache requests by result.", "result"),

		acmeRequests: f.counterVec("acme_requests_total",
			"Acme Support HTTP requests by brand, operation and status (\"error\" when no response).",
			"brand", "op", "status"),
		acmeDuration: f.histogramVec("acme_request_duration_seconds", "Acme Support request duration.",
			[]float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 20, 35}, "brand", "op"),
		acmeEvents: f.counterVec("acme_events_total",
			"Acme Support client events: rate_limited, circuit_opened, key_refused.", "brand", "event"),

		syncCustomers: f.counterVec("sync_customers_total", "Customers processed by the sync, by brand and outcome.",
			"brand", "outcome"),
		syncCycles: f.counterVec("sync_cycles_total", "Sync cycles by leadership and result.", "leader", "result"),
		syncDuration: f.histogram("sync_cycle_duration_seconds", "Duration of sync cycles led by this replica.",
			[]float64{.1, .5, 1, 5, 15, 30, 60, 120, 300}),
		syncFetched:      f.gauge("sync_entries_fetched", "Queue entries fetched by the last cycle led by this replica."),
		syncBackingOff:   f.gauge("sync_customers_backing_off", "Customers waiting for a retry after a customer-specific failure."),
		syncOrphans:      f.gauge("sync_orphan_entries", "Outstanding entries of missing customers seen by the last cycle."),
		syncUnconfigured: f.gauge("sync_unconfigured_entries", "Outstanding entries of brands without Acme credentials seen by the last cycle."),
		syncOldestAge: f.gaugeVec("sync_oldest_outstanding_age_seconds",
			"Age of the oldest outstanding queue entry per brand, as of the last cycle led by this replica.", "brand"),
	}

	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	f.gauge("build_info", "Build information; the value is always 1.", "version", version).Set(1)
	return m
}

// Handler serves the metrics in the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}

// RegisterDB exports connection pool statistics (go_sql_*) for db.
func (m *Metrics) RegisterDB(db *sql.DB, name string) {
	m.registry.MustRegister(collectors.NewDBStatsCollector(db, name))
}

// ObserveRequest records a served HTTP request. Unmatched requests share
// one route label so arbitrary paths cannot inflate cardinality.
func (m *Metrics) ObserveRequest(server, route string, status int, d time.Duration) {
	if route == "" {
		route = "unmatched"
	}
	m.httpRequests.WithLabelValues(server, route, strconv.Itoa(status)).Inc()
	m.httpDuration.WithLabelValues(server, route).Observe(d.Seconds())
}

// AuthFailed records a rejected partner authentication.
func (m *Metrics) AuthFailed(code string) { m.authFailures.WithLabelValues(code).Inc() }

// PartnerLookup records the outcome of a partner transaction lookup.
func (m *Metrics) PartnerLookup(outcome string) { m.partnerLookups.WithLabelValues(outcome).Inc() }

// CacheResult records a cache hit or miss.
func (m *Metrics) CacheResult(hit bool) {
	result := "miss"
	if hit {
		result = "hit"
	}
	m.cacheRequests.WithLabelValues(result).Inc()
}

// RequestDone records an Acme Support HTTP request; status 0 means no
// response was received.
func (m *Metrics) RequestDone(brand domain.BrandID, op string, status int, d time.Duration) {
	code := "error"
	if status > 0 {
		code = strconv.Itoa(status)
	}
	b := brandLabel(brand)
	m.acmeRequests.WithLabelValues(b, op, code).Inc()
	m.acmeDuration.WithLabelValues(b, op).Observe(d.Seconds())
}

// Event records an Acme Support client event.
func (m *Metrics) Event(brand domain.BrandID, event string) {
	m.acmeEvents.WithLabelValues(brandLabel(brand), event).Inc()
}

// CustomerSynced records the outcome of syncing one customer.
func (m *Metrics) CustomerSynced(brand domain.BrandID, outcome string) {
	m.syncCustomers.WithLabelValues(brandLabel(brand), outcome).Inc()
}

// CycleCompleted records a finished sync cycle. Gauges describe the queue as
// seen by the leader; a replica that is not the leader clears them so that
// stale values from an earlier leadership do not linger.
func (m *Metrics) CycleCompleted(s contactsync.RunSummary) {
	result := "ok"
	if s.Error != "" {
		result = "error"
	}
	m.syncCycles.WithLabelValues(strconv.FormatBool(s.Leader), result).Inc()
	m.syncBackingOff.Set(float64(s.BackingOff))

	m.syncOldestAge.Reset()
	if !s.Leader {
		m.syncFetched.Set(0)
		m.syncOrphans.Set(0)
		m.syncUnconfigured.Set(0)
		return
	}
	m.syncDuration.Observe(s.FinishedAt.Sub(s.StartedAt).Seconds())
	m.syncFetched.Set(float64(s.Fetched))
	m.syncOrphans.Set(float64(s.Orphans))
	m.syncUnconfigured.Set(float64(s.Unconfigured))
	for brand, oldest := range s.OldestQueued {
		m.syncOldestAge.WithLabelValues(brandLabel(brand)).Set(s.FinishedAt.Sub(oldest).Seconds())
	}
}

func brandLabel(b domain.BrandID) string { return strconv.FormatUint(uint64(b), 10) }

// factory builds namespaced collectors and registers them.
type factory struct{ r *prometheus.Registry }

func (f factory) counterVec(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: name, Help: help}, labels)
	f.r.MustRegister(c)
	return c
}

func (f factory) histogramVec(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Name: name, Help: help, Buckets: buckets}, labels)
	f.r.MustRegister(h)
	return h
}

func (f factory) histogram(name, help string, buckets []float64) prometheus.Histogram {
	h := prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: namespace, Name: name, Help: help, Buckets: buckets})
	f.r.MustRegister(h)
	return h
}

func (f factory) gauge(name, help string, constLabels ...string) prometheus.Gauge {
	labels := prometheus.Labels{}
	for i := 0; i+1 < len(constLabels); i += 2 {
		labels[constLabels[i]] = constLabels[i+1]
	}
	g := prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help, ConstLabels: labels})
	f.r.MustRegister(g)
	return g
}

func (f factory) gaugeVec(name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help}, labels)
	f.r.MustRegister(g)
	return g
}
