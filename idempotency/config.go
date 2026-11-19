package idempotency

import (
	"net/http"
	"time"

	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
)

// InFlightPolicy controls what happens when a second request arrives with an
// idempotency key whose first request is still running.
type InFlightPolicy int

const (
	// InFlightReject (default) answers the duplicate immediately with
	// 409 Conflict. It is the cheapest policy and matches the common
	// "the first request is still being processed, retry later" behaviour.
	InFlightReject InFlightPolicy = iota

	// InFlightWait parks the duplicate until the first request finishes and
	// then replays its stored response. It is friendlier to clients but
	// holds a goroutine (and connection) for the duration of the wait.
	InFlightWait
)

// LockingStore is the store capability idempotency needs: it must read and
// write entries (store.Store) and provide owned locks (store.Locker). Both
// MemoryStore and RedisStore satisfy it.
type LockingStore interface {
	store.Store
	store.Locker
}

// Config defines the settings for the idempotency middleware.
type Config struct {
	// Store persists the fingerprint and the stored response. It must be a
	// LockingStore (a store.Store that also provides owned locks), because
	// idempotency relies on the lock to guarantee a single request runs at a
	// time per key.
	Store LockingStore

	// TTL is how long a completed response is remembered so a later retry
	// with the same key replays it instead of running the handler again.
	// Default is 24h.
	TTL time.Duration

	// Skipper defines a function to skip the middleware. Defaults to
	// middleware.DefaultSkipper.
	Skipper middleware.Skipper

	// Header is the request header that carries the idempotency key.
	// Default is "Idempotency-Key".
	Header string

	// Required rejects requests that carry no key (400) instead of passing
	// them through. Default is false.
	Required bool

	// KeyFunc derives the idempotency key. When set it overrides Header; use
	// it to scope keys per route or tenant (for example by prefixing the
	// authenticated API key).
	KeyFunc func(c *echo.Context) string

	// MaxBodySize caps how many response bytes are stored. A larger response
	// is streamed to the client and not stored, so a retry re-runs the
	// handler. Default is 1MB.
	MaxBodySize int

	// InFlight selects the duplicate-request policy. Default is InFlightReject.
	InFlight InFlightPolicy

	// WaitTimeout bounds how long an InFlightWait duplicate waits for the
	// first request before giving up. Default is 10s.
	WaitTimeout time.Duration

	// RetryDelay and MaxRetries control the poll cadence an InFlightWait
	// duplicate uses while waiting for the first request to finish.
	// Defaults are 20ms and 10 retries.
	RetryDelay time.Duration
	MaxRetries int

	// LockTTL bounds how long the in-flight lock is held before another
	// request may take it over. Default is 30s. Set it longer than your
	// slowest idempotency-protected handler.
	LockTTL time.Duration

	// StoreStatusCodes is the allow-list of response statuses that are stored.
	// 5xx responses are never stored by default so a retry can succeed after a
	// transient failure. Default is []int{200, 201, 202, 204, 400, 401, 403,
	// 404, 409, 422, 429}.
	StoreStatusCodes []int

	// ErrorHandler renders the error response for rejected requests (missing
	// key when Required, a fingerprint mismatch, or a concurrent duplicate
	// under InFlightReject). Default renders 400/409/422 with a short message.
	ErrorHandler func(c *echo.Context, err error) error

	// FailClosed reports whether a store failure (not a lock conflict) should
	// reject the request instead of failing open. Default is false (fail open:
	// run the handler without idempotency).
	FailClosed bool
}

// DefaultConfig provides the standard settings for idempotency middleware.
var DefaultConfig = Config{
	Header:      "Idempotency-Key",
	TTL:         24 * time.Hour,
	MaxBodySize: 1024 * 1024,
	InFlight:    InFlightReject,
	WaitTimeout: 10 * time.Second,
	RetryDelay:  20 * time.Millisecond,
	MaxRetries:  10,
	LockTTL:     30 * time.Second,
	StoreStatusCodes: []int{
		http.StatusOK,
		http.StatusCreated,
		http.StatusAccepted,
		http.StatusNoContent,
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusConflict,
		http.StatusUnprocessableEntity,
		http.StatusTooManyRequests,
	},
	ErrorHandler: func(c *echo.Context, err error) error {
		return err
	},
}
