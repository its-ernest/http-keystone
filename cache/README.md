# http-keystone Cache Middleware

A **high-performance, production-ready caching middleware** for [Echo](https://echo.labstack.com/) in Go.
[→ View Full API Documentation](DOCS.md)

## Features

* **Concurrent-safe:** Uses `sync.Map` for high-concurrency environments.
* **Cache stampede prevention:** Only one request computes the cache per key; others wait or retry.
* **ETag & Not-Modified:** RFC 9110 entity-tags with quoted, list, weak, and `*` `If-None-Match` support.
* **Privacy-safe defaults:** Authenticated requests, cookies, and non-public responses are never stored unless you opt in.
* **Vary-aware keys:** Responses that vary on `Accept-Encoding` (or headers you configure) get separate cache entries.
* **Configurable:** TTL, max body size, retry behavior, skip functions, key generators.
* **Automatic cleanup:** Periodic eviction of expired entries prevents memory bloat.
* **Production-ready logging:** Tracks hits, misses, and cache events.

---

## Installation

```bash
go get github.com/its-ernest/http-keystone@latest
```

Then import `github.com/its-ernest/http-keystone/cache`.

---

## Basic Usage

```go
package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/its-ernest/http-keystone/cache" 
	"github.com/its-ernest/http-keystone/store"
)

func main() {
	e := echo.New()

	// SIMPLE CUSTOM LOGGER
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			start := time.Now()
			err := next(c)
			stop := time.Now()
			fmt.Printf("[%s] %d %s %s (%s)\n", 
				stop.Format("15:04:05"),
				c.Request().Method, 
				c.Request().URL.Path,
				stop.Sub(start),
			)
			return err
		}
	})

	// Setup local store
	memStore := store.NewMemoryStore()

	// Apply cache middleware
	e.Use(cache.New(cache.Config{
		Store: memStore,
		TTL:   20 * time.Second,
	}))

	// Echo V5 Handler
	e.GET("/test", func(c *echo.Context) error {
		fmt.Println(" [HANDLER] Generating fresh content...")
		timestamp := time.Now().Format(time.RFC3339Nano)
		return c.String(http.StatusOK, fmt.Sprintf("Generated at: %s", timestamp))
	})

	// Start
	fmt.Println("Starting Dev Server on :8080...")
	if err := e.Start(":8080"); err != nil {
		fmt.Printf("Server stopped: %v\n", err)
	}
}
```

---

## Configuration Options

| Field                  | Type                          | Default                | Description                                        |
| ---------------------- | ----------------------------- | ---------------------- | -------------------------------------------------- |
| `Store`                | `store.Store`                 | **Required**           | Backend cache store (`MemoryStore`, Redis, custom). |
| `TTL`                  | `time.Duration`               | `5 * time.Minute`      | Time-to-live for cached entries.                   |
| `Skipper`              | `func(*echo.Context) bool`    | `nil`                  | Function to skip caching for specific requests.    |
| `KeyGenerator`         | `func(*echo.Context) string`  | `DefaultKeyGenerator`  | Base cache key. `VaryHeaders` and credentials are appended automatically. |
| `MaxBodySize`          | `int`                         | 1MB                    | Maximum bytes buffered and cached; larger responses stream through uncached. |
| `WeakETag`             | `bool`                        | `false`                | Emit `W/"..."` validators instead of strong ETags. |
| `RetryDelay`           | `time.Duration`               | 20ms                   | Initial delay for a request waiting on another request populating the same key. Grows linearly with up to ±25% jitter. |
| `MaxRetries`           | `int`                         | 10                     | Maximum number of polls while waiting for a cold key to be populated. |
| `LockTTL`              | `time.Duration`               | 10s                    | How long the population lock is held before another request may take over. |
| `CacheAuthenticated`   | `bool`                        | `false`                | Store responses for requests with `Authorization`/`Cookie`, keyed per credential. |
| `CacheableStatusCodes` | `[]int`                       | `[200]`                | Response statuses that may be stored.              |
| `VaryHeaders`          | `[]string`                    | `["Accept-Encoding"]`  | Request headers hashed into the key and advertised in `Vary`. |
| `ShouldCache`          | `func(*echo.Context, *store.Entry) bool` | `nil`        | Extra veto after the built-in safety checks.       |

---

## Cache safety and privacy

The defaults are deliberately conservative, because caching a response for the
wrong client is a data leak:

* **Authenticated requests bypass the cache.** A request with `Authorization`,
  `Proxy-Authorization`, or `Cookie` is never served from cache and never
  stored. Set `CacheAuthenticated: true` to opt in; credentials are then hashed
  into the cache key, so different users never share an entry.
* **Only 200 by default.** Widen `CacheableStatusCodes` if you want to cache
  redirects or 404s.
* **Responses that must stay per-client are never stored:** any `Set-Cookie`, or
  `Cache-Control: private`, `no-store`, or `no-cache` marks the response as
  `X-Cache: BYPASS`.
* **`Vary` is honored.** A response whose `Vary` names a header that is not in
  `VaryHeaders` is not cached, and `Vary: *` disables caching. Add headers such
  as `Accept-Language` to `VaryHeaders` when handlers vary on them.
* **Request directives are honored.** `Cache-Control: no-store`/`no-cache` and
  `Pragma: no-cache` bypass the cache.
* **Hop-by-hop headers and `Set-Cookie` are stripped** from stored entries and
  never replayed to another client. `Date`/`Age` are regenerated.
* **`ShouldCache`** receives the response entry after the built-in policy and
  can veto storage for application-specific reasons.

If a route depends on a logged-in user, prefer a stable user or tenant ID in
`KeyGenerator` and leave `CacheAuthenticated` off, rather than hashing raw
session cookies into the key space.

---

## Response size and streaming

The middleware buffers a response only up to `MaxBodySize`. If a handler writes
more than that, or calls `Flush` (SSE, long polling, websockets), the buffered
bytes are sent, the rest of the response is streamed directly to the client, and
nothing is stored. Streamed responses carry `X-Cache: BYPASS`.

This keeps memory bounded by `MaxBodySize` per in-flight cacheable request
instead of by the size of whatever the handler produces.

---

## Conditional requests (ETag / If-None-Match)

Every stored response gets a quoted entity-tag derived from its body:
`"<sha256>"` by default, or `W/"<sha256>"` when `WeakETag` is enabled. The same
ETag is sent on the first `MISS` and on later `HIT`s.

On a hit, `If-None-Match` is parsed per RFC 9110:

- `"etag"` matches
- `W/"etag"` matches too (If-None-Match uses weak comparison)
- `"other", "etag"` — any member of the list matches
- multiple `If-None-Match` header lines are combined
- `*` matches any stored representation

A match returns `304 Not Modified` with no body but with `ETag`, `Vary`, and any
stored `Cache-Control`/`Expires`/`Last-Modified`/`Content-Location`, so clients
and intermediaries can refresh their metadata. The middleware does not invent
`Cache-Control: max-age`; it preserves whatever the handler set.

---

## Middleware Behavior

1. **Request policy:** Non-`GET` requests, `Skipper` matches, credential-bearing requests (unless `CacheAuthenticated`), and `no-store`/`no-cache` requests go straight to the handler.
2. **Cache Check:** The key is the base key plus a digest of the configured `Vary` headers (and credentials, when enabled).
3. **Cache Hit:** Returns cached response with `X-Cache: HIT`. ETag validated; returns 304 if client has latest version.
4. **Cache Miss:** Acquires lock for this key, re-checks the store, then runs the handler. Only one request populates a cold key.
5. **Other Concurrent Requests:** Poll the store with `RetryDelay` and linear backoff (up to `MaxRetries`) and replay the populated entry as `X-Cache: HIT`. If the budget expires, they fail open to the origin handler. Cancellation returns promptly.
6. **Response policy:** Status, `Set-Cookie`, `Cache-Control`, `Vary`, and `ShouldCache` are checked before storing.
7. **Cache Save:** Stores a sanitized entry with TTL and an ETag, then serves the same headers with `X-Cache: MISS`.
8. **Eviction:** Expired items removed automatically by the background evictor.

---

## `MemoryStore` Advanced Features

* **TTL-aware locks:** Locks expire automatically to prevent deadlocks.
* **Background eviction:** Periodic cleanup of expired cache entries.
* **Concurrent-safe operations:** All methods (`Get`, `Save`, `Delete`, `Lock`, `Unlock`) are goroutine-safe.
* **Optional singleflight / GetOrCompute** can be added for fully atomic cache population.

---

## HTTP Headers

* `X-Cache`: `HIT`, `MISS`, or `BYPASS` (not stored)
* `Etag`: quoted SHA256 hash of response body (`W/"..."` with `WeakETag`)
* `Vary`: advertises every configured `VaryHeaders` entry
* `Content-Type`: Respected from handler or auto-detected

---

## Logging

* `Cache HIT` / `Cache MISS` events logged via `log.Printf`.
* Fallbacks due to lock contention are logged.
* Can be replaced with a structured logger in production.

---

## Recommended practices:

* **Memory Usage:** `MemoryStore` is in-memory; consider limits for very large payloads.
* **Concurrency:** Tested with high concurrent GET requests; stampede prevention enabled.
* **Persistence:** [For multi-server deployments, replace `MemoryStore` with a distributed store (Redis, Memcached)](cache/redis.md).
* **TTL Enforcement:** Lock TTL must match cache TTL strategy to prevent stale locks.
* **Evictor Interval:** Adjust based on traffic, memory footprint, and TTLs.
