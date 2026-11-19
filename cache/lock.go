package cache

import (
	"context"
	"time"

	"github.com/its-ernest/http-keystone/store"
)

// acquireLockWithTTL attempts to acquire the lock at key and returns an unlock
// function. The bool is false when the lock could not be acquired (someone
// else holds it or the backend failed); the returned func is a no-op in that
// case so callers can defer it unconditionally.
func acquireLockWithTTL(ctx context.Context, s store.Store, key string, ttl time.Duration) (bool, func()) {
	lease, err := s.Lock(ctx, key, ttl)
	if err != nil {
		return false, func() {}
	}

	unlockFn := func() {
		_ = s.Unlock(ctx, lease)
	}

	return true, unlockFn
}
