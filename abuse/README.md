# http-keystone Abuse Middleware

[![Docs](https://img.shields.io/badge/docs-api_reference-blue?style=flat-square)](./DOCS.md)
[![Status](https://img.shields.io/badge/status-production--beta-cyan?style=flat-square)](#)

A **smart behavioral firewall** for [Echo v5](https://echo.labstack.com/). `http-keystone/abuse` tracks client reputation using a **Heat** scoring system to identify and neutralize scrapers, scanners, and malicious actors.

[**→ View API eference**](./DOCS.md)

---

## How It Works:

Unlike standard rate-limiters that reset every minute, think of this like a **heat** meter. While normal limits reset quickly, this middleware remembers a user's actions over time. Every bad request adds points that only disappear after the user stops for a while.

1.  **Passive Monitoring:** Every request is checked against a list of `Rules`.
2.  **Penalty Scoring:** If a client hits a sensitive path (e.g., `/.env` or `/phpmyadmin`), their "Heat" score increases.
3.  **Tarpit delay (Optional):** As the score approaches the threshold, the middleware artificially delays responses, wasting the attacker's time and resources.
4.  **The Block:** Once the `Threshold` is met, all further requests are rejected with a `403 Forbidden` until the `Cooldown` period expires.


---

## Installation

```bash
go get github.com/its-ernest/http-keystone@latest
```

Then import `github.com/its-ernest/http-keystone/abuse`.

---

## Basic Usage

```go
package main

import (
	"time"
	"github.com/labstack/echo/v5"
	"github.com/its-ernest/http-keystone/abuse"
	"github.com/its-ernest/http-keystone/store"
)

func main() {
	e := echo.New()

	// 1. Setup a Counter store (separate from Cache store for performance)
	abuseStore := store.NewMemoryCounter()

	// 2. Configure the Firewall
	e.Use(abuse.New(abuse.Config{
		Store:     abuseStore,
		Threshold: 100,        // Max heat before a total ban
		Cooldown:  1 * time.Hour, // Until forgetting about a ban
		EnableDelay: true,     // Slow down warm IPs
		Rules: []abuse.Rule{
			{Path: "/.env", Score: 100},       // Instant ban
			{Path: "/.git*", Score: 100},      // Instant ban
			{Path: "/wp-admin*", Score: 50},    // Two hits and the IP is banned
			{Path: "/api/v1/login", Method: "POST", Score: 5},  // Penalize brute-force attempts
		},
	}))

	e.Start(":8080")
}
```

---

## Memory lifecycle

`store.NewMemoryCounter()` keeps one entry per client key until it expires.
Expired keys are removed lazily on access. For long-running processes, start
the background evictor and bound the number of tracked clients:

```go
abuseStore := store.NewMemoryCounterWithLimit(100_000) // bound memory
abuseStore.StartEvictor(ctx, time.Minute)             // drop expired keys
defer abuseStore.Close()                              // stop the evictor
```

When the limit is reached, the key that expires first is evicted, so active
offenders keep their score while stale clients are dropped. `Close` only stops
the evictor; the counter stays usable.

## Distributed tracking with Redis

`store.RedisCounter` implements `store.Counter` and stores scores in Redis, so
every application instance sees the same heat for a client. `Increment` runs
`INCRBY` and `PEXPIRE` in one Lua script, making each update atomic: no lost
increments and no counter left without a cooldown TTL.

```go
redisCounter := store.NewRedisCounter("localhost:6379")
defer redisCounter.Close()

e.Use(abuse.New(abuse.Config{
	Store:     redisCounter,
	Threshold: 100,
	Cooldown:  time.Hour,
	Rules: []abuse.Rule{
		{Path: "/.env", Score: 100},
	},
}))
```

Use `store.NewRedisCounterWithOptions(store.RedisStoreOptions{...})` to set a
password, database, or pool sizes, or `store.NewRedisCounterWithClient(client)`
to share one `*redis.Client` with `store.NewRedisStore`. A counter created with
a borrowed client does not close it.

Keys are written exactly as the middleware builds them (for example
`abuse:heat:<client-ip>`), so services sharing one Redis instance must use
compatible rules and thresholds.

---

## Configuration Options

| Field | Type | Default | Description |
| :--- | :--- | :--- | :--- |
| `Store` | `store.Counter` | **Required** | Atomic counter backend (Memory or Redis). |
| `Threshold` | `int` | `100` | The score at which a client is fully blocked. `<= 0` uses the default. |
| `Rules` | `[]Rule` | `nil` | List of paths and their associated penalty scores. Copied by `New`. |
| `Cooldown` | `time.Duration` | `30m` | Time before a client's heat starts to reset. `<= 0` uses the default. |
| `EnableDelay` | `bool` | `false` | If true, delays warm clients instead of only blocking them. |
| `MaxDelay` | `time.Duration` | `2s` | Hard upper bound on the tarpit delay. `<= 0` uses the default. |
| `DelayFunc` | `func(int) time.Duration` | `score * 10ms` | Custom delay curve; the result is clamped to `MaxDelay`. `<= 0` means no delay. |
| `Jitter` | `bool` | `false` | Adds ±20% randomness so delayed clients do not retry in lockstep. |
| `ErrorHandler`| `func(*echo.Context, int)` | `Default` | Custom logic for when a user is blocked. |

> **Zero config is safe.** `abuse.New(Config{Store: counter})` uses
> `Threshold: 100`, `Cooldown: 30m`, and the default error handler, so it will
> not reject every request. Rules are copied, so mutating your slice after
> `New` has no effect.

---

## delay effect

When `EnableDelay` is active, the middleware calculates a sleep duration based on the current score:
$$Delay = CurrentScore \times 10ms$$

If a user has **80 points** of heat, every request they make will be artificially slowed down by **800ms**. This makes your API an expensive and frustrating target for automated crawlers without affecting legitimate users.

The delay is:

- **Clamped** to `MaxDelay` (default `2s`), so a score of 10,000 costs at most
  2 seconds, not 100.
- **Cancellable**: the wait is a timer on the request context, so a client that
  disconnects (or a server shutting down) releases the goroutine immediately and
  the middleware returns `context.Canceled`.
- **Skipped for cold clients**: scores at or below half the threshold are not
  delayed.
- **Applied after the block check**: a request that is already over the
  threshold is rejected with `ErrorHandler` and never parked in the tarpit.

```go
abuse.New(abuse.Config{
	Store:       counter,
	Threshold:   100,
	EnableDelay: true,
	MaxDelay:    2 * time.Second, // hard cap
	Jitter:      true,            // ±20%
})
```

Replace the curve with `DelayFunc`, for example exponential:

```go
DelayFunc: func(score int) time.Duration {
	return time.Duration(score*score) * time.Millisecond // still clamped by MaxDelay
},
```

> A tarpit deliberately holds the connection and its goroutine for the delay
> duration. Size `MaxDelay` with that in mind.

---

## Recommended Practices

* **Order Matters:** Place this middleware **above** your Logger and **above** your Cache. There is no point in logging or caching a request from a known malicious actor.
* **Real IP:** Ensure your `echo.IPExtractor` is configured correctly if you are running behind Nginx, Cloudflare, or AWS ALB.
* **Multi-instance deployments:** Use `store.NewRedisCounter` (or share a `*redis.Client` with `NewRedisCounterWithClient`) so heat scores are consistent across replicas. `MemoryCounter` is per-process.
* **Gradual Penalties:** Use small scores (2-5) for general API endpoints to catch high scrapers, and large scores (50-100) for sensitive system files.
