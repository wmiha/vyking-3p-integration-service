package contactsync

import (
	"slices"
	"sync"
	"time"

	"github.com/wmiha/vyking-3p-integration-service/internal/domain"
)

// maxExcluded bounds the exclusion list sent to the database. Customers
// beyond it are simply retried earlier than their backoff asks for.
const maxExcluded = 1000

// backoff remembers customers whose sync failed for customer-specific reasons
// (rejected data, unresolvable conflict, missing row) so they are retried
// with exponential delays instead of every cycle, without blocking the rest
// of the queue. The state lives in memory: a restart or leader change merely
// retries them sooner. A status column on sync_queue would make it durable
// (see NOTES.md).
type backoff struct {
	initial time.Duration
	max     time.Duration

	mu    sync.Mutex
	state map[domain.CustomerID]backoffState
}

type backoffState struct {
	failures int
	until    time.Time
}

func newBackoff(initial, max time.Duration) *backoff {
	return &backoff{initial: initial, max: max, state: make(map[domain.CustomerID]backoffState)}
}

// fail records a failure and returns when the customer may be retried.
func (b *backoff) fail(id domain.CustomerID, now time.Time) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.state[id]
	s.failures++
	delay := b.max
	if s.failures < 32 {
		delay = min(b.initial<<(s.failures-1), b.max)
	}
	s.until = now.Add(delay)
	b.state[id] = s
	return s.until
}

func (b *backoff) clear(id domain.CustomerID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.state, id)
}

// excluded returns the customers still backing off, latest deadline first,
// and forgets customers whose backoff ended long ago.
func (b *backoff) excluded(now time.Time) []domain.CustomerID {
	b.mu.Lock()
	defer b.mu.Unlock()

	type item struct {
		id    domain.CustomerID
		until time.Time
	}
	var active []item
	for id, s := range b.state {
		switch {
		case now.Before(s.until):
			active = append(active, item{id, s.until})
		case now.Sub(s.until) > b.max:
			delete(b.state, id) // no failure for a long time: start over
		}
	}
	slices.SortFunc(active, func(a, b item) int { return b.until.Compare(a.until) })

	ids := make([]domain.CustomerID, 0, min(len(active), maxExcluded))
	for _, it := range active[:min(len(active), maxExcluded)] {
		ids = append(ids, it.id)
	}
	return ids
}

// size reports how many customers are backing off.
func (b *backoff) size(now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, s := range b.state {
		if now.Before(s.until) {
			n++
		}
	}
	return n
}
