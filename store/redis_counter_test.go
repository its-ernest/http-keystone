package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRedisCounter(t *testing.T) (*RedisCounter, *miniredis.Miniredis) {
	t.Helper()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	counter := NewRedisCounter(mr.Addr())
	t.Cleanup(func() { require.NoError(t, counter.Close()) })

	return counter, mr
}

func TestRedisCounter_GetMissing(t *testing.T) {
	counter, _ := newTestRedisCounter(t)

	got, err := counter.Get(context.Background(), "missing")
	require.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, 0, got)
}

func TestRedisCounter_IncrementAndGet(t *testing.T) {
	counter, _ := newTestRedisCounter(t)
	ctx := context.Background()

	got, err := counter.Increment(ctx, "score", 2, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 2, got)

	got, err = counter.Increment(ctx, "score", 3, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, 5, got)

	got, err = counter.Get(ctx, "score")
	require.NoError(t, err)
	assert.Equal(t, 5, got)
}

func TestRedisCounter_TTLExpires(t *testing.T) {
	counter, mr := newTestRedisCounter(t)
	ctx := context.Background()

	_, err := counter.Increment(ctx, "score", 7, time.Second)
	require.NoError(t, err)

	mr.FastForward(1500 * time.Millisecond)

	got, err := counter.Get(ctx, "score")
	require.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, 0, got)
}

func TestRedisCounter_IncrementRefreshesTTL(t *testing.T) {
	counter, mr := newTestRedisCounter(t)
	ctx := context.Background()

	_, err := counter.Increment(ctx, "score", 1, time.Second)
	require.NoError(t, err)

	mr.FastForward(900 * time.Millisecond)

	// The second increment should slide the expiry window forward.
	_, err = counter.Increment(ctx, "score", 1, time.Second)
	require.NoError(t, err)

	mr.FastForward(900 * time.Millisecond)

	got, err := counter.Get(ctx, "score")
	require.NoError(t, err)
	assert.Equal(t, 2, got, "sliding TTL should keep the key alive")
}

func TestRedisCounter_ZeroTTLNeverExpires(t *testing.T) {
	counter, mr := newTestRedisCounter(t)
	ctx := context.Background()

	_, err := counter.Increment(ctx, "score", 3, 0)
	require.NoError(t, err)

	mr.FastForward(24 * time.Hour)

	got, err := counter.Get(ctx, "score")
	require.NoError(t, err)
	assert.Equal(t, 3, got)
}

func TestRedisCounter_SubMillisecondTTLIsNotDropped(t *testing.T) {
	counter, mr := newTestRedisCounter(t)
	ctx := context.Background()

	_, err := counter.Increment(ctx, "score", 1, time.Nanosecond)
	require.NoError(t, err)

	mr.FastForward(5 * time.Millisecond)

	got, err := counter.Get(ctx, "score")
	require.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, 0, got)
}

func TestRedisCounter_Reset(t *testing.T) {
	counter, _ := newTestRedisCounter(t)
	ctx := context.Background()

	_, err := counter.Increment(ctx, "score", 4, time.Minute)
	require.NoError(t, err)

	require.NoError(t, counter.Reset(ctx, "score"))

	got, err := counter.Get(ctx, "score")
	require.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, 0, got)
}

func TestRedisCounter_ConcurrentIncrements(t *testing.T) {
	counter, _ := newTestRedisCounter(t)
	ctx := context.Background()

	const goroutines = 100
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := counter.Increment(ctx, "concurrent", 1, time.Minute)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	got, err := counter.Get(ctx, "concurrent")
	require.NoError(t, err)
	assert.Equal(t, goroutines, got)
}

func TestRedisCounter_Ping(t *testing.T) {
	counter, _ := newTestRedisCounter(t)
	require.NoError(t, counter.Ping(context.Background()))
}

func TestRedisCounter_ContextCancellation(t *testing.T) {
	counter, _ := newTestRedisCounter(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := counter.Increment(ctx, "score", 1, time.Minute)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestRedisCounter_ClosedClientReturnsError(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	counter := NewRedisCounter(mr.Addr())
	require.NoError(t, counter.Close())

	ctx := context.Background()
	_, err = counter.Get(ctx, "score")
	assert.Error(t, err)

	_, err = counter.Increment(ctx, "score", 1, time.Minute)
	assert.Error(t, err)

	assert.Error(t, counter.Reset(ctx, "score"))
	assert.Error(t, counter.Ping(ctx))
}

func TestRedisCounter_BorrowedClientIsNotClosed(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()

	counter := NewRedisCounterWithClient(client)
	require.NoError(t, counter.Close(), "closing a borrowed counter must be a no-op")
	require.NoError(t, client.Ping(context.Background()).Err())
}
