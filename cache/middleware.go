package cache

import (
	"net/http"
	"time"

	"github.com/its-ernest/http-keystone/internal"
	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
)

// New returns an Echo middleware for caching HTTP responses.
//
// By default only anonymous, cacheable GET requests are stored, only 200
// responses are kept, and requests carrying Authorization or Cookie headers
// bypass the cache. See Config for the knobs that relax these defaults.
func New(config Config) echo.MiddlewareFunc {
	// Defaults & sanity checks
	if config.Store == nil {
		panic("http-keystone: cache middleware requires a store")
	}
	if config.TTL == 0 {
		config.TTL = 5 * time.Minute
	}
	if config.KeyGenerator == nil {
		config.KeyGenerator = DefaultKeyGenerator
	}
	if config.MaxBodySize == 0 {
		config.MaxBodySize = 1024 * 1024 // 1MB
	}
	if config.RetryDelay == 0 {
		config.RetryDelay = 20 * time.Millisecond
	}
	if config.MaxRetries == 0 {
		config.MaxRetries = 10
	}
	if config.LockTTL == 0 {
		config.LockTTL = 10 * time.Second
	}
	if config.CacheableStatusCodes == nil {
		config.CacheableStatusCodes = []int{http.StatusOK}
	}
	if config.VaryHeaders == nil {
		config.VaryHeaders = []string{"Accept-Encoding"}
	}

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if config.Skipper != nil && config.Skipper(c) {
				return next(c)
			}
			if c.Request().Method != http.MethodGet {
				return next(c)
			}
			if !requestCacheable(c, config) {
				return next(c)
			}

			key := cacheKey(c, config)
			lockKey := key + ":lock"
			ctx := c.Request().Context()

			entry, err := config.Store.Get(ctx, key)
			if err == nil && entry != nil {
				return replay(c, entry, config)
			}

			locked, unlockFn := acquireLockWithTTL(ctx, config.Store, lockKey, config.LockTTL)
			if !locked {
				// Another request is populating this key. Wait for its result
				// instead of running the handler and creating a stampede.
				entry, ok, err := waitForEntry(ctx, config, key)
				if err != nil {
					return err
				}
				if ok {
					return replay(c, entry, config)
				}
				// The retry budget was exhausted without an entry (the other
				// request failed, streamed, or was not cacheable). Fail open:
				// serve the request from the origin and let a later request
				// populate the cache.
				return next(c)
			}
			defer unlockFn()

			// Double-check: another request may have populated the entry
			// between our miss and the lock acquisition.
			if entry, err := config.Store.Get(ctx, key); err == nil && entry != nil {
				return replay(c, entry, config)
			}

			originalWriter := c.Response()
			recorder := internal.NewResponseRecorder(originalWriter, config.MaxBodySize)
			recorder.BeforeWriteHeader = func(h http.Header) {
				if h.Get("X-Cache") == "" {
					if recorder.Passthrough() {
						h.Set("X-Cache", "BYPASS")
					} else {
						h.Set("X-Cache", "MISS")
					}
				}
			}
			c.SetResponse(recorder)
			defer c.SetResponse(originalWriter)

			if err := next(c); err != nil {
				// Echo should render the error response; never cache a partial
				// or failed response.
				recorder.Discard()
				return err
			}

			// The response overran MaxBodySize or was flushed by the handler.
			// It has already been streamed to the client and must not be cached.
			if recorder.Passthrough() {
				return nil
			}

			// The policy runs against the raw handler headers so it can still
			// see Set-Cookie and Cache-Control before they are sanitized.
			candidate := &store.Entry{
				Status: recorder.Status(),
				Header: http.Header(recorder.Header()).Clone(),
				Body:   recorder.Body(),
			}

			saved := false
			if responseCacheable(c, candidate, config) {
				stored := sanitizeHeader(candidate.Header)
				ensureVary(stored, config.VaryHeaders)
				stored.Set("ETag", etagFor(candidate.Body, config.WeakETag))

				candidate.Header = stored
				if err := config.Store.Save(ctx, key, candidate, config.TTL); err == nil {
					saved = true
					// Serve exactly what was stored so a MISS and a later HIT
					// cannot disagree on headers.
					replaceHeader(recorder.Header(), stored)
				}
			}

			if saved {
				recorder.Header().Set("X-Cache", "MISS")
			} else {
				recorder.Header().Set("X-Cache", "BYPASS")
			}

			return recorder.Commit()
		}
	}
}

// replaceHeader overwrites dst with the contents of src.
func replaceHeader(dst http.Header, src http.Header) {
	for name := range dst {
		delete(dst, name)
	}
	for name, values := range src {
		dst[name] = append([]string(nil), values...)
	}
}
