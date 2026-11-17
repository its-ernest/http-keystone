package cache

import (
	"fmt"
	"time"

	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
)

// Config structure for http-keystone/cache middleware
type Config struct {
	Store store.Store
	TTL   time.Duration
	// Skipper allows requests to bypass the cache entirely.
	Skipper func(c *echo.Context) bool

	// KeyGenerator produces the base cache key. VaryHeaders are hashed into
	// the key automatically; a custom generator does not need to handle them.
	KeyGenerator func(c *echo.Context) string

	// MaxBodySize prevents caching of massive responses (default 1MB).
	// Responses larger than this are streamed to the client and not cached.
	MaxBodySize int

	// WeakETag emits weak validators (W/"...") instead of strong ones. Use it
	// for bodies whose bytes can change without a semantic change. Default is
	// false (strong ETags).
	WeakETag bool

	// RetryDelay and MaxRetries control what a request does while another
	// request is populating the same cold key. The waiter polls the store up to
	// MaxRetries times, starting at RetryDelay and growing linearly with jitter.
	// Defaults are 20ms and 10 retries (roughly a one-second budget).
	RetryDelay time.Duration
	MaxRetries int

	// LockTTL bounds how long the population lock is held before another
	// request may take it over. Default is 10s. Set it longer than your
	// slowest cacheable handler to avoid concurrent population.
	LockTTL time.Duration

	// CacheAuthenticated allows responses to requests carrying Authorization
	// or Cookie headers to be cached. Default is false.
	//
	// When enabled, Authorization and Cookie values are mixed into the cache
	// key automatically, so different users never share an entry. Prefer a
	// stable user/tenant identifier in KeyGenerator over raw session cookies
	// to keep the key space manageable.
	CacheAuthenticated bool

	// CacheableStatusCodes is the allow-list of response statuses that may be
	// stored. Default is []int{http.StatusOK}.
	CacheableStatusCodes []int

	// VaryHeaders lists request headers that are hashed into the cache key and
	// advertised in the response Vary header. Default is
	// []string{"Accept-Encoding"}.
	//
	// A response whose Vary header names a header that is not in this list is
	// not cached, because doing so could serve the wrong representation.
	// Add headers such as "Accept-Language" here when handlers vary on them.
	VaryHeaders []string

	// ShouldCache runs after the built-in safety policy (status allow-list,
	// Cache-Control, Set-Cookie, Vary) and can further restrict caching.
	ShouldCache func(c *echo.Context, entry *store.Entry) bool
}

// DefaultKeyGenerator generates a cache key based on method + URL.
// VaryHeaders are appended by the middleware, so this key is only the base.
func DefaultKeyGenerator(c *echo.Context) string {
	return fmt.Sprintf("cache:%s:%s", c.Request().Method, c.Request().URL.String())
}
