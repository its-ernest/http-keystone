# http-keystone Steady Middleware

A **high-performance concurrency limiter** for [Echo](https://echo.labstack.com/) v5. 

[→ View Full API Documentation](DOCS.md)

`steady` ensures your server remains stable under heavy load by limiting the number of requests processed simultaneously. Instead of immediately rejecting excess traffic, it allows requests to wait in a queue for a configurable amount of time before returning a "Service Unavailable" response.

## Features

* **Concurrency Control:** Strictly limit the number of active goroutines handling requests.
* **Smart Queueing:** Requests wait for a slot instead of being dropped immediately.
* **Configurable Timeouts:** Define how long a request should wait before giving up.
* **Custom Error Handling:** Fully customize the response sent when the limit is exceeded.
* **Context Awareness:** Automatically stops waiting if the client cancels the request.
* **Echo v5 Ready:** Designed specifically for the latest Echo *pointer-based* context.

---

## Installation

```bash
go get github.com/its-ernest/http-keystone@latest
```

Then import `github.com/its-ernest/http-keystone/steady`.

---

## Basic Usage

```go
package main

import (
	"net/http"
	"time"

	"github.com/its-ernest/http-keystone/steady"
	"github.com/labstack/echo/v5"
)

func main() {
	e := echo.New()

	// Apply steady middleware with custom limits
	e.Use(steady.New(steady.Config{
		MaxConcurrent: 50,               // Allow 50 simultaneous requests
		WaitTimeout:   10 * time.Second, // Wait up to 10s for a slot
	}))

	e.GET("/", func(c *echo.Context) error {
		return c.String(http.StatusOK, "Stable and Steady!")
	})

	e.Start(":8080")
}
```

---

## Configuration Options

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `MaxConcurrent` | `int` | `100` | Maximum number of requests to process at once. |
| `MaxWaiting` | `int` | `100` | Maximum number of requests that may queue. `0` uses the default; a negative value means unlimited. |
| `WaitTimeout` | `time.Duration` | `10s` | How long a request will wait in the queue for a slot. |
| `RetryAfter` | `time.Duration` | `1s` | Value (in whole seconds, rounded up, min `1`) the default rejection handlers send as `Retry-After`. Independent of `WaitTimeout` so a short wait never yields `Retry-After: 0`. |
| `Skipper` | `middleware.Skipper` | `DefaultSkipper` | Function to skip this middleware for certain routes. |
| `ErrorHandler` | `func(*echo.Context) error` | *Internal* | Called when a request times out in the queue (503 + `Retry-After`). A custom handler's own `Retry-After` is respected. |
| `QueueFullHandler` | `func(*echo.Context) error` | *Internal* | Called when `MaxWaiting` is already reached. Default is 503 + `Retry-After`, wrapping `ErrQueueFull`. |
| `Stats` | `*Stats` | `nil` | Optional live counters: `InFlight`, `Waiting`, `Rejected`. |

---

## How it Works

1. **Slot Acquisition:** When a request arrives, the middleware tries to acquire a "slot" from an internal semaphore.
2. **Immediate Execution:** If a slot is available, the request proceeds immediately.
3. **Bounded Queueing:** If the server is at `MaxConcurrent` capacity, the request
   joins the waiting queue — but only if fewer than `MaxWaiting` requests are
   already queued. The queue is bounded, so a burst cannot accumulate an
   unlimited number of goroutines, connections and request bodies.
4. **Queue Full:** Requests beyond `MaxWaiting` are rejected *immediately* by
   `QueueFullHandler` (default `503 Service Unavailable` with `Retry-After`),
   wrapping `steady.ErrQueueFull`. No goroutine is parked.
5. **Waiting:** A queued request waits until:
    - A slot becomes free (as another request finishes).
    - The `WaitTimeout` is reached.
    - The client closes the connection (context cancellation), which frees its
      queue slot immediately.
6. **Timeout:** If `WaitTimeout` is reached before a slot is freed, `ErrorHandler`
   is triggered (default `503 Service Unavailable` with a `Retry-After` header).
   The `Retry-After` value comes from `RetryAfter` (default `1s`), rounded up to a
   whole second and never less than `1`, so a short `WaitTimeout` never produces
   `Retry-After: 0`.

```go
stats := &steady.Stats{}

e.Use(steady.New(steady.Config{
	MaxConcurrent: 100,
	MaxWaiting:    200, // at most 200 requests queue; the rest get 503
	Stats:         stats,
}))

// Later, from a metrics handler:
// stats.InFlight.Load(), stats.Waiting.Load(), stats.Rejected.Load()
```

Queue rejections wrap `steady.ErrQueueFull`, so a custom handler can label them,
and callers/tests can distinguish them from a `WaitTimeout` rejection:

```go
QueueFullHandler: func(c *echo.Context) error {
	c.Response().Header().Set("Retry-After", "2")
	return echo.NewHTTPError(http.StatusServiceUnavailable, "queue is full").
		Wrap(steady.ErrQueueFull)
},

// errors.Is(err, steady.ErrQueueFull) == true for queue-full rejections
```

---

## Why use Steady?

Standard rate limiters often focus on requests-per-second (RPS). However, some requests are "heavier" than others (e.g., intensive DB queries or image processing). `steady` focuses on **concurrency**, ensuring that no matter how slow your handlers are, you never exceed the resource limits (CPU/Memory) of your server.
