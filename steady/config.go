package steady

import (
	"errors"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
)

// ErrQueueFull is wrapped by the default QueueFullHandler when MaxWaiting is
// exceeded. Use errors.Is(err, steady.ErrQueueFull) to distinguish an immediate
// queue rejection from a WaitTimeout error in a custom handler.
var ErrQueueFull = errors.New("steady: waiting queue is full")

// Stats exposes live counters for one middleware instance. All fields are safe
// for concurrent use. Pass a value in Config.Stats to observe a running
// middleware; a zero value is ready to use.
type Stats struct {
	// InFlight is the number of requests currently holding a slot.
	InFlight atomic.Int64
	// Waiting is the number of requests queued for a slot.
	Waiting atomic.Int64
	// Rejected counts queue-full and wait-timeout rejections.
	Rejected atomic.Int64
}

// Config defines the configuration for the steady middleware.
type Config struct {
	// Skipper defines a function to skip middleware.
	Skipper middleware.Skipper

	// MaxConcurrent is the maximum number of concurrent requests allowed.
	// Default is 100.
	MaxConcurrent int

	// MaxWaiting is the maximum number of requests that may queue for a slot.
	// Requests arriving when the queue is full are rejected immediately with
	// QueueFullHandler instead of parking another goroutine.
	// Default is 100. A negative value means unlimited.
	MaxWaiting int

	// WaitTimeout is the maximum time a request will wait for a processing slot.
	// Default is 10 seconds.
	WaitTimeout time.Duration

	// RetryAfter is how long the default rejection handlers tell clients to
	// wait before retrying, via the Retry-After header. It is deliberately
	// independent of WaitTimeout: deriving it from a short WaitTimeout yields
	// "Retry-After: 0", which tells clients to retry immediately and invites a
	// thundering herd. The emitted value is max(1, ceil(RetryAfter seconds)),
	// so it is never less than one second. Default is 1 second.
	RetryAfter time.Duration

	// ErrorHandler is called when a request times out waiting for a slot.
	// Default returns 503 Service Unavailable with a Retry-After header.
	// A custom ErrorHandler owns its own Retry-After; the middleware never
	// overwrites a value it sets.
	ErrorHandler func(c *echo.Context) error

	// QueueFullHandler is called when MaxWaiting is already reached.
	// Default returns 503 Service Unavailable with Retry-After and wraps
	// ErrQueueFull. If nil, ErrorHandler is used.
	QueueFullHandler func(c *echo.Context) error

	// Stats optionally receives live counters for this middleware instance.
	Stats *Stats
}

// DefaultConfig is the default configuration for the steady middleware.
var DefaultConfig = Config{
	Skipper:       middleware.DefaultSkipper,
	MaxConcurrent: 100,
	MaxWaiting:    100,
	WaitTimeout:   10 * time.Second,
	RetryAfter:    time.Second,
}
