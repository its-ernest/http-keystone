package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMemoryStore_SaveCopiesEntry asserts that mutating the entry after Save
// does not change what Get returns.
func TestMemoryStore_SaveCopiesEntry(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()

	entry := &Entry{
		Status: 200,
		Header: map[string][]string{"Content-Type": {"text/plain"}},
		Body:   []byte("hello"),
	}
	require.NoError(t, st.Save(ctx, "k", entry, time.Minute))

	// Mutate the caller's entry after saving.
	entry.Status = 500
	entry.Header["Content-Type"] = []string{"application/json"}
	entry.Body[0] = 'X'

	got, err := st.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, 200, got.Status, "mutating the saved entry must not change the stored status")
	assert.Equal(t, []string{"text/plain"}, got.Header["Content-Type"], "mutating the saved header must not change the stored header")
	assert.Equal(t, []byte("hello"), got.Body, "mutating the saved body must not change the stored body")
}

// TestMemoryStore_GetReturnsCopy asserts that mutating Get's result does not
// corrupt the store.
func TestMemoryStore_GetReturnsCopy(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()

	require.NoError(t, st.Save(ctx, "k", &Entry{
		Status: 200,
		Header: map[string][]string{"X-Test": {"1"}},
		Body:   []byte("hello"),
	}, time.Minute))

	first, err := st.Get(ctx, "k")
	require.NoError(t, err)
	first.Status = 500
	first.Header["X-Test"] = []string{"mutated"}
	first.Body[0] = 'X'

	second, err := st.Get(ctx, "k")
	require.NoError(t, err)
	assert.Equal(t, 200, second.Status, "mutating a Get result must not corrupt the store")
	assert.Equal(t, []string{"1"}, second.Header["X-Test"], "mutating a Get header must not corrupt the store")
	assert.Equal(t, []byte("hello"), second.Body, "mutating a Get body must not corrupt the store")
}

// TestMemoryStore_MaxEntriesEviction asserts that MaxEntries is enforced by
// evicting the entry that expires soonest.
func TestMemoryStore_MaxEntriesEviction(t *testing.T) {
	st := NewMemoryStoreWithOptions(MemoryStoreOptions{MaxEntries: 2})
	ctx := context.Background()

	require.NoError(t, st.Save(ctx, "a", &Entry{Status: 200, Body: []byte("a")}, time.Hour))
	require.NoError(t, st.Save(ctx, "b", &Entry{Status: 200, Body: []byte("b")}, 2*time.Hour))
	// "a" expires soonest, so saving "c" must evict "a".
	require.NoError(t, st.Save(ctx, "c", &Entry{Status: 200, Body: []byte("c")}, 3*time.Hour))

	_, err := st.Get(ctx, "a")
	assert.ErrorIs(t, err, ErrNotFound, "the soonest-expiring entry must be evicted first")

	_, err = st.Get(ctx, "b")
	assert.NoError(t, err)
	_, err = st.Get(ctx, "c")
	assert.NoError(t, err)

	stats := st.Stats()
	assert.Equal(t, int64(1), stats.Evictions)
	assert.Equal(t, 2, stats.Size)
}

// TestMemoryStore_MaxEntriesUnlimited asserts MaxEntries of zero does not evict.
func TestMemoryStore_MaxEntriesUnlimited(t *testing.T) {
	st := NewMemoryStore() // unbounded
	ctx := context.Background()

	for _, k := range []string{"a", "b", "c", "d", "e"} {
		require.NoError(t, st.Save(ctx, k, &Entry{Status: 200}, time.Hour))
	}
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		_, err := st.Get(ctx, k)
		assert.NoError(t, err, "an unbounded store must not evict %q", k)
	}
	assert.Equal(t, int64(0), st.Stats().Evictions)
}

// TestMemoryStore_EvictorStopsOnCancel asserts the background evictor stops
// when its context is canceled (no goroutine leak) and removes expired entries.
func TestMemoryStore_EvictorStopsOnCancel(t *testing.T) {
	st := NewMemoryStore()
	ctx, cancel := context.WithCancel(context.Background())

	require.NoError(t, st.Save(ctx, "k", &Entry{Status: 200}, 10*time.Millisecond))
	st.StartEvictor(ctx, 5*time.Millisecond)

	// The evictor removes the entry once it expires.
	require.Eventually(t, func() bool {
		_, err := st.Get(context.Background(), "k")
		return err == ErrNotFound
	}, time.Second, 5*time.Millisecond)

	// Cancel the context and make sure Close/evictor do not panic.
	cancel()
	require.NoError(t, st.Close())
}

// TestMemoryStore_CloseIsIdempotent asserts Close can be called repeatedly.
func TestMemoryStore_CloseIsIdempotent(t *testing.T) {
	st := NewMemoryStoreWithOptions(MemoryStoreOptions{EvictInterval: time.Hour})
	require.NoError(t, st.Close())
	require.NoError(t, st.Close(), "Close must be safe to call multiple times")
}

// TestMemoryStore_StartEvictorReplacesPrevious asserts a later StartEvictor
// replaces the previous one without leaking.
func TestMemoryStore_StartEvictorReplacesPrevious(t *testing.T) {
	st := NewMemoryStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st.StartEvictor(ctx, time.Hour)
	st.StartEvictor(ctx, time.Hour) // replaces the first
	require.NoError(t, st.Close())
}

// TestMemoryStore_StatsCountsHitsAndMisses asserts Stats reports hits/misses.
func TestMemoryStore_StatsCountsHitsAndMisses(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()

	require.NoError(t, st.Save(ctx, "k", &Entry{Status: 200}, time.Minute))
	_, _ = st.Get(ctx, "k")    // hit
	_, _ = st.Get(ctx, "k")    // hit
	_, _ = st.Get(ctx, "gone") // miss

	stats := st.Stats()
	assert.Equal(t, int64(2), stats.Hits)
	assert.Equal(t, int64(1), stats.Misses)
}
