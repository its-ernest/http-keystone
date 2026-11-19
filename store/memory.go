// Package store provides the underlying persistence and locking mechanisms
// for the http-keystone middleware suite.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// item represents a cached value and its expiration deadline.
type item struct {
	entry     *Entry
	expiresAt time.Time
}

// MemoryStore is an in-memory implementation of the Store interface.
// It is safe for concurrent use and is ideal for single-instance
// applications. Entries and their bodies are deep-copied on the way in and on
// the way out, so callers can never mutate the store's data by holding on to
// an Entry they saved or received.
type MemoryStore struct {
	mu   sync.Mutex
	data map[string]item

	// maxEntries bounds the number of live entries. Zero means unlimited.
	maxEntries int

	// hits, misses, evictions are cumulative counters exposed via Stats.
	hits      atomic.Int64
	misses    atomic.Int64
	evictions atomic.Int64

	// stop is non-nil while a background evictor is running.
	stop chan struct{}

	locksMu sync.Mutex
	locks   sync.Map
}

// MemoryStoreOptions configures a MemoryStore.
type MemoryStoreOptions struct {
	// MaxEntries bounds the number of live entries. When the limit is reached
	// and a new key is saved, the entry that expires soonest is evicted to
	// make room. Zero means unlimited.
	MaxEntries int

	// StartEvictor launches a background evictor that removes expired entries
	// every interval. The zero value disables it. Use StartEvictor directly
	// when you need to tie the evictor to a context you control.
	EvictInterval time.Duration
}

// lockItem stores metadata about an active mutex-like lock. The token
// identifies the owner so Unlock and Refresh can refuse to touch a lock that
// has expired and been re-acquired by someone else.
type lockItem struct {
	token     string
	expiresAt time.Time
}

// MemoryStoreStats is a point-in-time snapshot of a MemoryStore's counters.
type MemoryStoreStats struct {
	// Hits is the number of successful Get calls.
	Hits int64
	// Misses is the number of Get calls that found nothing (including expired).
	Misses int64
	// Evictions is the number of entries removed to respect MaxEntries.
	Evictions int64
	// Size is the number of live entries right now.
	Size int
}

// NewMemoryStore initializes and returns a new, unbounded MemoryStore.
func NewMemoryStore() *MemoryStore {
	return NewMemoryStoreWithOptions(MemoryStoreOptions{})
}

// NewMemoryStoreWithOptions initializes a MemoryStore with the given options.
// When Options.EvictInterval is positive, a background evictor is started and
// stopped by Close.
func NewMemoryStoreWithOptions(opts MemoryStoreOptions) *MemoryStore {
	m := &MemoryStore{
		data:       make(map[string]item),
		maxEntries: opts.MaxEntries,
	}
	if m.maxEntries < 0 {
		m.maxEntries = 0
	}
	if opts.EvictInterval > 0 {
		m.StartEvictor(context.Background(), opts.EvictInterval)
	}
	return m
}

// clone returns a deep copy of the entry so the store never aliases caller
// memory.
func (e *Entry) clone() *Entry {
	if e == nil {
		return nil
	}
	out := &Entry{Status: e.Status}
	if e.Header != nil {
		out.Header = make(map[string][]string, len(e.Header))
		for name, values := range e.Header {
			out.Header[name] = append([]string(nil), values...)
		}
	}
	if e.Body != nil {
		out.Body = append([]byte(nil), e.Body...)
	}
	return out
}

// Get retrieves a cached Entry by its key. The returned entry is a deep copy,
// so mutating it does not affect the store. If the entry exists but has
// expired, it is deleted and ErrNotFound is returned.
func (m *MemoryStore) Get(_ context.Context, key string) (*Entry, error) {
	m.mu.Lock()
	i, ok := m.data[key]
	if ok && !time.Now().Before(i.expiresAt) {
		delete(m.data, key)
		ok = false
	}
	m.mu.Unlock()

	if !ok {
		m.misses.Add(1)
		return nil, ErrNotFound
	}
	m.hits.Add(1)
	return i.entry.clone(), nil
}

// Save persists a deep copy of an Entry into memory with a specific
// time-to-live (TTL). When MaxEntries is set and the store is full, the entry
// that expires soonest is evicted to make room.
func (m *MemoryStore) Save(_ context.Context, key string, entry *Entry, ttl time.Duration) error {
	stored := entry.clone()

	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	m.evictExpiredLocked(now)

	if m.maxEntries > 0 {
		if _, exists := m.data[key]; !exists && len(m.data) >= m.maxEntries {
			m.evictSoonestLocked(now)
		}
	}

	m.data[key] = item{entry: stored, expiresAt: now.Add(ttl)}
	return nil
}

// Delete removes a specific entry from the memory store immediately.
func (m *MemoryStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	delete(m.data, key)
	m.mu.Unlock()
	return nil
}

// Stats returns a snapshot of the store's counters.
func (m *MemoryStore) Stats() MemoryStoreStats {
	m.mu.Lock()
	size := len(m.data)
	m.mu.Unlock()
	return MemoryStoreStats{
		Hits:      m.hits.Load(),
		Misses:    m.misses.Load(),
		Evictions: m.evictions.Load(),
		Size:      size,
	}
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

// evictExpiredLocked removes every expired entry. The caller must hold m.mu.
func (m *MemoryStore) evictExpiredLocked(now time.Time) {
	for key, i := range m.data {
		if !now.Before(i.expiresAt) {
			delete(m.data, key)
		}
	}
}

// evictSoonestLocked removes the live entry that expires soonest (ties broken
// by key name) to make room. The caller must hold m.mu.
func (m *MemoryStore) evictSoonestLocked(now time.Time) {
	var (
		victim  string
		expiry  time.Time
		expired bool
		found   bool
	)
	for key, i := range m.data {
		e := i.expiresAt
		isExpired := !now.Before(e)
		if !found || (isExpired && !expired) ||
			(isExpired == expired && e.Before(expiry)) ||
			(isExpired == expired && e.Equal(expiry) && key < victim) {
			victim, expiry, expired, found = key, e, isExpired, true
		}
	}
	if !found {
		return
	}
	delete(m.data, victim)
	m.evictions.Add(1)
}

// StartEvictor launches a background goroutine that periodically scans the
// store and removes expired entries. The goroutine stops when ctx is canceled
// or Close is called, so no goroutine leaks. It is safe to call more than
// once; a later call replaces the previous evictor.
func (m *MemoryStore) StartEvictor(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}

	m.mu.Lock()
	stop := make(chan struct{})
	if m.stop != nil {
		close(m.stop)
	}
	m.stop = stop
	m.mu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.mu.Lock()
				m.evictExpiredLocked(time.Now())
				m.mu.Unlock()
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Close stops a background evictor started by StartEvictor. It is safe to call
// multiple times and does not invalidate the store.
func (m *MemoryStore) Close() error {
	m.mu.Lock()
	if m.stop != nil {
		close(m.stop)
		m.stop = nil
	}
	m.mu.Unlock()
	return nil
}
