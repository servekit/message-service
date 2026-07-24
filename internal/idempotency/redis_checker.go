package idempotency

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// pendingMarker is the placeholder value written by Reserve. Its presence
// (with empty payload return) signals "another caller is in flight".
const pendingMarker = "PENDING"

// RedisChecker implements Checker with *redis.Client.
type RedisChecker struct {
	client *redis.Client
	cfg    *Config
}

// NewRedisChecker constructs a RedisChecker. client must be non-nil and
// already Ping'd by the caller (service.New does this via redisx.New).
// cfg carries the key prefix and per-channel TTLs; must be non-nil.
func NewRedisChecker(client *redis.Client, cfg *Config) *RedisChecker {
	return &RedisChecker{client: client, cfg: cfg}
}

// Reserve implements Checker.
func (c *RedisChecker) Reserve(ctx context.Context, channel, senderID, key string) (acquired bool, payload []byte, err error) {
	redisKey := c.buildKey(channel, senderID, key)
	ttl := c.ttlFor(channel)
	acquired, err = c.client.SetNX(ctx, redisKey, pendingMarker, ttl).Result()
	if err != nil {
		return false, nil, err
	}
	if acquired {
		return true, nil, nil
	}
	// Key exists — GET to see what's there. Has its own TTL left from the
	// original SETNX; we don't refresh it here.
	val, err := c.client.Get(ctx, redisKey).Result()
	if err != nil {
		return false, nil, err
	}
	if val == pendingMarker {
		return false, nil, nil // in-flight
	}
	return false, []byte(val), nil // completed
}

// Complete implements Checker.
func (c *RedisChecker) Complete(ctx context.Context, channel, senderID, key string, payload []byte) error {
	redisKey := c.buildKey(channel, senderID, key)
	// SET overwrites the PENDING marker. TTL refreshes from the original
	// Reserve so the cached payload has the full window.
	return c.client.Set(ctx, redisKey, payload, c.ttlFor(channel)).Err()
}

// Release implements Checker.
func (c *RedisChecker) Release(ctx context.Context, channel, senderID, key string) error {
	redisKey := c.buildKey(channel, senderID, key)
	// DEL is a no-op if the key has already expired or been released.
	return c.client.Del(ctx, redisKey).Err()
}

// --- internal helpers ---

// buildKey constructs the Redis key for an idempotency reservation.
// Format: {keyPrefix}:{channel}:{senderID}:{key}
//
// Channel is namespace-isolated so the same senderID+key pair on different
// channels (email vs sms) does not collide.
func (c *RedisChecker) buildKey(channel, senderID, key string) string {
	return fmt.Sprintf("%s:%s:%s:%s", c.cfg.KeyPrefix, channel, senderID, key)
}

// ttlFor resolves the per-channel TTL. Falls back to SMS TTL for unknown
// channels (defensive — only "email" / "sms" are expected in practice).
func (c *RedisChecker) ttlFor(channel string) time.Duration {
	if channel == "email" {
		return c.cfg.EmailTTL
	}
	return c.cfg.SMSTTL
}
