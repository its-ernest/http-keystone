package store

import (
	"context"
	"sync"
	"time"
)

// MemoryCounter is a thread-safe in-memory Counter. Entries are stored with an
// expiry and are removed lazily on access, by an optional background evictor
// (see StartEvictor), or when MaxEntries is exceeded.
type MemoryCounter struct {
	mu       sync.Mutex
	counts   map[string]int
	expiries map[string]time.Time

	// maxEntries bounds the number of tracked keys. Zero means unlimited.
	maxEntries int

	stop chan struct{}
}

// NewMemoryCounter returns a new thread-safe memory-based Counter.
func NewMemoryCounter() *MemoryCounter {
	return &MemoryCounter{
		counts:   make(map[string]int),
		expiries: make(map[string]time.Time),
	}
}

// NewMemoryCounterWithLimit returns a MemoryCounter that keeps at most
// maxEntries keys. When the limit is reached, the key that expires first is
// evicted to make room for the new one. A maxEntries value <= 0 means
// unlimited.
func NewMemoryCounterWithLimit(maxEntries int) *MemoryCounter {
	c := NewMemoryCounter()
	if maxEntries > 0 {
		c.maxEntries = maxEntries
	}
	return c
}

func (m *MemoryCounter) Increment(ctx context.Context, key string, n int, ttl time.Duration) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	m.evictExpiredLocked(now)

	// 1. Check if expired or missing
	if exp, ok := m.expiries[key]; ok && now.After(exp) {
		delete(m.counts, key)
		delete(m.expiries, key)
	}

	// 2. Enforce the size limit before adding a brand new key.
	if _, exists := m.counts[key]; !exists && m.maxEntries > 0 && len(m.counts) >= m.maxEntries {
		m.evictSoonestLocked()
	}

	// 3. Increment
	m.counts[key] += n

	// 4. Update Expiry (only if it's a new key or you want to "slide" the window)
	if ttl > 0 {
		m.expiries[key] = now.Add(ttl)
	}

	return m.counts[key], nil
}

func (m *MemoryCounter) Reset(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.counts, key)
	delete(m.expiries, key)
	return nil
}

// Get returns the current count for key. An expired key is deleted and 0 is
// returned.
func (m *MemoryCounter) Get(_ context.Context, key string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	if exp, ok := m.expiries[key]; ok && now.After(exp) {
		delete(m.counts, key)
		delete(m.expiries, key)
		return 0, nil
	}

	return m.counts[key], nil
}

// Len returns the number of tracked keys, excluding expired ones.
func (m *MemoryCounter) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.evictExpiredLocked(time.Now())
	return len(m.counts)
}

// StartEvictor launches a background goroutine that removes expired keys every
// interval, stopping when ctx is done or Close is called. It is safe to call
// more than once; later calls replace the previous evictor.
func (m *MemoryCounter) StartEvictor(ctx context.Context, interval time.Duration) {
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
// multiple times and does not invalidate the counter.
func (m *MemoryCounter) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.stop != nil {
		close(m.stop)
		m.stop = nil
	}
	return nil
}

// evictExpiredLocked removes every expired key. The caller must hold m.mu.
func (m *MemoryCounter) evictExpiredLocked(now time.Time) {
	for key, exp := range m.expiries {
		if now.After(exp) {
			delete(m.counts, key)
			delete(m.expiries, key)
		}
	}
}

// evictSoonestLocked removes the key with the earliest expiry (ties broken by
// key name). The caller must hold m.mu.
func (m *MemoryCounter) evictSoonestLocked() {
	var (
		oldestKey string
		oldestExp time.Time
		found     bool
	)

	for key, exp := range m.expiries {
		if !found || exp.Before(oldestExp) || (exp.Equal(oldestExp) && key < oldestKey) {
			oldestKey, oldestExp, found = key, exp, true
		}
	}

	// Keys without an expiry are only evicted when nothing else is tracked.
	if !found {
		for key := range m.counts {
			oldestKey, found = key, true
			break
		}
	}

	delete(m.counts, oldestKey)
	delete(m.expiries, oldestKey)
}
