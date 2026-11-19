package cache

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noLockStore implements only store.Store (Reader+Writer) and deliberately
// does NOT implement store.Locker. It proves cache.New works with a backend
// that has no lock methods: stampede protection is simply skipped.
type noLockStore struct {
	data map[string]*store.Entry
}

func (s *noLockStore) Get(_ context.Context, key string) (*store.Entry, error) {
	e, ok := s.data[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return e, nil
}

func (s *noLockStore) Save(_ context.Context, key string, entry *store.Entry, _ time.Duration) error {
	s.data[key] = entry
	return nil
}

func (s *noLockStore) Delete(_ context.Context, key string) error {
	delete(s.data, key)
	return nil
}

var (
	_ store.Store = (*noLockStore)(nil)
	// Ensure it does not accidentally satisfy Locker.
	_ = func() bool { _, ok := interface{}((*noLockStore)(nil)).(store.Locker); return ok }
)

// TestCacheWorksWithoutLocker asserts cache.New compiles and works with a
// store that has no lock methods.
func TestCacheWorksWithoutLocker(t *testing.T) {
	e := echo.New()
	st := &noLockStore{data: make(map[string]*store.Entry)}
	calls := 0

	handler := New(Config{Store: st, TTL: time.Minute})(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "hello")
	})

	c1, rec1 := newCacheContext(t, e, http.MethodGet, "/nolock")
	require.NoError(t, handler(c1))
	assert.Equal(t, "hello", rec1.Body.String())
	assert.Equal(t, "MISS", rec1.Header().Get("X-Cache"))

	c2, rec2 := newCacheContext(t, e, http.MethodGet, "/nolock")
	require.NoError(t, handler(c2))
	assert.Equal(t, "hello", rec2.Body.String())
	assert.Equal(t, "HIT", rec2.Header().Get("X-Cache"))
	assert.Equal(t, 1, calls, "a hit must not run the handler")
}

// TestNoLockStoreIsNotLocker is a compile-time guard that noLockStore does not
// implement Locker.
func TestNoLockStoreIsNotLocker(t *testing.T) {
	var s store.Store = &noLockStore{data: make(map[string]*store.Entry)}
	_, ok := s.(store.Locker)
	assert.False(t, ok)
}
