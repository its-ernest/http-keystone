package cache

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingStore records store interactions used by the stampede tests.
type countingStore struct {
	*store.MemoryStore

	mu      sync.Mutex
	gets    int
	lockTTL time.Duration
}

func newCountingStore() *countingStore {
	return &countingStore{MemoryStore: store.NewMemoryStore()}
}

func (s *countingStore) Get(ctx context.Context, key string) (*store.Entry, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	return s.MemoryStore.Get(ctx, key)
}

func (s *countingStore) Lock(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	s.lockTTL = ttl
	s.mu.Unlock()
	return s.MemoryStore.Lock(ctx, key, ttl)
}

func (s *countingStore) getCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

func (s *countingStore) recordedLockTTL() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lockTTL
}

func TestCacheStampedeRunsHandlerOnce(t *testing.T) {
	e := echo.New()
	st := newCountingStore()
	var calls atomic.Int32

	handler := New(Config{
		Store:      st,
		TTL:        time.Minute,
		RetryDelay: 5 * time.Millisecond,
		MaxRetries: 100,
	})(func(c *echo.Context) error {
		calls.Add(1)
		time.Sleep(100 * time.Millisecond)
		return c.String(http.StatusOK, "slow")
	})

	const clients = 100
	var wg sync.WaitGroup
	start := make(chan struct{})
	statuses := make(chan int, clients)
	bodies := make(chan string, clients)
	caches := make(chan string, clients)
	etags := make(chan string, clients)
	errs := make(chan error, clients)

	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			req := httptest.NewRequest(http.MethodGet, "/cold", nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)

			errs <- handler(c)
			statuses <- rec.Code
			bodies <- rec.Body.String()
			caches <- rec.Header().Get("X-Cache")
			etags <- rec.Header().Get("ETag")
		}()
	}

	close(start)
	wg.Wait()
	close(errs)
	close(statuses)
	close(bodies)
	close(caches)
	close(etags)

	for err := range errs {
		require.NoError(t, err)
	}
	for status := range statuses {
		assert.Equal(t, http.StatusOK, status)
	}
	for body := range bodies {
		assert.Equal(t, "slow", body)
	}
	for etag := range etags {
		assert.NotEmpty(t, etag, "every client must receive the ETag, including waiters")
	}

	assert.Equal(t, int32(1), calls.Load(), "concurrent misses must execute the handler once")

	hits, misses := 0, 0
	for cache := range caches {
		switch cache {
		case "HIT":
			hits++
		case "MISS":
			misses++
		default:
			t.Errorf("unexpected X-Cache value %q", cache)
		}
	}
	assert.Equal(t, 1, misses)
	assert.Equal(t, clients-1, hits, "waiters must replay the populated entry")
}

func TestCacheWaiterRetriesAreBounded(t *testing.T) {
	e := echo.New()
	st := newCountingStore()
	calls := 0

	cfg := Config{
		Store:      st,
		TTL:        time.Minute,
		RetryDelay: time.Millisecond,
		MaxRetries: 3,
	}
	handler := New(cfg)(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "origin")
	})

	// Hold the lock as another request would.
	key := cacheKey(mustContext(t, e, "/busy"), resolvedConfig(cfg))
	locked, err := st.Lock(context.Background(), key+":lock", time.Minute)
	require.NoError(t, err)
	require.True(t, locked)
	defer st.Unlock(context.Background(), key+":lock")

	c, rec := newCacheContext(t, e, http.MethodGet, "/busy")
	require.NoError(t, handler(c))

	assert.Equal(t, "origin", rec.Body.String())
	assert.Equal(t, 1, calls)
	// One initial lookup plus exactly MaxRetries polls.
	assert.Equal(t, cfg.MaxRetries+1, st.getCount())
}

func TestCacheWaiterReturnsContextError(t *testing.T) {
	e := echo.New()
	st := newCountingStore()
	called := false

	cfg := Config{
		Store:      st,
		TTL:        time.Minute,
		RetryDelay: 50 * time.Millisecond,
		MaxRetries: 100,
	}
	handler := New(cfg)(func(c *echo.Context) error {
		called = true
		return c.String(http.StatusOK, "origin")
	})

	key := cacheKey(mustContext(t, e, "/cancel"), resolvedConfig(cfg))
	locked, err := st.Lock(context.Background(), key+":lock", time.Minute)
	require.NoError(t, err)
	require.True(t, locked)
	defer st.Unlock(context.Background(), key+":lock")

	req := httptest.NewRequest(http.MethodGet, "/cancel", nil)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	c := e.NewContext(req.WithContext(ctx), httptest.NewRecorder())

	start := time.Now()
	err = handler(c)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, elapsed, time.Second, "a canceled waiter must return promptly")
	assert.False(t, called, "a canceled waiter must not run the handler")
}

func TestCacheLockTTLIsConfigurable(t *testing.T) {
	e := echo.New()
	st := newCountingStore()

	handler := New(Config{
		Store:   st,
		TTL:     time.Minute,
		LockTTL: 42 * time.Second,
	})(func(c *echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	c, _ := newCacheContext(t, e, http.MethodGet, "/ttl")
	require.NoError(t, handler(c))

	assert.Equal(t, 42*time.Second, st.recordedLockTTL())
}

// populateOnFirstMiss simulates another request populating the entry between
// the middleware's cache miss and its lock acquisition.
type populateOnFirstMiss struct {
	*store.MemoryStore

	entry *store.Entry
	once  sync.Once
}

func (s *populateOnFirstMiss) Get(ctx context.Context, key string) (*store.Entry, error) {
	entry, err := s.MemoryStore.Get(ctx, key)
	if err != nil {
		s.once.Do(func() {
			_ = s.MemoryStore.Save(ctx, key, s.entry, time.Minute)
		})
	}
	return entry, err
}

func TestCachePopulatedEntryServedAfterLockAcquisition(t *testing.T) {
	e := echo.New()
	st := &populateOnFirstMiss{
		MemoryStore: store.NewMemoryStore(),
		entry: &store.Entry{
			Status: http.StatusOK,
			Header: map[string][]string{"Content-Type": {"text/plain"}},
			Body:   []byte("cached"),
		},
	}
	calls := 0

	handler := New(Config{Store: st, TTL: time.Minute})(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "fresh")
	})

	c, rec := newCacheContext(t, e, http.MethodGet, "/raced")
	require.NoError(t, handler(c))

	assert.Equal(t, "cached", rec.Body.String())
	assert.Equal(t, "HIT", rec.Header().Get("X-Cache"))
	assert.Equal(t, 0, calls, "the double-check must serve the entry populated while acquiring the lock")
}

func TestRetryDelayGrowthAndJitter(t *testing.T) {
	base := 20 * time.Millisecond

	for _, tc := range []struct {
		attempt int
		min     time.Duration
		max     time.Duration
	}{
		{0, 15 * time.Millisecond, 25 * time.Millisecond},
		{1, 30 * time.Millisecond, 50 * time.Millisecond},
		{3, 60 * time.Millisecond, 100 * time.Millisecond},
	} {
		for i := 0; i < 100; i++ {
			got := retryDelay(base, tc.attempt)
			assert.GreaterOrEqual(t, got, tc.min, "attempt %d", tc.attempt)
			assert.LessOrEqual(t, got, tc.max, "attempt %d", tc.attempt)
		}
	}

	assert.Equal(t, time.Duration(0), retryDelay(0, 5))
	assert.Equal(t, time.Duration(0), retryDelay(-time.Second, 5))
}

func mustContext(t *testing.T, e *echo.Echo, path string) *echo.Context {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	return e.NewContext(req, httptest.NewRecorder())
}

func TestCacheWaiterReplaysVaryEntry(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()

	cfg := Config{
		Store:       st,
		TTL:         time.Minute,
		VaryHeaders: []string{"Accept-Encoding"},
	}
	handler := New(cfg)(func(c *echo.Context) error {
		return c.String(http.StatusOK, "body-"+c.Request().Header.Get("Accept-Encoding"))
	})

	// Prime the entry for gzip.
	prime, _ := newCacheContextWithHeaders(t, e, "/vary", map[string]string{"Accept-Encoding": "gzip"})
	require.NoError(t, handler(prime))

	// Hold the lock under the gzip key so a second gzip request would wait.
	key := cacheKey(prime, resolvedConfig(cfg))
	locked, err := st.Lock(context.Background(), key+":lock", time.Minute)
	require.NoError(t, err)
	require.True(t, locked)
	defer st.Unlock(context.Background(), key+":lock")

	// The waiter finds the entry immediately and must not touch the lock.
	waiter, rec := newCacheContextWithHeaders(t, e, "/vary", map[string]string{"Accept-Encoding": "gzip"})
	require.NoError(t, handler(waiter))
	assert.Equal(t, "body-gzip", rec.Body.String())
	assert.Equal(t, "HIT", rec.Header().Get("X-Cache"))
	assert.True(t, strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding"))
}
