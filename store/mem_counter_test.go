package store

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitFor polls cond until it is true or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return cond()
}

func TestMemoryCounterNewReturnsConcreteType(t *testing.T) {
	var _ *MemoryCounter = NewMemoryCounter()
	var _ Counter = NewMemoryCounter()
}

func TestMemoryCounterExpiredKeyIsRemovedOnAccess(t *testing.T) {
	ctx := context.Background()
	c := NewMemoryCounter()

	_, err := c.Increment(ctx, "1.2.3.4", 5, 5*time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, 1, c.Len())

	require.True(t, waitFor(t, 2*time.Second, func() bool {
		score, getErr := c.Get(ctx, "1.2.3.4")
		return errors.Is(getErr, ErrNotFound) && score == 0
	}))

	assert.Equal(t, 0, c.Len(), "Get must delete the expired key, not just hide it")
}

func TestMemoryCounterEvictorRemovesExpiredKeys(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := NewMemoryCounter()
	c.StartEvictor(ctx, 2*time.Millisecond)
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	for i := 0; i < 50; i++ {
		_, err := c.Increment(ctx, "client-"+strconv.Itoa(i), 1, 5*time.Millisecond)
		require.NoError(t, err)
	}

	// The janitor must drop the keys without any request touching them.
	require.True(t, waitFor(t, 2*time.Second, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.counts) == 0 && len(c.expiries) == 0
	}), "background evictor did not remove expired keys")
}

func TestMemoryCounterCloseStopsEvictor(t *testing.T) {
	ctx := context.Background()
	c := NewMemoryCounter()
	c.StartEvictor(ctx, 2*time.Millisecond)
	require.NoError(t, c.Close())
	require.NoError(t, c.Close(), "Close must be idempotent")

	_, err := c.Increment(ctx, "stale", 1, 5*time.Millisecond)
	require.NoError(t, err)

	// Give a hypothetical evictor ample time to run, then inspect the raw maps:
	// nothing should have been removed by a background goroutine.
	time.Sleep(50 * time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	assert.Len(t, c.counts, 1)
}

func TestMemoryCounterMaxEntries(t *testing.T) {
	ctx := context.Background()
	c := NewMemoryCounterWithLimit(2)

	_, err := c.Increment(ctx, "a", 1, time.Hour)
	require.NoError(t, err)
	_, err = c.Increment(ctx, "b", 1, 2*time.Hour)
	require.NoError(t, err)
	require.Equal(t, 2, c.Len())

	// The soonest-expiring key ("a") makes room for the new one.
	_, err = c.Increment(ctx, "c", 1, 3*time.Hour)
	require.NoError(t, err)

	assert.Equal(t, 2, c.Len(), "memory must stay bounded")
	_, err = c.Get(ctx, "a")
	assert.ErrorIs(t, err, ErrNotFound, "the earliest-expiring key must be evicted")
	score, err := c.Get(ctx, "c")
	require.NoError(t, err)
	assert.Equal(t, 1, score)
}

func TestMemoryCounterMaxEntriesPrefersExpiredKeys(t *testing.T) {
	ctx := context.Background()
	c := NewMemoryCounterWithLimit(2)

	_, err := c.Increment(ctx, "live", 1, time.Hour)
	require.NoError(t, err)
	_, err = c.Increment(ctx, "dead", 1, time.Millisecond)
	require.NoError(t, err)

	require.True(t, waitFor(t, 2*time.Second, func() bool {
		exp, ok := func() (time.Time, bool) {
			c.mu.Lock()
			defer c.mu.Unlock()
			exp, ok := c.expiries["dead"]
			return exp, ok
		}()
		return ok && time.Now().After(exp)
	}))

	_, err = c.Increment(ctx, "new", 1, time.Hour)
	require.NoError(t, err)

	score, err := c.Get(ctx, "live")
	require.NoError(t, err)
	assert.Equal(t, 1, score, "an expired key must be evicted before a live one")
}

func TestMemoryCounterUnlimitedByDefault(t *testing.T) {
	ctx := context.Background()
	c := NewMemoryCounter()

	for i := 0; i < 5000; i++ {
		_, err := c.Increment(ctx, "client-"+strconv.Itoa(i), 1, time.Hour)
		require.NoError(t, err)
	}
	assert.Equal(t, 5000, c.Len())
}

func TestMemoryCounterConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	c := NewMemoryCounterWithLimit(64)
	c.StartEvictor(ctx, time.Millisecond)
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := net.JoinHostPort("10.0.0."+strconv.Itoa(i), "0")
			for j := 0; j < 100; j++ {
				_, _ = c.Increment(ctx, key, 1, time.Millisecond)
				_, _ = c.Get(ctx, key)
				_ = c.Len()
				if j%10 == 0 {
					_ = c.Reset(ctx, key)
				}
			}
		}(i)
	}
	wg.Wait()

	assert.LessOrEqual(t, c.Len(), 64)
}
