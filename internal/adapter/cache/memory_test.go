package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestMemoryExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := NewMemory[string](10*time.Second, 10, time.Minute)
		defer c.Close()

		c.Set(ctx, "k", "v")
		got, ok := c.Get(ctx, "k")
		assert.True(t, ok)
		assert.Equal(t, "v", got)

		time.Sleep(9 * time.Second)
		_, ok = c.Get(ctx, "k")
		assert.True(t, ok, "still fresh just before the ttl")

		time.Sleep(time.Second)
		_, ok = c.Get(ctx, "k")
		assert.False(t, ok, "expired exactly at the ttl")

		c.Set(ctx, "k", "v2")
		got, ok = c.Get(ctx, "k")
		assert.True(t, ok, "overwriting renews the ttl")
		assert.Equal(t, "v2", got)
	})
}

func TestMemorySweepsExpiredEntries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := NewMemory[int](time.Second, 100, 5*time.Second)
		defer c.Close()

		for i := range 10 {
			c.Set(ctx, fmt.Sprint(i), i)
		}
		assert.Equal(t, 10, c.Len())

		time.Sleep(5 * time.Second)
		synctest.Wait()
		assert.Equal(t, 0, c.Len(), "the background sweep frees expired entries")
	})
}

func TestMemoryIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		c := NewMemory[int](time.Minute, 3, time.Minute)
		defer c.Close()

		for i := range 10 {
			c.Set(ctx, fmt.Sprint(i), i)
			assert.LessOrEqual(t, c.Len(), 3)
		}
		got, ok := c.Get(ctx, "9")
		assert.True(t, ok, "the newest entry is always kept")
		assert.Equal(t, 9, got)

		c.Set(ctx, "9", 99)
		assert.Equal(t, 3, c.Len(), "overwriting does not evict")
	})
}

func TestMemoryConcurrentUse(t *testing.T) {
	ctx := context.Background()
	c := NewMemory[int](time.Minute, 50, time.Millisecond)
	defer c.Close()

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 1000 {
				key := fmt.Sprint((g * i) % 100)
				c.Set(ctx, key, i)
				c.Get(ctx, key)
			}
		})
	}
	wg.Wait()
	assert.LessOrEqual(t, c.Len(), 50)
	assert.NoError(t, c.Ping(ctx))
}
