// Package ratelimit implements a token bucket that can also be paused, which
// is what an API client needs to respect both a published rate limit and a
// server's Retry-After answers.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Limiter allows `rate` events per second on average with bursts of up to
// `burst` events. It is safe for concurrent use.
type Limiter struct {
	mu          sync.Mutex
	rate        float64 // tokens per second
	burst       float64
	tokens      float64
	last        time.Time // last refill
	pausedUntil time.Time
}

// PerMinute returns a limiter for n events per minute with the given burst.
// It starts full, like a fresh server-side bucket.
func PerMinute(n, burst int) *Limiter {
	return &Limiter{
		rate:   float64(n) / 60,
		burst:  float64(burst),
		tokens: float64(burst),
		last:   time.Now(),
	}
}

// Wait blocks until an event may happen or ctx ends.
func (l *Limiter) Wait(ctx context.Context) error {
	for {
		wait := l.reserve()
		if wait == 0 {
			return nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// reserve takes a token and returns 0, or returns how long to wait before
// trying again.
func (l *Limiter) reserve() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	if now.Before(l.pausedUntil) {
		return l.pausedUntil.Sub(now)
	}
	if elapsed := now.Sub(l.last); elapsed > 0 {
		l.tokens = min(l.burst, l.tokens+elapsed.Seconds()*l.rate)
		l.last = now
	}
	if l.tokens >= 1 {
		l.tokens--
		return 0
	}
	return time.Duration((1 - l.tokens) / l.rate * float64(time.Second))
}

// PauseUntil blocks every caller until t, e.g. after a 429 with Retry-After.
// The bucket is emptied: once the pause ends, events resume at the steady
// rate instead of bursting into a server that just rejected us.
func (l *Limiter) PauseUntil(t time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t.After(l.pausedUntil) {
		l.pausedUntil = t
		l.tokens = 0
		l.last = t
	}
}
