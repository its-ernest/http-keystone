package store

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrNotFound is returned when a requested key does not exist in the store
	// or has already expired. Middleware should typically handle this by
	// proceeding to the next handler to fetch fresh data.
	ErrNotFound = errors.New("store: item not found")

	// ErrLockFailed is returned when a lock cannot be acquired because another
	// owner currently holds it. Callers should treat it as "someone else is
	// already doing this work" (for example, wait for the result or fail open).
	ErrLockFailed = errors.New("store: could not acquire lock")

	// ErrLockNotHeld is returned when a lease is refreshed or operated on but
	// is no longer the owner: it expired and may have been re-acquired by a
	// different caller. Unlock treats this as a no-op, but Refresh reports it
	// so a holder can react (stop work, retry) instead of assuming it still
	// owns the lock.
	ErrLockNotHeld = errors.New("store: lock is not held")
)

// Entry represents the data that actually persists
type Entry struct {
	Status int                 `json:"status"`
	Header map[string][]string `json:"header"`
	Body   []byte              `json:"body"`
}

// Lease is an owned lock handed back by Store.Lock. The Token is an
// unguessable identifier for the owner; Unlock and Refresh compare it so a
// caller can never release or extend a lock that now belongs to someone else
// after its TTL expired and it was re-acquired.
type Lease struct {
	// Key is the logical key the lock was acquired for.
	Key string

	// Token identifies the owner of the lock.
	Token string
}

// Reader is the read half of a Store: it fetches entries by key.
type Reader interface {
	// Get retrieves an entry by key. It returns ErrNotFound when the key is
	// missing or has expired.
	Get(ctx context.Context, key string) (*Entry, error)
}

// Writer is the write half of a Store: it saves and deletes entries.
type Writer interface {
	// Save stores an entry with a specific TTL.
	Save(ctx context.Context, key string, entry *Entry, ttl time.Duration) error

	// Delete removes an entry.
	Delete(ctx context.Context, key string) error
}

// Store defines the contract for any backend that can read and write entries
// (Memory, Redis, etc.). It is a Reader and a Writer. Backends that can also
// provide owned locks implement Locker in addition; callers that only need
// persistence should depend on Store, not Locker.
type Store interface {
	Reader
	Writer
}

// Locker provides owned, expiring locks on top of a Store. It is optional:
// a Store used only as a cache does not need to implement it.
type Locker interface {
	// Lock attempts to acquire an owned lock for key that expires after ttl.
	// On success it returns a Lease identifying the owner. It returns
	// ErrLockFailed when another owner holds a live lock for the key. A
	// non-positive ttl creates a lock that does not expire on its own.
	Lock(ctx context.Context, key string, ttl time.Duration) (*Lease, error)

	// Unlock releases the lock identified by lease. Unlocking with a lease
	// that is no longer the owner (it expired and was re-acquired) is a no-op
	// and never releases the current owner's lock.
	Unlock(ctx context.Context, lease *Lease) error

	// Refresh extends the TTL of the lock identified by lease. It returns
	// ErrLockNotHeld when the lease is no longer the owner. A non-positive ttl
	// makes the lock stop expiring on its own.
	Refresh(ctx context.Context, lease *Lease, ttl time.Duration) error
}

// Counter defines a simpler interface for numeric tracking (Abuse, Rate Limiting)
type Counter interface {
	// Get returns the current count for a key. It returns ErrNotFound when the
	// key is missing or has expired, matching Store.Get.
	Get(ctx context.Context, key string) (int, error)

	// Increment adds n to the key and returns the new total.
	// if the key doesn't exist, it's created with value 'n' and the given TTL.
	Increment(ctx context.Context, key string, n int, ttl time.Duration) (int, error)

	// Reset clears the counter
	Reset(ctx context.Context, key string) error
}

// Compile-time assertions that the concrete backends satisfy the interfaces.
var (
	_ Store   = (*MemoryStore)(nil)
	_ Locker  = (*MemoryStore)(nil)
	_ Counter = (*MemoryCounter)(nil)

	_ Store   = (*RedisStore)(nil)
	_ Locker  = (*RedisStore)(nil)
	_ Counter = (*RedisCounter)(nil)
)
