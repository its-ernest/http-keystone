package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/its-ernest/http-keystone/store"
	"github.com/stretchr/testify/assert"
)

// readerWriterStore implements only Reader and Writer (no Locker, no Counter).
// It proves a custom cache backend does not need to implement lock methods.
type readerWriterStore struct {
	data map[string]*store.Entry
}

func (s *readerWriterStore) Get(_ context.Context, key string) (*store.Entry, error) {
	e, ok := s.data[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return e, nil
}

func (s *readerWriterStore) Save(_ context.Context, key string, entry *store.Entry, _ time.Duration) error {
	s.data[key] = entry
	return nil
}

func (s *readerWriterStore) Delete(_ context.Context, key string) error {
	delete(s.data, key)
	return nil
}

// Compile-time proof that a Reader+Writer fake satisfies store.Store and does
// NOT satisfy store.Locker, so it can back cache.New (which only needs Store).
var (
	_ store.Store  = (*readerWriterStore)(nil)
	_ store.Reader = (*readerWriterStore)(nil)
	_ store.Writer = (*readerWriterStore)(nil)
)

// TestReaderWriterOnlyStoreIsNotLocker documents the split: a store that is
// only a Reader/Writer must not be mistaken for a Locker.
func TestReaderWriterOnlyStoreIsNotLocker(t *testing.T) {
	var s store.Store = &readerWriterStore{data: make(map[string]*store.Entry)}
	_, isLocker := s.(store.Locker)
	assert.False(t, isLocker, "a Reader/Writer store must not satisfy Locker")
}

// counterOnlyCounter implements only Counter, proving abuse.New can accept a
// counter-only fake.
type counterOnlyCounter struct {
	counts map[string]int
}

func (c *counterOnlyCounter) Get(_ context.Context, key string) (int, error) {
	v, ok := c.counts[key]
	if !ok {
		return 0, store.ErrNotFound
	}
	return v, nil
}

func (c *counterOnlyCounter) Increment(_ context.Context, key string, n int, _ time.Duration) (int, error) {
	c.counts[key] += n
	return c.counts[key], nil
}

func (c *counterOnlyCounter) Reset(_ context.Context, key string) error {
	delete(c.counts, key)
	return nil
}

// Compile-time proof that the fake satisfies only Counter.
var _ store.Counter = (*counterOnlyCounter)(nil)

// TestCounterOnlyMissingKeyIsNotFound asserts the unified not-found semantics.
func TestCounterOnlyMissingKeyIsNotFound(t *testing.T) {
	c := &counterOnlyCounter{counts: make(map[string]int)}
	_, err := c.Get(context.Background(), "missing")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestMemoryCounterMissingKeyIsNotFound asserts the real MemoryCounter also
// returns ErrNotFound for a missing key.
func TestMemoryCounterMissingKeyIsNotFound(t *testing.T) {
	c := store.NewMemoryCounter()
	_, err := c.Get(context.Background(), "missing")
	assert.ErrorIs(t, err, store.ErrNotFound)
}
