package steady

import (
	"context"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v5"
)

// New returns a steady middleware that limits concurrent requests.
func New(config Config) echo.MiddlewareFunc {
	// Defaults
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = DefaultConfig.MaxConcurrent
	}
	if config.MaxWaiting == 0 {
		config.MaxWaiting = DefaultConfig.MaxWaiting
	}
	if config.WaitTimeout <= 0 {
		config.WaitTimeout = DefaultConfig.WaitTimeout
	}
	if config.Skipper == nil {
		config.Skipper = DefaultConfig.Skipper
	}
	if config.ErrorHandler == nil {
		config.ErrorHandler = func(c *echo.Context) error {
			c.Response().Header().Set("Retry-After", strconv.Itoa(int(config.WaitTimeout.Seconds())))
			return echo.NewHTTPError(http.StatusServiceUnavailable, "Server is reaching capacity. Please try again shortly.")
		}
	}
	if config.QueueFullHandler == nil {
		config.QueueFullHandler = func(c *echo.Context) error {
			c.Response().Header().Set("Retry-After", strconv.Itoa(int(config.WaitTimeout.Seconds())))
			return echo.NewHTTPError(http.StatusServiceUnavailable, "Server is at capacity. Please try again shortly.").
				Wrap(ErrQueueFull)
		}
	}

	stats := config.Stats
	if stats == nil {
		stats = &Stats{}
	}

	// The Semaphore
	sem := make(chan struct{}, config.MaxConcurrent)

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if config.Skipper(c) {
				return next(c)
			}

			// Reserve a place in the queue, but never block: a request that
			// does not fit is rejected immediately instead of parking another
			// goroutine (and connection, and body) for WaitTimeout.
			waiting := stats.Waiting.Add(1)
			if config.MaxWaiting > 0 && waiting > int64(config.MaxWaiting) {
				stats.Waiting.Add(-1)
				stats.Rejected.Add(1)
				return config.QueueFullHandler(c)
			}

			// Create a timeout context for the wait period
			ctx, cancel := context.WithTimeout(c.Request().Context(), config.WaitTimeout)
			defer cancel()

			select {
			case sem <- struct{}{}:
				// Acquired a slot!
				stats.Waiting.Add(-1)
				stats.InFlight.Add(1)
				defer func() {
					stats.InFlight.Add(-1)
					<-sem
				}() // Release slot when done
				return next(c)

			case <-ctx.Done():
				stats.Waiting.Add(-1)
				// We waited too long or the client disconnected
				if ctx.Err() == context.DeadlineExceeded {
					stats.Rejected.Add(1)
					return config.ErrorHandler(c)
				}
				// Client disconnected, just return the context error
				return ctx.Err()
			}
		}
	}
}
