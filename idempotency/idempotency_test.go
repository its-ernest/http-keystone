package idempotency

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newContext(e *echo.Echo, method, path, body string, headers map[string]string) (*echo.Context, *httptest.ResponseRecorder) {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, bytes.NewBufferString(body))
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	return e.NewContext(r, rec), rec
}

// TestIdempotentConcurrentExecutesOnce asserts that many concurrent requests
// with the same key run the handler exactly once.
func TestIdempotentConcurrentExecutesOnce(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	var calls atomic.Int32

	handler := New(Config{
		Store:      st,
		TTL:        time.Minute,
		InFlight:   InFlightWait,
		RetryDelay: time.Millisecond,
		MaxRetries: 200,
	})(func(c *echo.Context) error {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		return c.String(http.StatusOK, "created")
	})

	const clients = 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, clients)

	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c, _ := newContext(e, http.MethodPost, "/pay", `{"amount":100}`, map[string]string{"Idempotency-Key": "key-1"})
			errs <- handler(c)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), calls.Load(), "concurrent duplicates must run the handler once")
}

// TestIdempotentRetryReplays asserts that a later request with the same key and
// body replays the stored response without running the handler.
func TestIdempotentRetryReplays(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0

	handler := New(Config{Store: st, TTL: time.Minute})(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusCreated, "stored")
	})

	c1, rec1 := newContext(e, http.MethodPost, "/pay", "body", map[string]string{"Idempotency-Key": "k"})
	require.NoError(t, handler(c1))
	assert.Equal(t, "stored", rec1.Body.String())
	assert.Equal(t, http.StatusCreated, rec1.Code)
	assert.Empty(t, rec1.Header().Get("Idempotency-Replayed"))

	c2, rec2 := newContext(e, http.MethodPost, "/pay", "body", map[string]string{"Idempotency-Key": "k"})
	require.NoError(t, handler(c2))
	assert.Equal(t, "stored", rec2.Body.String())
	assert.Equal(t, http.StatusCreated, rec2.Code)
	assert.Equal(t, "true", rec2.Header().Get("Idempotency-Replayed"))
	assert.Equal(t, 1, calls, "a retry must not run the handler")
}

// TestIdempotentDifferentBodyRejected asserts that reusing a key with a
// different body is rejected and never replays the wrong response.
func TestIdempotentDifferentBodyRejected(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0

	handler := New(Config{Store: st, TTL: time.Minute})(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "first")
	})

	c1, _ := newContext(e, http.MethodPost, "/pay", "one", map[string]string{"Idempotency-Key": "k"})
	require.NoError(t, handler(c1))

	c2, _ := newContext(e, http.MethodPost, "/pay", "two", map[string]string{"Idempotency-Key": "k"})
	err := handler(c2)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrFingerprintMismatch)
	assert.Equal(t, 1, calls, "a mismatch must not run the handler")
}

// TestIdempotentMissingKeyPassesThrough asserts a missing key passes through
// unless Required is set.
func TestIdempotentMissingKeyPassesThrough(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0

	handler := New(Config{Store: st, TTL: time.Minute})(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "ok")
	})

	c, rec := newContext(e, http.MethodPost, "/pay", "x", nil)
	require.NoError(t, handler(c))
	assert.Equal(t, "ok", rec.Body.String())
	assert.Equal(t, 1, calls)
}

// TestIdempotentMissingKeyRequired asserts a missing key is rejected when
// Required is set.
func TestIdempotentMissingKeyRequired(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0

	handler := New(Config{Store: st, TTL: time.Minute, Required: true})(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "ok")
	})

	c, _ := newContext(e, http.MethodPost, "/pay", "x", nil)
	err := handler(c)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrKeyRequired)
	assert.Equal(t, 0, calls)
}

// TestIdempotentSafeMethodPassesThrough asserts GET/HEAD/OPTIONS are not
// protected.
func TestIdempotentSafeMethodPassesThrough(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0

	handler := New(Config{Store: st, TTL: time.Minute, Required: true})(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "ok")
	})

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		c, _ := newContext(e, method, "/pay", "", nil)
		require.NoError(t, handler(c))
	}
	assert.Equal(t, 3, calls, "safe methods must bypass idempotency")
}

// TestIdempotentServerErrorNotCached asserts 5xx responses are not stored, so a
// retry re-runs the handler.
func TestIdempotentServerErrorNotCached(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0

	handler := New(Config{Store: st, TTL: time.Minute})(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusInternalServerError, "boom")
	})

	c1, _ := newContext(e, http.MethodPost, "/pay", "x", map[string]string{"Idempotency-Key": "k"})
	require.NoError(t, handler(c1))
	c2, _ := newContext(e, http.MethodPost, "/pay", "x", map[string]string{"Idempotency-Key": "k"})
	require.NoError(t, handler(c2))

	assert.Equal(t, 2, calls, "a 5xx must not be cached")
}

// TestIdempotentTTLExpiry asserts that after the TTL the key is forgotten and
// the handler runs again.
func TestIdempotentTTLExpiry(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()
	calls := 0

	handler := New(Config{Store: st, TTL: 50 * time.Millisecond})(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "ok")
	})

	c1, _ := newContext(e, http.MethodPost, "/pay", "x", map[string]string{"Idempotency-Key": "k"})
	require.NoError(t, handler(c1))

	time.Sleep(80 * time.Millisecond)

	c2, rec2 := newContext(e, http.MethodPost, "/pay", "x", map[string]string{"Idempotency-Key": "k"})
	require.NoError(t, handler(c2))
	assert.Empty(t, rec2.Header().Get("Idempotency-Replayed"), "an expired key must not replay")
	assert.Equal(t, 2, calls)
}

// TestIdempotentInFlightReject asserts that a concurrent duplicate gets a
// conflict under the default reject policy.
func TestIdempotentInFlightReject(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()

	handler := New(Config{
		Store:    st,
		TTL:      time.Minute,
		InFlight: InFlightReject,
	})(func(c *echo.Context) error {
		time.Sleep(50 * time.Millisecond)
		return c.String(http.StatusOK, "done")
	})

	// First request holds the lock and runs.
	go func() {
		c, _ := newContext(e, http.MethodPost, "/pay", "x", map[string]string{"Idempotency-Key": "k"})
		_ = handler(c)
	}()
	time.Sleep(10 * time.Millisecond)

	c2, _ := newContext(e, http.MethodPost, "/pay", "x", map[string]string{"Idempotency-Key": "k"})
	err := handler(c2)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrRequestInFlight)
}

// TestIdempotentStoreFailureFailsOpen asserts a store failure runs the handler
// when FailClosed is false.
func TestIdempotentStoreFailureFailsOpen(t *testing.T) {
	e := echo.New()
	st := &failingStore{Store: store.NewMemoryStore(), Locker: store.NewMemoryStore()}
	calls := 0

	handler := New(Config{Store: st, TTL: time.Minute})(func(c *echo.Context) error {
		calls++
		return c.String(http.StatusOK, "ok")
	})

	c, rec := newContext(e, http.MethodPost, "/pay", "x", map[string]string{"Idempotency-Key": "k"})
	require.NoError(t, handler(c))
	assert.Equal(t, "ok", rec.Body.String())
	assert.Equal(t, 1, calls)
}

// TestIdempotentKeyFuncOverridesHeader asserts KeyFunc takes precedence.
func TestIdempotentKeyFuncOverridesHeader(t *testing.T) {
	e := echo.New()
	st := store.NewMemoryStore()

	handler := New(Config{
		Store: st,
		TTL:   time.Minute,
		KeyFunc: func(c *echo.Context) string {
			return "tenant:" + c.Request().Header.Get("X-Tenant")
		},
	})(func(c *echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	c1, _ := newContext(e, http.MethodPost, "/pay", "x", map[string]string{"X-Tenant": "a"})
	require.NoError(t, handler(c1))
	c2, rec2 := newContext(e, http.MethodPost, "/pay", "x", map[string]string{"X-Tenant": "a"})
	require.NoError(t, handler(c2))
	assert.Equal(t, "true", rec2.Header().Get("Idempotency-Replayed"))
}

// failingStore fails every Get with an error but otherwise behaves like a
// real store (including locks, so it satisfies LockingStore).
type failingStore struct {
	store.Store
	store.Locker
}

func (s *failingStore) Get(ctx context.Context, key string) (*store.Entry, error) {
	return nil, assert.AnError
}
