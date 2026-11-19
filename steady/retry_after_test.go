package steady

import (
	"net/http"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRetryAfterNeverZero asserts that a very short WaitTimeout still produces
// a Retry-After of at least one second, never "0" (which would tell clients to
// retry immediately).
func TestRetryAfterNeverZero(t *testing.T) {
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
	require.Error(t, err)
	assert.Equal(t, "1", rec2.Header().Get("Retry-After"))

	close(release)
}

// TestRetryAfterConfigurable asserts that RetryAfter drives the header,
// rounded up to whole seconds.
func TestRetryAfterConfigurable(t *testing.T) {
	e := echo.New()
	release := make(chan struct{})

	handler := New(Config{
		MaxConcurrent: 1,
		MaxWaiting:    4,
		WaitTimeout:   50 * time.Millisecond,
		RetryAfter:    3 * time.Second,
	})(func(c *echo.Context) error {
		<-release
		return c.String(http.StatusOK, "ok")
	})

	c1, _ := newSteadyContext(e, nil)
	go func() { _ = handler(c1) }()
	time.Sleep(20 * time.Millisecond)

	c2, rec2 := newSteadyContext(e, nil)
	_ = handler(c2)
	assert.Equal(t, "3", rec2.Header().Get("Retry-After"))

	close(release)
}

// TestRetryAfterRoundedUp asserts that a fractional RetryAfter rounds up.
func TestRetryAfterRoundedUp(t *testing.T) {
	e := echo.New()
	release := make(chan struct{})

	handler := New(Config{
		MaxConcurrent: 1,
		MaxWaiting:    4,
		WaitTimeout:   50 * time.Millisecond,
		RetryAfter:    1500 * time.Millisecond,
	})(func(c *echo.Context) error {
		<-release
		return c.String(http.StatusOK, "ok")
	})

	c1, _ := newSteadyContext(e, nil)
	go func() { _ = handler(c1) }()
	time.Sleep(20 * time.Millisecond)

	c2, rec2 := newSteadyContext(e, nil)
	_ = handler(c2)
	assert.Equal(t, "2", rec2.Header().Get("Retry-After"))

	close(release)
}

// TestCustomErrorHandlerRetryAfterRespected asserts that a custom ErrorHandler
// that sets its own Retry-After is not overwritten by the default.
func TestCustomErrorHandlerRetryAfterRespected(t *testing.T) {
	e := echo.New()
	release := make(chan struct{})

	handler := New(Config{
		MaxConcurrent: 1,
		MaxWaiting:    4,
		WaitTimeout:   50 * time.Millisecond,
		ErrorHandler: func(c *echo.Context) error {
			c.Response().Header().Set("Retry-After", "42")
			return echo.NewHTTPError(http.StatusServiceUnavailable, "custom")
		},
	})(func(c *echo.Context) error {
		<-release
		return c.String(http.StatusOK, "ok")
	})

	c1, _ := newSteadyContext(e, nil)
	go func() { _ = handler(c1) }()
	time.Sleep(20 * time.Millisecond)

	c2, rec2 := newSteadyContext(e, nil)
	_ = handler(c2)
	assert.Equal(t, "42", rec2.Header().Get("Retry-After"), "a custom Retry-After must be respected")

	close(release)
}

// TestRetryAfterHeaderHelper covers the rounding/clamping helper directly.
func TestRetryAfterHeaderHelper(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "1"},
		{-time.Second, "1"},
		{50 * time.Millisecond, "1"},
		{time.Second, "1"},
		{1500 * time.Millisecond, "2"},
		{3 * time.Second, "3"},
		{10 * time.Second, "10"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, retryAfterHeader(tc.in), "retryAfterHeader(%v)", tc.in)
	}
}
