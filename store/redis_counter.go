package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrCounterOverflow is returned when a counter value cannot be represented
// as an int on the current platform.
var ErrCounterOverflow = errors.New("store: counter value overflows int")

// RedisCounter implements Counter using Redis. It is suitable for distributed
// deployments where multiple application instances must share abuse scores,
// rate limits, or any other numeric counter.
//
// Increment is atomic: the INCRBY and the optional expiry update run in a
// single Lua script, so concurrent increments cannot lose updates or leave a
// key without its TTL.
type RedisCounter struct {
	client     *redis.Client
	ownsClient bool
}

// Compile-time check that RedisCounter satisfies Counter.
var _ Counter = (*RedisCounter)(nil)

// redisCounterIncrementScript increments a counter and, when a positive TTL
// is provided, sets its expiry in the same atomic operation.
var redisCounterIncrementScript = redis.NewScript(`
local value = redis.call("INCRBY", KEYS[1], ARGV[1])
local ttl = tonumber(ARGV[2])
if ttl and ttl > 0 then
	redis.call("PEXPIRE", KEYS[1], ttl)
end
return value
`)

// NewRedisCounter creates a Redis-backed Counter using addr (for example
// "localhost:6379"). This is the convenience constructor for simple setups.
func NewRedisCounter(addr string) *RedisCounter {
	return NewRedisCounterWithOptions(RedisStoreOptions{
		Addr:         addr,
		PoolSize:     10,
		MinIdleConns: 5,
	})
}

// NewRedisCounterWithOptions creates a RedisCounter with custom connection
// settings. It accepts the same options type as RedisStore so cache and abuse
// tracking can be pointed at the same Redis instance.
func NewRedisCounterWithOptions(opts RedisStoreOptions) *RedisCounter {
	return &RedisCounter{
		client:     newRedisClient(opts),
		ownsClient: true,
	}
}

// NewRedisCounterWithClient creates a RedisCounter that reuses an existing
// go-redis client. The caller retains ownership of the client: Close is a
// no-op for counters created this way, and the caller must close the client
// when the application shuts down.
func NewRedisCounterWithClient(client *redis.Client) *RedisCounter {
	return &RedisCounter{client: client}
}

// Get returns the current value for key. A missing or expired key returns
// ErrNotFound, matching the in-memory Counter implementation.
func (c *RedisCounter) Get(ctx context.Context, key string) (int, error) {
	value, err := c.client.Get(ctx, key).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: redis counter get %q: %w", key, err)
	}
	return counterInt(value)
}

// Increment atomically adds n to key and returns the new total. When ttl is
// positive, it becomes the key's expiry; every increment refreshes it, which
// gives abuse tracking a sliding cooldown window.
func (c *RedisCounter) Increment(ctx context.Context, key string, n int, ttl time.Duration) (int, error) {
	ttlMillis := ttl.Milliseconds()
	if ttl > 0 && ttlMillis == 0 {
		// Redis PEXPIRE uses millisecond precision; never silently drop a
		// sub-millisecond TTL.
		ttlMillis = 1
	}

	value, err := redisCounterIncrementScript.Run(ctx, c.client, []string{key}, n, ttlMillis).Int64()
	if err != nil {
		return 0, fmt.Errorf("store: redis counter increment %q: %w", key, err)
	}
	return counterInt(value)
}

// Reset removes key immediately.
func (c *RedisCounter) Reset(ctx context.Context, key string) error {
	if err := c.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("store: redis counter reset %q: %w", key, err)
	}
	return nil
}

// Ping verifies that the Redis server is reachable.
func (c *RedisCounter) Ping(ctx context.Context) error {
	if err := c.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("store: redis counter ping: %w", err)
	}
	return nil
}

// Close closes the underlying client when the counter owns it. It is a no-op
// for counters created with NewRedisCounterWithClient.
func (c *RedisCounter) Close() error {
	if c == nil || c.client == nil || !c.ownsClient {
		return nil
	}
	return c.client.Close()
}

// counterInt converts a Redis int64 to int without silently truncating.
func counterInt(value int64) (int, error) {
	converted := int(value)
	if int64(converted) != value {
		return 0, ErrCounterOverflow
	}
	return converted, nil
}
