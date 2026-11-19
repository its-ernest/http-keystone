# http-keystone Idempotency Middleware

A **safe-retry middleware** for [Echo](https://echo.labstack.com/) v5.

[→ View Full API Documentation](DOCS.md)

`idempotency` makes unsafe requests (`POST`, `PUT`, `PATCH`, `DELETE`) safe to retry. The first request that carries an `Idempotency-Key` runs the handler and stores its response; any later request with the same key replays that response instead of running the handler again. This is the behaviour behind the IETF idempotency-key draft and payment APIs such as Stripe's.

## Features

- **Safe retries:** a retried request returns the original response byte-for-byte instead of running the handler twice.
- **Single execution under concurrency:** a lock (owned, expiring) guarantees only one request per key runs at a time — across a single instance (`MemoryStore`) or a whole cluster (`RedisStore`).
- **Request fingerprinting:** reusing a key with a different method, path, or body is rejected (`ErrFingerprintMismatch`), never replayed with the wrong response.
- **Flexible in-flight policy:** reject duplicates immediately (`409`) or wait and replay.
- **Bounded storage:** a response size cap and a status allow-list keep only what is safe to replay (never `5xx`).
- **Echo v5 ready:** designed for the latest pointer-based `*echo.Context`.

---

## Installation

```bash
go get github.com/its-ernest/http-keystone@latest
```

Then import `github.com/its-ernest/http-keystone/idempotency`.

---

## Basic Usage

```go
package main

import (
	"net/http"

	"github.com/its-ernest/http-keystone/idempotency"
	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
)

func main() {
	e := echo.New()

	e.Use(idempotency.New(idempotency.Config{
		Store: store.NewMemoryStore(),
		TTL:   24 * time.Hour,
	}))

	e.POST("/payments", func(c *echo.Context) error {
		// This handler runs once per Idempotency-Key, no matter how many
		// times the client retries.
		return c.String(http.StatusCreated, "payment created")
	})

	e.Start(":8080")
}
```

A client retries safely by sending the same key:

```http
POST /payments HTTP/1.1
Idempotency-Key: 8f1c2b7e-...

{"amount": 100}
```

---

## Configuration Options

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `Store` | `store.Store` | *(required)* | Persists the fingerprint and the stored response; also provides the owned lock. |
| `TTL` | `time.Duration` | `24h` | How long a completed response is remembered. |
| `Skipper` | `middleware.Skipper` | `DefaultSkipper` | Skip the middleware for selected routes. |
| `Header` | `string` | `Idempotency-Key` | Request header that carries the key. |
| `Required` | `bool` | `false` | Reject requests with no key (`400`) instead of passing through. |
| `KeyFunc` | `func(*echo.Context) string` | `nil` | Derive the key (e.g. scope per route/tenant); overrides `Header`. |
| `MaxBodySize` | `int` | `1MB` | Responses larger than this are streamed and not stored. |
| `InFlight` | `InFlightPolicy` | `InFlightReject` | `InFlightReject` (409) or `InFlightWait` (wait and replay). |
| `WaitTimeout` | `time.Duration` | `10s` | How long an `InFlightWait` duplicate waits. |
| `RetryDelay` / `MaxRetries` | `duration` / `int` | `20ms` / `10` | Poll cadence while waiting. |
| `LockTTL` | `time.Duration` | `30s` | Upper bound on the in-flight lock. |
| `StoreStatusCodes` | `[]int` | `200,201,202,204,400,401,403,404,409,422,429` | Statuses that are stored (never `5xx` by default). |
| `ErrorHandler` | `func(*echo.Context, error) error` | *renders error* | Render the rejection response. |
| `FailClosed` | `bool` | `false` | Reject on store failure instead of failing open. |

---

## How it Works

1. **Method filter:** safe methods (`GET`, `HEAD`, `OPTIONS`, …) pass straight through.
2. **Key:** taken from `KeyFunc` or the configured header. Missing key passes through, or `400` when `Required`.
3. **Fingerprint:** `method` + request URI + a hash of the body identify the request.
4. **Replay:** if a stored response exists and the fingerprint matches, it is replayed (byte-for-byte, with `Idempotency-Replayed: true`). A fingerprint mismatch is rejected — the wrong response is never returned.
5. **Lock:** the request acquires an owned, expiring lock keyed by the idempotency key, so exactly one request runs at a time.
6. **Run & store:** the handler runs behind a recorder; its status, safe headers, and body are stored with the TTL, provided the status is in `StoreStatusCodes` and the body fits `MaxBodySize`.
7. **Concurrent duplicates:** rejected with `409` (`InFlightReject`) or parked until the first finishes and then replayed (`InFlightWait`).

> **Retries never run the handler twice.** A network blip after the server stored the response cannot cause a duplicate side effect — the retry replays the stored result.

---

## Why use Idempotency?

Retries are inevitable: clients time out, connections drop, and users double-click. Without idempotency, a retried `POST /payments` can charge a card twice. This middleware makes any unsafe endpoint safe to retry by remembering the outcome of the first request, keyed by a client-supplied `Idempotency-Key`.

See the runnable example in [`_examples/idempotency`](../_examples/idempotency).