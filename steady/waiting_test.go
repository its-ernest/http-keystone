package steady

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// waitUntil polls cond with a deadline; tests never sleep for a fixed period.
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) bool {
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

func newSteadyContext(e *echo.Echo, ctx context.Context) (*echo.Context, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

func TestMaxWaitingRejectsImmediately(t *testing.T) {
	e := echo.New()
	release := make(chan struct{})
	stats := &Stats{}

	var (
		mu    sync.Mutex
		codes []int
	)
	handler := New(Config{
		MaxConcurrent: 1,
		MaxWaiting:    2,
		WaitTimeout:   5 * time.Second,
		Stats:         stats,
	})(func(c *echo.Context) error {
		<-release
		return c.String(http.StatusOK, "ok")
	})

	run := func(c *echo.Context, rec *httptest.ResponseRecorder) {
		_ = handler(c)
		mu.Lock()
		codes = append(codes, rec.Code)
		mu.Unlock()
	}

	// Request 1 holds the only slot.
	c1, _ := newSteadyContext(e, nil)
	go func() { _ = handler(c1) }()
	require.True(t, waitUntil(t, time.Second, func() bool { return stats.InFlight.Load() == 1 }))

	// Requests 2 and 3 queue.
	c2, rec2 := newSteadyContext(e, nil)
	c3, rec3 := newSteadyContext(e, nil)
	go run(c2, rec2)
	go run(c3, rec3)
	require.True(t, waitUntil(t, time.Second, func() bool { return stats.Waiting.Load() == 2 }),
		"requests 2 and 3 must be queued")

	// Request 4 is rejected without waiting for the queue to drain.
	c4, rec4 := newSteadyContext(e, nil)
	start := time.Now()
	err := handler(c4)
	elapsed := time.Since(start)

	require.Error(t, err)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	assert.Equal(t, http.StatusServiceUnavailable, he.Code)
	assert.ErrorIs(t, err, ErrQueueFull)
	assert.NotEmpty(t, rec4.Header().Get("Retry-After"))
	assert.Less(t, elapsed, 250*time.Millisecond, "a full queue must reject immediately")
	assert.Equal(t, int64(1), stats.Rejected.Load())

	// Releasing the slot lets the queued requests through.
	close(release)
	require.True(t, waitUntil(t, 2*time.Second, func() bool {
		return stats.InFlight.Load() == 0 && stats.Waiting.Load() == 0
	}))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []int{http.StatusOK, http.StatusOK}, codes)
}

func TestCanceledWaiterReleasesQueueSlot(t *testing.T) {
	e := echo.New()
	release := make(chan struct{})
	stats := &Stats{}

	handler := New(Config{
		MaxConcurrent: 1,
		MaxWaiting:    1,
		WaitTimeout:   5 * time.Second,
		Stats:         stats,
	})(func(c *echo.Context) error {
		<-release
		return c.String(http.StatusOK, "ok")
	})

	c1, _ := newSteadyContext(e, nil)
	go func() { _ = handler(c1) }()
	require.True(t, waitUntil(t, time.Second, func() bool { return stats.InFlight.Load() == 1 }))

	ctx, cancel := context.WithCancel(context.Background())
	c2, _ := newSteadyContext(e, ctx)

	done := make(chan error, 1)
	go func() { done <- handler(c2) }()
	require.True(t, waitUntil(t, time.Second, func() bool { return stats.Waiting.Load() == 1 }))

	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("a canceled waiter must return immediately")
	}

	require.True(t, waitUntil(t, time.Second, func() bool { return stats.Waiting.Load() == 0 }))

	// The freed queue slot can now be used by another request.
	close(release)
	require.True(t, waitUntil(t, time.Second, func() bool { return stats.InFlight.Load() == 0 }))
}

func TestWaitTimeoutStillReturnsRetryAfter(t *testing.T) {
	e := echo.New()
	release := make(chan struct{})
	stats := &Stats{}

	handler := New(Config{
		MaxConcurrent: 1,
		MaxWaiting:    4,
		WaitTimeout:   50 * time.Millisecond,
		Stats:         stats,
	})(func(c *echo.Context) error {
		<-release
		return c.String(http.StatusOK, "ok")
	})

	c1, _ := newSteadyContext(e, nil)
	go func() { _ = handler(c1) }()
	require.True(t, waitUntil(t, time.Second, func() bool { return stats.InFlight.Load() == 1 }))

	c2, rec2 := newSteadyContext(e, nil)
	err := handler(c2)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	assert.Equal(t, http.StatusServiceUnavailable, he.Code)
	assert.NotEmpty(t, rec2.Header().Get("Retry-After"))
	assert.Equal(t, int64(1), stats.Rejected.Load())
	assert.NotErrorIs(t, err, ErrQueueFull)

	close(release)
}
