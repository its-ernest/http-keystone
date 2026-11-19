// Package idempotency provides safe-retry middleware for Echo v5.
//
// It makes unsafe requests (POST, PUT, PATCH, DELETE) safe to retry by
// remembering the response to a request that carries an idempotency key and
// replaying it when the same key is seen again. This is the behaviour behind
// the "Idempotency-Key" header in the IETF draft and in payment APIs such as
// Stripe's.
//
// The middleware is built on store.Store and its owned locks (see the store
// package), so the "only one request runs at a time per key" guarantee holds
// across a single instance (MemoryStore) or a whole cluster (RedisStore).
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/its-ernest/http-keystone/internal"
	"github.com/its-ernest/http-keystone/store"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
)

// keyHeaderPrefix is prepended to the idempotency key to form the store key,
// so idempotency entries never collide with other uses of the same store.
const keyHeaderPrefix = "idempotency:"

// fingerprintHeader stores the request fingerprint alongside the response, so
// a later request with the same key but a different body can be detected.
// It is stripped from the stored response before it is replayed.
const fingerprintHeader = "X-Idempotency-Fingerprint"

// replayedHeader marks a response that was served from the store rather than
// produced by the handler. It is stripped from a freshly stored response.
const replayedHeader = "Idempotency-Replayed"

// New returns an Echo middleware that makes unsafe requests idempotent.
func New(config Config) echo.MiddlewareFunc {
	if config.Store == nil {
		panic("http-keystone/idempotency: Store is required")
	}
	if config.TTL <= 0 {
		config.TTL = DefaultConfig.TTL
	}
	if config.Skipper == nil {
		config.Skipper = middleware.DefaultSkipper
	}
	if config.Header == "" {
		config.Header = DefaultConfig.Header
	}
	if config.MaxBodySize <= 0 {
		config.MaxBodySize = DefaultConfig.MaxBodySize
	}
	if config.WaitTimeout <= 0 {
		config.WaitTimeout = DefaultConfig.WaitTimeout
	}
	if config.RetryDelay <= 0 {
		config.RetryDelay = DefaultConfig.RetryDelay
	}
	if config.MaxRetries <= 0 {
		config.MaxRetries = DefaultConfig.MaxRetries
	}
	if config.LockTTL <= 0 {
		config.LockTTL = DefaultConfig.LockTTL
	}
	if config.StoreStatusCodes == nil {
		config.StoreStatusCodes = DefaultConfig.StoreStatusCodes
	}
	if config.ErrorHandler == nil {
		config.ErrorHandler = DefaultConfig.ErrorHandler
	}

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if config.Skipper(c) {
				return next(c)
			}

			// Only unsafe methods are idempotent. GET/HEAD/OPTIONS/... always
			// pass straight through.
			if !isIdempotentMethod(c.Request().Method) {
				return next(c)
			}

			key := config.key(c)
			if key == "" {
				if config.Required {
					return config.ErrorHandler(c, ErrKeyRequired)
				}
				return next(c)
			}
			if !validKey(key) {
				return config.ErrorHandler(c, ErrKeyInvalid)
			}

			// Fingerprint the request so a key reused with a different body is
			// rejected rather than replayed. Reading the body here and putting
			// it back lets the handler read it again unchanged.
			body, err := readBody(c)
			if err != nil {
				return config.ErrorHandler(c, err)
			}
			fingerprint := fingerprint(c.Request().Method, c.Request().URL.RequestURI(), body)

			ctx := c.Request().Context()
			storeKey := keyHeaderPrefix + key

			// 1. A completed response for this key already exists: replay it,
			//    or reject it when the request does not match the stored one.
			entry, err := config.Store.Get(ctx, storeKey)
			switch {
			case err == nil && entry != nil:
				return config.replayOrReject(c, entry, fingerprint)
			case err == nil:
				// A nil entry with no error is treated as a miss.
			case err == store.ErrNotFound:
				// Expected on the first request; fall through to acquire.
			default:
				// A real store failure. Fail open or closed per config.
				return config.onStoreFailure(c, err, next)
			}

			// 2. No completed response: try to become the single runner.
			lease, err := config.Store.Lock(ctx, storeKey+":lock", config.LockTTL)
			if err == store.ErrLockFailed {
				return config.onDuplicate(c, storeKey, fingerprint, next)
			}
			if err != nil {
				return config.onStoreFailure(c, err, next)
			}
			defer func() { _ = config.Store.Unlock(ctx, lease) }()

			// 3. Double-check: another request may have completed between our
			//    first Get and our lock acquisition.
			if entry, err := config.Store.Get(ctx, storeKey); err == nil && entry != nil {
				return config.replayOrReject(c, entry, fingerprint)
			}

			// 4. Run the handler behind a recorder so we can store what it
			//    produced. A streaming or oversized response is not stored.
			originalWriter := c.Response()
			recorder := internal.NewResponseRecorder(originalWriter, config.MaxBodySize)
			c.SetResponse(recorder)
			defer c.SetResponse(originalWriter)

			if err := next(c); err != nil {
				recorder.Discard()
				return err
			}
			if recorder.Passthrough() {
				return nil
			}

			candidate := &store.Entry{
				Status: recorder.Status(),
				Header: recorder.Header().Clone(),
				Body:   recorder.Body(),
			}

			if statusStorable(candidate.Status, config.StoreStatusCodes) {
				stored := sanitizeHeader(candidate.Header)
				// The fingerprint rides in a dedicated header so the stored
				// entry carries everything needed to validate a later retry.
				stored.Set(fingerprintHeader, fingerprint)
				candidate.Header = stored
				if err := config.Store.Save(ctx, storeKey, candidate, config.TTL); err == nil {
					replaceHeader(recorder.Header(), stored)
				}
			}

			return recorder.Commit()
		}
	}
}

// key returns the idempotency key for c: KeyFunc if set, else the header.
func (config Config) key(c *echo.Context) string {
	if config.KeyFunc != nil {
		return config.KeyFunc(c)
	}
	return c.Request().Header.Get(config.Header)
}

// replayOrReject serves a stored entry when the fingerprint matches, or hands
// a mismatch to the ErrorHandler so the wrong response is never replayed.
func (config Config) replayOrReject(c *echo.Context, entry *store.Entry, fingerprint string) error {
	stored := entry.Header[fingerprintHeader]
	if len(stored) == 0 || stored[0] != fingerprint {
		return config.ErrorHandler(c, ErrFingerprintMismatch)
	}
	return replay(c, entry)
}

// onDuplicate handles a request that lost the race to become the runner.
func (config Config) onDuplicate(c *echo.Context, storeKey, fingerprint string, next echo.HandlerFunc) error {
	if config.InFlight == InFlightWait {
		entry, ok, err := waitForEntry(c.Request().Context(), config, storeKey)
		if err != nil {
			return err
		}
		if ok {
			return config.replayOrReject(c, entry, fingerprint)
		}
		// The wait budget ran out without a stored response; fall through to
		// the conflict response below.
	}
	return config.ErrorHandler(c, ErrRequestInFlight)
}

// onStoreFailure fails open (run the handler without idempotency) or closed
// (reject) when the store itself errors.
func (config Config) onStoreFailure(c *echo.Context, err error, next echo.HandlerFunc) error {
	if config.FailClosed {
		return err
	}
	return next(c)
}

// readBody returns the full request body and restores it so a downstream
// handler can read it again.
func readBody(c *echo.Context) ([]byte, error) {
	req := c.Request()
	if req.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

// isIdempotentMethod reports whether method is unsafe and therefore eligible
// for idempotency protection.
func isIdempotentMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// validKey reports whether key is 1-255 printable ASCII characters with no
// control bytes.
func validKey(key string) bool {
	if len(key) == 0 || len(key) > 255 {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

// fingerprint identifies a request for mismatch detection: method, request
// URI, and a hash of the body.
func fingerprint(method, requestURI string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(requestURI))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// statusStorable reports whether status is in the configured allow-list.
func statusStorable(status int, allowed []int) bool {
	for _, code := range allowed {
		if status == code {
			return true
		}
	}
	return false
}

// replay writes a stored entry to the response.
func replay(c *echo.Context, entry *store.Entry) error {
	headers := sanitizeHeader(http.Header(entry.Header))
	// The fingerprint is internal metadata, not part of the client response.
	headers.Del(fingerprintHeader)
	headers.Del(replayedHeader)

	for name, values := range headers {
		for _, value := range values {
			c.Response().Header().Add(name, value)
		}
	}
	c.Response().Header().Set(replayedHeader, "true")

	contentType := headers.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return c.Blob(entry.Status, contentType, entry.Body)
}

// waitForEntry polls the store for a completed entry while the first request
// runs, returning it once it appears or ok=false when the budget is spent.
func waitForEntry(ctx context.Context, config Config, storeKey string) (*store.Entry, bool, error) {
	for attempt := 0; attempt < config.MaxRetries; attempt++ {
		if err := sleepWithContext(ctx, retryDelay(config.RetryDelay, attempt)); err != nil {
			return nil, false, err
		}
		entry, err := config.Store.Get(ctx, storeKey)
		if err == nil && entry != nil {
			return entry, true, nil
		}
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
	}
	return nil, false, nil
}

// sleepWithContext waits for d or until ctx is done, whichever comes first.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryDelay grows linearly with the attempt number and adds +/-25% jitter so
// waiters do not poll in lockstep.
func retryDelay(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	delay := base * time.Duration(attempt+1)
	spread := int64(delay / 4)
	if spread <= 0 {
		return delay
	}
	return delay + time.Duration(rand.Int64N(2*spread+1)) - time.Duration(spread)
}
