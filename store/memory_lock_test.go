package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMemoryStore_ConcurrentLockSingleOwner asserts that when many goroutines
// race to acquire the same key, exactly one wins and the rest get
// ErrLockFailed. Run with -race to catch the takeover race the old
// LoadOrStore/Store pair had.
func TestMemoryStore_ConcurrentLockSingleOwner(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()

	const goroutines = 100
	var (
		won   atomic.Int32
		cond  sync.WaitGroup
		start = make(chan struct{})
	)

	cond.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer cond.Done()
			<-start

			lease, err := st.Lock(ctx, "hot-key", time.Minute)
			if err == nil && lease != nil {
				won.Add(1)
			} else {
				assert.ErrorIs(t, err, ErrLockFailed)
			}
		}()
	}

	close(start)
	cond.Wait()

	assert.Equal(t, int32(1), won.Load(), "exactly one goroutine must hold the lock")
}

// TestMemoryStore_StaleUnlockDoesNotRelease asserts that unlocking with a lease
// that is no longer the owner (its lock expired and was re-acquired) leaves the
// current owner's lock intact.
func TestMemoryStore_StaleUnlockDoesNotRelease(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()

	first, err := st.Lock(ctx, "res", 50*time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, first)

	// Let the first lock expire, then take it over.
	time.Sleep(80 * time.Millisecond)
	second, err := st.Lock(ctx, "res", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, second)
	require.NotEqual(t, first.Token, second.Token)

	// The stale lease must not release the new owner's lock.
	require.NoError(t, st.Unlock(ctx, first))

	_, err = st.Lock(ctx, "res", time.Minute)
	assert.ErrorIs(t, err, ErrLockFailed, "the current owner's lock must survive a stale unlock")

	// The real owner can still release it.
	require.NoError(t, st.Unlock(ctx, second))
	third, err := st.Lock(ctx, "res", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, third)
}

// TestMemoryStore_Refresh verifies that only the owner can extend a lock.
func TestMemoryStore_Refresh(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()

	lease, err := st.Lock(ctx, "res", 50*time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, lease)

	require.NoError(t, st.Refresh(ctx, lease, time.Minute))

	// A different token cannot refresh the lock.
	other := &Lease{Key: "res", Token: "not-the-owner"}
	assert.ErrorIs(t, st.Refresh(ctx, other, time.Minute), ErrLockNotHeld)

	// The owner still holds a live lock after the refresh.
	_, err = st.Lock(ctx, "res", time.Minute)
	assert.ErrorIs(t, err, ErrLockFailed)
}

// TestMemoryStore_LockTakeoverAfterExpiry asserts an expired lock can be
// acquired again and the original holder is fenced out by the token.
func TestMemoryStore_LockTakeoverAfterExpiry(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()

	first, err := st.Lock(ctx, "res", 40*time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, first)

	time.Sleep(70 * time.Millisecond)

	second, err := st.Lock(ctx, "res", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.NotEqual(t, first.Token, second.Token)

	// The first holder's late unlock must not free the second holder's lock.
	require.NoError(t, st.Unlock(ctx, first))
	_, err = st.Lock(ctx, "res", time.Minute)
	assert.ErrorIs(t, err, ErrLockFailed)
}
