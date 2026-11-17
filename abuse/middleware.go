// Package abuse provides behavioral analysis middleware for Echo v5.
// It tracks client reputation via a scoring system to mitigate scrapers and scanners.
package abuse

import (
	"context"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
)

type (
	// Rule defines the "heat" points added for specific path patterns.
	// Path supports exact matches (e.g., "/.env") and a trailing "*" wildcard
	// on a path-segment boundary: "/admin*" matches "/admin" and
	// "/admin/config" but not "/administrator".
	Rule struct {
		// Path is the URL path to match. It supports exact matches and a
		// trailing "*" wildcard on a path-segment boundary; Echo's router is
		// not involved. Matching is case-sensitive and ignores query strings.
		Path string

		// Method is the HTTP method to match (GET, POST, etc.). Empty means match all.
		Method string

		// Score is the amount of *heat* to add when this rule matches
		Score int
	}

	// Config defines the settings for the abuse detection middleware.
	Config struct {
		// Skipper defines a function to skip middleware. Defaults to
		// middleware.DefaultSkipper.
		Skipper middleware.Skipper

		// KeyFunc derives the identity used to track heat. The returned string
		// is appended to "abuse:heat:" as the store key.
		//
		// The default is c.RealIP(). Note that c.RealIP() trusts
		// X-Forwarded-For / X-Real-IP when Echo has an IPExtractor configured,
		// and Echo's default extracts the header values without verification.
		// Behind a proxy, configure e.IPExtractor (see
		// echo.ExtractIPFromXFFHeader) or set KeyFunc to something the client
		// cannot choose. See NewForwardedIPKeyFunc for a ready-made helper.
		KeyFunc func(c *echo.Context) string

		Store store.Counter
		// Threshold is the heat score at which requests are rejected.
		// Values <= 0 fall back to DefaultConfig.Threshold (100) so a zero
		// Config can never block every request.
		Threshold int
		Rules     []Rule
		// Cooldown is how long heat is remembered. Values <= 0 fall back to
		// DefaultConfig.Cooldown (30 minutes).
		Cooldown time.Duration
		// EnableDelay adds latency to suspicious requests instead of a hard block.
		EnableDelay bool
		// MaxDelay caps the tarpit delay applied when EnableDelay is set.
		// Values <= 0 fall back to DefaultConfig.MaxDelay (2s).
		MaxDelay time.Duration
		// DelayFunc computes the tarpit delay for a heat score. The result is
		// clamped to MaxDelay and never exceeds it.
		// Values <= 0 mean no delay.
		// Default is a linear curve of score*10ms (e.g. 75 heat -> 750ms).
		DelayFunc func(score int) time.Duration
		// Jitter adds +/-20% randomness to the computed delay so delayed
		// clients do not retry in lockstep. The clamped MaxDelay is still
		// honoured.
		Jitter       bool
		ErrorHandler func(c *echo.Context, score int) error
	}
)

// DefaultConfig provides the standard settings for abuse detection middleware.
var DefaultConfig = Config{
	Skipper:   middleware.DefaultSkipper,
	KeyFunc:   func(c *echo.Context) string { return c.RealIP() },
	Threshold: 100,
	Cooldown:  30 * time.Minute,
	MaxDelay:  2 * time.Second,
	DelayFunc: func(score int) time.Duration {
		return time.Duration(score) * 10 * time.Millisecond
	},
	ErrorHandler: func(c *echo.Context, score int) error {
		return echo.NewHTTPError(http.StatusForbidden, "Access denied due to suspicious activity")
	},
}

// New returns an Echo middleware that monitors and limits abusive behavior.
// It should be placed early in the middleware stack to protect downstream resources.
func New(config Config) echo.MiddlewareFunc {
	// 1. Panic if store is missing
	if config.Store == nil {
		panic("http-keystone/abuse: Store is required")
	}

	// 2. Apply defaults. A zero Threshold would otherwise make
	// `currentScore >= config.Threshold` true for every request and reject all
	// traffic, so non-positive values fall back to DefaultConfig.
	if config.Threshold <= 0 {
		config.Threshold = DefaultConfig.Threshold
	}
	if config.Cooldown <= 0 {
		config.Cooldown = DefaultConfig.Cooldown
	}
	if config.Skipper == nil {
		config.Skipper = DefaultConfig.Skipper
	}
	if config.KeyFunc == nil {
		config.KeyFunc = DefaultConfig.KeyFunc
	}
	if config.MaxDelay <= 0 {
		config.MaxDelay = DefaultConfig.MaxDelay
	}
	if config.DelayFunc == nil {
		config.DelayFunc = DefaultConfig.DelayFunc
	}
	if config.ErrorHandler == nil {
		config.ErrorHandler = DefaultConfig.ErrorHandler
	}

	// Copy Rules so mutating the caller's slice after New has no effect.
	config.Rules = append([]Rule(nil), config.Rules...)

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if config.Skipper(c) {
				return next(c)
			}

			ctx := c.Request().Context()

			// The identity is deliberately pluggable: c.RealIP() alone trusts
			// proxy headers and lets a client pick its own abuse key.
			clientID := config.KeyFunc(c)
			if clientID == "" {
				clientID = c.RealIP()
			}
			key := "abuse:heat:" + clientID

			// 1. Fetch current heat
			currentScore, _ := config.Store.Get(ctx, key)

			// 2. Immediate Block first: blocked clients are rejected without
			// being parked in a tarpit sleep.
			if currentScore >= config.Threshold {
				return config.ErrorHandler(c, currentScore)
			}

			// 3. Tarpit Logic: slow "warm" clients down, cancellably and with
			// a hard upper bound.
			if config.EnableDelay && currentScore > (config.Threshold/2) {
				if err := sleepWithContext(ctx, config.delayFor(currentScore)); err != nil {
					return err
				}
			}

			// 4. Pre-Handler Path Analysis
			requestPath := c.Request().URL.Path
			penalty := 0
			for _, rule := range config.Rules {

				// check and match exact request method if any is defined in rule
				if rule.Method != "" && rule.Method != c.Request().Method {
					continue
				}

				if matchPath(rule.Path, requestPath) {
					penalty += rule.Score
				}
			}

			// 5. Atomic Update and Final Enforcement
			if penalty > 0 {
				newScore, err := config.Store.Increment(ctx, key, penalty, config.Cooldown)
				if err == nil && newScore >= config.Threshold {
					return config.ErrorHandler(c, newScore)
				}
			}

			return next(c)
		}
	}
}

// delayFor returns the clamped tarpit delay for a heat score.
func (config Config) delayFor(score int) time.Duration {
	delay := config.DelayFunc(score)
	if delay <= 0 {
		return 0
	}

	if config.Jitter {
		delta := float64(delay) * 0.2
		delay += time.Duration((rand.Float64()*2 - 1) * delta)
	}

	if delay > config.MaxDelay {
		delay = config.MaxDelay
	}
	if delay < 0 {
		return 0
	}
	return delay
}

// sleepWithContext waits for d or until ctx is done, whichever comes first.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// NewForwardedIPKeyFunc returns a KeyFunc that resolves the client IP from
// X-Forwarded-For, trusting only the direct peer when that peer is a trusted
// proxy. Pass echo.TrustIPRange (and the other echo.Trust* options) to describe
// your proxies; by default loopback, link-local and private ranges are trusted.
//
// Use it when the middleware runs behind a reverse proxy and you do not want to
// set echo.Echo.IPExtractor globally:
//
//	e.Use(abuse.New(abuse.Config{
//		Store:   counter,
//		KeyFunc: abuse.NewForwardedIPKeyFunc(echo.TrustIPRange(proxyCIDR)),
//	}))
//
// Without trusted proxies (for example when the app faces the internet
// directly), use echo.ExtractIPDirect with NewIPKeyFunc so headers are ignored
// completely.
func NewForwardedIPKeyFunc(options ...echo.TrustOption) func(c *echo.Context) string {
	extractor := echo.ExtractIPFromXFFHeader(options...)
	return func(c *echo.Context) string {
		return extractor(c.Request())
	}
}

// NewIPKeyFunc adapts an echo.IPExtractor into a KeyFunc. A nil extractor falls
// back to c.RealIP().
func NewIPKeyFunc(extractor echo.IPExtractor) func(c *echo.Context) string {
	if extractor == nil {
		return func(c *echo.Context) string { return c.RealIP() }
	}
	return func(c *echo.Context) string {
		return extractor(c.Request())
	}
}
