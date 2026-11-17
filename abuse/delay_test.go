package abuse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// staticCounter reports a fixed heat score and records writes.
type staticCounter struct {
	score      int
	increments atomic.Int64
}

func (s *staticCounter) Get(context.Context, string) (int, error) { return s.score, nil }

func (s *staticCounter) Increment(_ context.Context, _ string, n int, _ time.Duration) (int, error) {
	s.increments.Add(1)
	return s.score + n, nil
}

func (s *staticCounter) Reset(context.Context, string) error { return nil }

// runTimed executes handler and returns the error plus the elapsed wall time.
func runTimed(t *testing.T, handler echo.HandlerFunc, req *http.Request) (error, time.Duration) {
	t.Helper()
	e := echo.New()
	c := e.NewContext(req, httptest.NewRecorder())

	start := time.Now()
	err := handler(c)
	return err, time.Since(start)
}

func newDelayHandler(t *testing.T, config Config, counter *staticCounter) echo.HandlerFunc {
	t.Helper()
	config.Store = counter
	config.EnableDelay = true
	config.Rules = []Rule{{Path: "/warm", Score: 1}}

	return New(config)(func(c *echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})
}

func TestDelayClampsToMaxDelay(t *testing.T) {
	counter := &staticCounter{score: 10_000}
	handler := newDelayHandler(t, Config{
		Threshold: 15_000,
		MaxDelay:  50 * time.Millisecond,
	}, counter)

	err, elapsed := runTimed(t, handler, httptest.NewRequest(http.MethodGet, "/warm", nil))
	require.NoError(t, err)

	assert.GreaterOrEqual(t, elapsed, 40*time.Millisecond, "the delay must still be applied")
	assert.Less(t, elapsed, time.Second, "a huge score must never sleep longer than MaxDelay")
}

func TestDelayCancelledRequestReturnsImmediately(t *testing.T) {
	counter := &staticCounter{score: 75}
	handler := newDelayHandler(t, Config{
		Threshold: 100,
		MaxDelay:  10 * time.Second,
	}, counter)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodGet, "/warm", nil).WithContext(ctx)
	err, elapsed := runTimed(t, handler, req)

	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, elapsed, 500*time.Millisecond, "a canceled request must not sit in the tarpit")
}

func TestNoDelayAtOrBelowHalfThreshold(t *testing.T) {
	for _, score := range []int{0, 50} {
		counter := &staticCounter{score: score}
		handler := newDelayHandler(t, Config{
			Threshold: 100,
			MaxDelay:  5 * time.Second,
		}, counter)

		err, elapsed := runTimed(t, handler, httptest.NewRequest(http.MethodGet, "/warm", nil))
		require.NoError(t, err)
		assert.Less(t, elapsed, 100*time.Millisecond, "score %d must not be delayed", score)
	}
}

func TestCustomDelayFuncIsClamped(t *testing.T) {
	counter := &staticCounter{score: 75}
	handler := newDelayHandler(t, Config{
		Threshold: 100,
		MaxDelay:  20 * time.Millisecond,
		DelayFunc: func(int) time.Duration { return 5 * time.Second },
	}, counter)

	err, elapsed := runTimed(t, handler, httptest.NewRequest(http.MethodGet, "/warm", nil))
	require.NoError(t, err)
	assert.GreaterOrEqual(t, elapsed, 15*time.Millisecond)
	assert.Less(t, elapsed, time.Second, "a custom DelayFunc must be clamped too")
}

func TestBlockedRequestIsNotDelayed(t *testing.T) {
	counter := &staticCounter{score: 100}
	handler := newDelayHandler(t, Config{
		Threshold: 100,
		MaxDelay:  5 * time.Second,
	}, counter)

	err, elapsed := runTimed(t, handler, httptest.NewRequest(http.MethodGet, "/warm", nil))

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	assert.Equal(t, http.StatusForbidden, he.Code)
	assert.Less(t, elapsed, 500*time.Millisecond, "blocked requests must be rejected before any delay")
}

func TestJitterStaysWithinBounds(t *testing.T) {
	config := Config{
		Threshold: 100,
		MaxDelay:  100 * time.Millisecond,
		DelayFunc: func(int) time.Duration { return 100 * time.Millisecond },
		Jitter:    true,
	}

	for i := 0; i < 100; i++ {
		delay := config.delayFor(75)
		assert.LessOrEqual(t, delay, config.MaxDelay)
		assert.GreaterOrEqual(t, delay, 70*time.Millisecond, "jitter must stay around +/-20%%")
	}
}

func TestDelayForZeroOrNegative(t *testing.T) {
	config := Config{
		MaxDelay:  time.Second,
		DelayFunc: func(int) time.Duration { return -time.Second },
	}
	assert.Zero(t, config.delayFor(75))
}
