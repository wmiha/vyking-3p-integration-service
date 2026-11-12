// Package cache provides cache backends for the use cases' cache ports.
//
// Memory is an in-process cache: fast and dependency-free, but per replica
// (each pod has its own copy, so hit rates drop as replicas grow). A shared
// backend such as Redis would be another type in this package with the same
// Get/Set/Ping methods; services and wiring would not change.
package cache

import (
	"context"
	"sync"
	"time"
)

// Memory is a bounded TTL cache, safe for concurrent use.
type Memory[V any] struct {
	ttl        time.Duration
	maxEntries int

	mu    sync.Mutex
	items map[string]entry[V]

	stop chan struct{}
	done chan struct{}
}

type entry[V any] struct {
	value   V
	expires time.Time
}

// NewMemory returns a cache keeping values for ttl and at most maxEntries
// values. A background sweep removes expired entries every sweepInterval;
// Close stops it.
func NewMemory[V any](ttl time.Duration, maxEntries int, sweepInterval time.Duration) *Memory[V] {
	c := &Memory[V]{
		ttl:        ttl,
		maxEntries: maxEntries,
		items:      make(map[string]entry[V]),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	go c.sweep(sweepInterval)
	return c
}

// Get returns the value for key if present and not expired.
func (c *Memory[V]) Get(_ context.Context, key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.items[key]
	if !ok || !time.Now().Before(e.expires) {
		var zero V
		return zero, false
	}
	return e.value, true
}

// Set stores value under key. When the cache is full an arbitrary entry is
// evicted: O(1), and good enough for a short-TTL cache whose working set is
// small compared to its capacity.
func (c *Memory[V]) Set(_ context.Context, key string, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.items[key]; !exists && len(c.items) >= c.maxEntries {
		for k := range c.items {
			delete(c.items, k)
			break
		}
	}
	c.items[key] = entry[V]{value: value, expires: time.Now().Add(c.ttl)}
}

// Ping always succeeds; it exists so readiness checks treat every cache
// backend alike (a network cache would really ping its server).
func (c *Memory[V]) Ping(context.Context) error { return nil }

// Len reports the number of stored entries, including expired ones not yet
// swept.
func (c *Memory[V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// Close stops the background sweep.
func (c *Memory[V]) Close() {
	close(c.stop)
	<-c.done
}

func (c *Memory[V]) sweep(interval time.Duration) {
	defer close(c.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			now := time.Now()
			c.mu.Lock()
			for k, e := range c.items {
				if !now.Before(e.expires) {
					delete(c.items, k)
				}
			}
			c.mu.Unlock()
		}
	}
}
