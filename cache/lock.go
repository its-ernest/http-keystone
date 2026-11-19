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
//
// Locking is optional: a Store that also implements store.Locker gets stampede
// protection, while a plain Store (for example a custom cache backend or a
// test fake with no lock methods) is served directly. This keeps cache.New
// usable with any store.Store.
func acquireLockWithTTL(ctx context.Context, s store.Store, key string, ttl time.Duration) (bool, func()) {
	locker, ok := s.(store.Locker)
	if !ok {
		// No lock support: skip stampede protection and let the caller run
		// the handler directly. The bool is true so the caller proceeds.
		return true, func() {}
	}

	lease, err := locker.Lock(ctx, key, ttl)
	if err != nil {
		return false, func() {}
	}

	unlockFn := func() {
		_ = locker.Unlock(ctx, lease)
	}

	return true, unlockFn
}
