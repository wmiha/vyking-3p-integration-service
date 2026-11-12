package ratelimit

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitN performs n sequential Waits and returns the fake time they took.
func waitN(t *testing.T, l *Limiter, n int) time.Duration {
	t.Helper()
	start := time.Now()
	for range n {
		require.NoError(t, l.Wait(context.Background()))
	}
	return time.Since(start)
}

func TestBurstThenSteadyRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := PerMinute(60, 10) // Acme Support: 60/min, burst 10

		assert.Zero(t, waitN(t, l, 10), "the burst is available immediately")
		assert.Equal(t, time.Second, waitN(t, l, 1), "then one event per second")
		assert.Equal(t, 59*time.Second, waitN(t, l, 59))
	})
}

func TestTokensAccumulateUpToBurst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := PerMinute(60, 10)
		waitN(t, l, 10)

		time.Sleep(time.Hour) // idle far longer than needed to refill
		assert.Zero(t, waitN(t, l, 10), "refilled to the burst")
		assert.Equal(t, time.Second, waitN(t, l, 1), "but never beyond it")
	})
}

func TestPauseUntil(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := PerMinute(60, 10)
		l.PauseUntil(time.Now().Add(30 * time.Second))

		assert.Equal(t, 31*time.Second, waitN(t, l, 1),
			"blocked for the pause, then resumes at the steady rate without a burst")
		assert.Equal(t, time.Second, waitN(t, l, 1))

		l.PauseUntil(time.Now().Add(-time.Minute))
		assert.Equal(t, time.Second, waitN(t, l, 1), "a pause in the past changes nothing")
	})
}

func TestWaitHonoursContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := PerMinute(60, 1)
		l.PauseUntil(time.Now().Add(time.Hour))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		err := l.Wait(ctx)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, 5*time.Second, time.Since(start))
	})
}

func TestConcurrentWaitersShareTheRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := PerMinute(60, 10)
		start := time.Now()

		var wg sync.WaitGroup
		for range 10 {
			wg.Go(func() {
				for range 7 {
					assert.NoError(t, l.Wait(context.Background()))
				}
			})
		}
		wg.Wait()
		// 70 events: 10 from the burst, 60 more at one per second.
		assert.Equal(t, 60*time.Second, time.Since(start))
	})
}
