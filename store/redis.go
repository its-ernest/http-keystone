package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore implements the Store interface using Redis as the backend.
// It is suitable for distributed production environments where multiple
// application instances need to share a consistent cache.
type RedisStore struct {
	client *redis.Client
}

// RedisStoreOptions provides configuration options for RedisStore.
type RedisStoreOptions struct {
	// Addr is the Redis server address (e.g., "localhost:6379")
	Addr string

	// PoolSize is the maximum number of socket connections.
	// Default is 10 connections per CPU.
	PoolSize int

	// MinIdleConns is the minimum number of idle connections.
	// Default is 0.
	MinIdleConns int

	// Password for authentication. Empty string means no password.
	Password string

	// DB is the Redis database to use. Default is 0.
	DB int
}

// NewRedisStore creates a new RedisStore with the provided address.
// This is a convenience constructor for simple setups.
func NewRedisStore(addr string) *RedisStore {
	return NewRedisStoreWithOptions(RedisStoreOptions{
		Addr:         addr,
		PoolSize:     10,
		MinIdleConns: 5,
	})
}

// NewRedisStoreWithOptions creates a new RedisStore with custom configuration.
func NewRedisStoreWithOptions(opts RedisStoreOptions) *RedisStore {
	return &RedisStore{
		client: newRedisClient(opts),
	}
}

// newRedisClient builds a go-redis client from the shared options type.
// A zero PoolSize/MinIdleConns/PoolTimeout lets go-redis apply its defaults.
func newRedisClient(opts RedisStoreOptions) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:         opts.Addr,
		PoolSize:     opts.PoolSize,
		MinIdleConns: opts.MinIdleConns,
		Password:     opts.Password,
		DB:           opts.DB,
	})
}

// Get retrieves a cached Entry by its key from Redis.
// Returns ErrNotFound if the key does not exist or has expired.
func (r *RedisStore) Get(ctx context.Context, key string) (*Entry, error) {
	val, err := r.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	var e Entry
	if err := json.Unmarshal([]byte(val), &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// Save persists an Entry into Redis with a specific time-to-live (TTL).
func (r *RedisStore) Save(ctx context.Context, key string, entry *Entry, ttl time.Duration) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return r.client.Set(ctx, key, data, ttl).Err()
}

// Delete removes a specific entry from Redis immediately.
func (r *RedisStore) Delete(ctx context.Context, key string) error {
	return r.client.Del(ctx, key).Err()
}

// lockTokenScript releases a lock only if the stored value still matches the
// caller's token (compare-and-delete). It returns 1 when the lock was
// released, 0 when it was not owned by the caller.
var lockTokenScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
else
	return 0
end
`)

// lockRefreshScript extends a lock's TTL only if the caller still owns it.
var lockRefreshScript = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("pexpire", KEYS[1], ARGV[2])
else
	return 0
end
`)

// Lock attempts to acquire an owned lock for key using SET NX with a unique
// token, so the lock is atomic and carries its owner. It returns ErrLockFailed
// when another owner holds a live lock.
func (r *RedisStore) Lock(ctx context.Context, key string, ttl time.Duration) (*Lease, error) {
	token := newLockToken()
	ok, err := r.client.SetNX(ctx, "lock:"+key, token, ttl).Result()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrLockFailed
	}
	return &Lease{Key: key, Token: token}, nil
}

// Unlock releases the lock identified by lease using a compare-and-delete, so
// a caller whose lock already expired and was re-acquired by someone else
// cannot delete the new owner's lock.
func (r *RedisStore) Unlock(ctx context.Context, lease *Lease) error {
	if lease == nil {
		return nil
	}
	return lockTokenScript.Run(ctx, r.client, []string{"lock:" + lease.Key}, lease.Token).Err()
}

// Refresh extends the TTL of the lock identified by lease. It returns
// ErrLockNotHeld when the lease is no longer the owner.
func (r *RedisStore) Refresh(ctx context.Context, lease *Lease, ttl time.Duration) error {
	if lease == nil {
		return ErrLockNotHeld
	}
	n, err := lockRefreshScript.Run(ctx, r.client, []string{"lock:" + lease.Key}, lease.Token, ttl.Milliseconds()).Int()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLockNotHeld
	}
	return nil
}

// Close closes the Redis connection pool.
// It is important to call this when shutting down the application.
func (r *RedisStore) Close() error {
	return r.client.Close()
}

// Ping verifies that the Redis server is reachable. It is useful for
// readiness checks and for failing fast when the configured address is wrong.
func (r *RedisStore) Ping(ctx context.Context) error {
	if err := r.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("store: redis ping: %w", err)
	}
	return nil
}
