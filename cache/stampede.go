package cache

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/its-ernest/http-keystone/store"
)

// waitForEntry polls the store while another request holds the population
// lock. It returns the entry once it appears, ok=false when the retry budget
// is exhausted, or ctx.Err() when the request is canceled.
//
// Polling is intentional: a waiter does not block the lock holder and any
// replica can pick up the result, including replicas sharing a Redis store.
func waitForEntry(ctx context.Context, config Config, key string) (*store.Entry, bool, error) {
	for attempt := 0; attempt < config.MaxRetries; attempt++ {
		if err := sleepWithContext(ctx, retryDelay(config.RetryDelay, attempt)); err != nil {
			return nil, false, err
		}

		entry, err := config.Store.Get(ctx, key)
		if err == nil && entry != nil {
			return entry, true, nil
		}
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
	}

	return nil, false, nil
}

// sleepWithContext waits for d or until the request context is canceled.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryDelay grows linearly with the attempt number and adds ±25% jitter so
// waiters do not poll in lockstep and multiply the load on the store.
func retryDelay(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}

	delay := base * time.Duration(attempt+1)
	spread := int64(delay / 4)
	if spread <= 0 {
		return delay
	}

	return delay + time.Duration(rand.Int64N(2*spread+1)) - time.Duration(spread)
}
