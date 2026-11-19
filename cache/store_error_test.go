package cache

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// faultyStore wraps a Store and injects failures for selected methods. It
// also implements Locker so it satisfies the locking capability the cache
// middleware type-asserts for.
type faultyStore struct {
	store.Store
	store.Locker

	getErr  error
	saveErr error
	lockErr error
}

func (s *faultyStore) Get(ctx context.Context, key string) (*store.Entry, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.Store.Get(ctx, key)
}

func (s *faultyStore) Save(ctx context.Context, key string, entry *store.Entry, ttl time.Duration) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.Store.Save(ctx, key, entry, ttl)
}

func (s *faultyStore) Lock(ctx context.Context, key string, ttl time.Duration) (*store.Lease, error) {
	if s.lockErr != nil {
		return nil, s.lockErr
	}
	return s.Locker.Lock(ctx, key, ttl)
}

func TestCacheGetErrorFailsOpen(t *testing.T) {
	e := echo.New()
	st := &faultyStore{Store: store.NewMemoryStore(), Locker: store.NewMemoryStore(), getErr: errors.New("store unavailable")}
	calls := 0

	handler := New(Config{
		Store:      st,
		TTL:        time.Minute,
		RetryDelay: time.Millisecond,
		MaxRetries: 1,
	})(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "ok")
	})

	for i := 0; i < 2; i++ {
		c, rec := newCacheContext(t, e, http.MethodGet, "/get-error")
		require.NoError(t, handler(c))
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "ok", rec.Body.String())
	}

	assert.Equal(t, 2, calls, "a broken store must not break requests or serve stale data")
}

func TestCacheSaveErrorFailsOpen(t *testing.T) {
	e := echo.New()
	st := &faultyStore{Store: store.NewMemoryStore(), Locker: store.NewMemoryStore(), saveErr: errors.New("disk full")}
	calls := 0

	handler := New(Config{Store: st, TTL: time.Minute})(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "ok")
	})

	first, rec1 := newCacheContext(t, e, http.MethodGet, "/save-error")
	require.NoError(t, handler(first))
	assert.Equal(t, "ok", rec1.Body.String())
	assert.Equal(t, "BYPASS", rec1.Header().Get("X-Cache"), "an unsuccessful Save must be reported")

	second, rec2 := newCacheContext(t, e, http.MethodGet, "/save-error")
	require.NoError(t, handler(second))
	assert.Equal(t, "ok", rec2.Body.String())
	assert.Equal(t, "BYPASS", rec2.Header().Get("X-Cache"))
	assert.Equal(t, 2, calls)
}

func TestCacheLockErrorFailsOpen(t *testing.T) {
	e := echo.New()
	st := &faultyStore{Store: store.NewMemoryStore(), Locker: store.NewMemoryStore(), lockErr: errors.New("lock backend down")}
	calls := 0

	handler := New(Config{
		Store:      st,
		TTL:        time.Minute,
		RetryDelay: time.Millisecond,
		MaxRetries: 2,
	})(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "ok")
	})

	c, rec := newCacheContext(t, e, http.MethodGet, "/lock-error")
	require.NoError(t, handler(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok", rec.Body.String())
	assert.Equal(t, 1, calls, "a failed lock must fall back to the handler")

	// The fallback path does not own a lock, so it does not populate the cache.
	// A later request must still return a response instead of failing.
	c2, rec2 := newCacheContext(t, e, http.MethodGet, "/lock-error")
	require.NoError(t, handler(c2))
	assert.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, "ok", rec2.Body.String())
	assert.Equal(t, 2, calls)
}

func TestCacheTTLExpiry(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	redisStore := store.NewRedisStore(mr.Addr())
	defer func() { require.NoError(t, redisStore.Close()) }()

	e := echo.New()
	calls := 0
	handler := New(Config{Store: redisStore, TTL: time.Second})(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, fmt.Sprintf("call-%d", calls))
	})

	first, rec1 := newCacheContext(t, e, http.MethodGet, "/ttl")
	require.NoError(t, handler(first))
	assert.Equal(t, "call-1", rec1.Body.String())
	assert.Equal(t, "MISS", rec1.Header().Get("X-Cache"))

	hit, recHit := newCacheContext(t, e, http.MethodGet, "/ttl")
	require.NoError(t, handler(hit))
	assert.Equal(t, "call-1", recHit.Body.String())
	assert.Equal(t, "HIT", recHit.Header().Get("X-Cache"))
	assert.Equal(t, 1, calls)

	// Expire the cache entry (the lock is already released).
	mr.FastForward(1500 * time.Millisecond)

	after, recAfter := newCacheContext(t, e, http.MethodGet, "/ttl")
	require.NoError(t, handler(after))
	assert.Equal(t, "call-2", recAfter.Body.String())
	assert.Equal(t, "MISS", recAfter.Header().Get("X-Cache"))
	assert.Equal(t, 2, calls, "an expired entry must be recomputed")
}
