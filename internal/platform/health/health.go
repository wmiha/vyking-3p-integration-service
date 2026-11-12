// Package health aggregates dependency checks for the readiness probe.
package health

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Pinger is implemented by every dependency the service needs to serve
// traffic: database pools, cache backends, ...
type Pinger interface {
	Ping(ctx context.Context) error
}

// PingFunc adapts a function to the Pinger interface.
type PingFunc func(ctx context.Context) error

func (f PingFunc) Ping(ctx context.Context) error { return f(ctx) }

// Readiness states reported by Check.
const (
	StatusOK           = "ok"
	StatusUnavailable  = "unavailable"
	StatusShuttingDown = "shutting_down"
)

// Report is the readiness result, serialized as the /ready response body.
type Report struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// Ready reports whether the instance should receive traffic.
func (r Report) Ready() bool { return r.Status == StatusOK }

// Checker runs all registered checks concurrently with a shared timeout.
type Checker struct {
	timeout      time.Duration
	shuttingDown atomic.Bool

	mu     sync.RWMutex
	checks map[string]Pinger
}

func NewChecker(timeout time.Duration) *Checker {
	return &Checker{timeout: timeout, checks: make(map[string]Pinger)}
}

// Register adds a named dependency check.
func (c *Checker) Register(name string, p Pinger) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checks[name] = p
}

// SetShuttingDown makes every further Check report unavailable so the load
// balancer stops routing new traffic before the servers close.
func (c *Checker) SetShuttingDown() { c.shuttingDown.Store(true) }

// Check pings all dependencies and returns the aggregated report.
func (c *Checker) Check(ctx context.Context) Report {
	if c.shuttingDown.Load() {
		return Report{Status: StatusShuttingDown}
	}

	c.mu.RLock()
	checks := make(map[string]Pinger, len(c.checks))
	for name, p := range c.checks {
		checks[name] = p
	}
	c.mu.RUnlock()

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		report = Report{Status: StatusOK, Checks: make(map[string]string, len(checks))}
	)
	for name, p := range checks {
		wg.Go(func() {
			result := StatusOK
			if err := p.Ping(ctx); err != nil {
				result = err.Error()
			}
			mu.Lock()
			defer mu.Unlock()
			report.Checks[name] = result
			if result != StatusOK {
				report.Status = StatusUnavailable
			}
		})
	}
	wg.Wait()
	return report
}
