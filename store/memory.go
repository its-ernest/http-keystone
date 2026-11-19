// Package store provides the underlying persistence and locking mechanisms
// for the http-keystone middleware suite.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"sync"
	"time"
)

// item represents a cached value and its expiration deadline.
type item struct {
	entry     *Entry
	expiresAt time.Time
}

// MemoryStore is an in-memory implementation of the Store interface.
// It utilizes sync.Map for thread-safe data operations and distributed-style
// locking simulations, making it ideal for single-instance applications.
type MemoryStore struct {
	data sync.Map

	locksMu sync.Mutex
	locks   sync.Map
}

// lockItem stores metadata about an active mutex-like lock. The token
// identifies the owner so Unlock and Refresh can refuse to touch a lock that
// has expired and been re-acquired by someone else.
type lockItem struct {
	token     string
	expiresAt time.Time
}

// NewMemoryStore initializes and returns a new pointer to a MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{}
}

// Get retrieves a cached Entry by its key. If the entry exists but has
// expired, it is deleted and ErrNotFound is returned.
func (m *MemoryStore) Get(_ context.Context, key string) (*Entry, error) {
	val, ok := m.data.Load(key)
	if !ok {
		return nil, ErrNotFound
	}

	i := val.(item)
	if time.Now().After(i.expiresAt) {
		m.data.Delete(key)
		return nil, ErrNotFound
	}
	return i.entry, nil
}

// Save persists an Entry into memory with a specific time-to-live (TTL).
func (m *MemoryStore) Save(_ context.Context, key string, entry *Entry, ttl time.Duration) error {
	m.data.Store(key, item{
		entry:     entry,
		expiresAt: time.Now().Add(ttl),
	})
	return nil
}

// Delete removes a specific entry from the memory store immediately.
func (m *MemoryStore) Delete(_ context.Context, key string) error {
	m.data.Delete(key)
	return nil
}

// Lock attempts to acquire an owned, non-blocking lock for a given key.
// It returns a Lease identifying the owner, or ErrLockFailed if another caller
// holds a live lock. An expired lock may be taken over atomically: the check
// and the overwrite happen under one mutex, so two goroutines can never both
// observe the same expired lock and both succeed.
func (m *MemoryStore) Lock(_ context.Context, key string, ttl time.Duration) (*Lease, error) {
	m.locksMu.Lock()
	defer m.locksMu.Unlock()

	now := time.Now()
	token := newLockToken()

	if val, ok := m.locks.Load(key); ok {
		existing := val.(lockItem)
		if now.Before(existing.expiresAt) {
			return nil, ErrLockFailed
		}
		// The lock expired; take it over. LoadAndStore is not needed because
		// the mutex already excludes other Lock/Unlock/Refresh callers.
		m.locks.Store(key, lockItem{token: token, expiresAt: now.Add(ttl)})
		return &Lease{Key: key, Token: token}, nil
	}

	m.locks.Store(key, lockItem{token: token, expiresAt: now.Add(ttl)})
	return &Lease{Key: key, Token: token}, nil
}

// Unlock releases the lock identified by lease. It only deletes the entry when
// the stored token matches the lease, so a caller whose lock already expired
// and was re-acquired cannot release the new owner's lock.
func (m *MemoryStore) Unlock(_ context.Context, lease *Lease) error {
	if lease == nil {
		return nil
	}

	m.locksMu.Lock()
	defer m.locksMu.Unlock()

	if val, ok := m.locks.Load(lease.Key); ok {
		if val.(lockItem).token == lease.Token {
			m.locks.Delete(lease.Key)
		}
	}
	return nil
}

// Refresh extends the TTL of the lock identified by lease. It returns
// ErrLockNotHeld when the lease is no longer the owner.
func (m *MemoryStore) Refresh(_ context.Context, lease *Lease, ttl time.Duration) error {
	if lease == nil {
		return ErrLockNotHeld
	}

	m.locksMu.Lock()
	defer m.locksMu.Unlock()

	val, ok := m.locks.Load(lease.Key)
	if !ok || val.(lockItem).token != lease.Token {
		return ErrLockNotHeld
	}
	m.locks.Store(lease.Key, lockItem{token: lease.Token, expiresAt: time.Now().Add(ttl)})
	return nil
}

// newLockToken returns a random, unguessable owner token for a lock.
func newLockToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should not fail; fall back to a time-based token so the
		// lock still has a unique-enough owner rather than colliding.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// StartEvictor launches a background goroutine that periodically scans the
// store and removes expired entries. The goroutine stops when the provided
// stop channel is closed.
func (m *MemoryStore) StartEvictor(interval time.Duration, stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.data.Range(func(k, v interface{}) bool {
					i := v.(item)
					if time.Now().After(i.expiresAt) {
						m.data.Delete(k)
					}
					return true
				})
			case <-stop:
				return
			}
		}
	}()
}
