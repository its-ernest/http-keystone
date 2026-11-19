package idempotency

import "errors"

var (
	// ErrKeyRequired is passed to ErrorHandler when a request carries no
	// idempotency key but Config.Required is set.
	ErrKeyRequired = errors.New("idempotency: an Idempotency-Key header is required")

	// ErrKeyInvalid is passed to ErrorHandler when the key is empty, too
	// long, or contains control characters.
	ErrKeyInvalid = errors.New("idempotency: the Idempotency-Key is invalid")

	// ErrFingerprintMismatch is passed to ErrorHandler when a key is reused
	// with a different request (method, path, or body) than the one that was
	// stored. The stored response is never replayed in this case.
	ErrFingerprintMismatch = errors.New("idempotency: the key was already used with a different request")

	// ErrRequestInFlight is passed to ErrorHandler when a duplicate arrives
	// while the first request with the same key is still running and the
	// policy is InFlightReject.
	ErrRequestInFlight = errors.New("idempotency: a request with this key is already being processed")
)
