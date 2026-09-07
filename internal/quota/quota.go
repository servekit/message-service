// Package quota implements the per-app daily send-attempt cap: a Redis
// INCR counter keyed by (app_key, channel, UTC day). limit <= 0 means
// unlimited. Attempts are counted, not deliveries — the goal is protecting
// vendor accounts from runaway loops, not precise billing.
package quota

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/servekit/message-service/pkg/xcodes"
)

// Checker gates sends against the daily cap.
type Checker struct {
	rdb    *redis.Client
	prefix string
}

// NewChecker builds a Checker. prefix scopes the Redis keys ("msg:quota"
// default at the call site).
func NewChecker(rdb *redis.Client, prefix string) *Checker {
	if prefix == "" {
		prefix = "msg:quota"
	}
	return &Checker{rdb: rdb, prefix: prefix}
}

// Allow increments the day counter for (appKey, channel) and reports
// whether the send may proceed. Returns nil error when limit <= 0.
// counter TTL covers the day plus headroom so stale keys expire naturally.
func (c *Checker) Allow(ctx context.Context, appKey, channel string, limit int64) error {
	if limit <= 0 {
		return nil
	}
	now := time.Now().UTC()
	key := fmt.Sprintf("%s:%s:%s:%s", c.prefix, appKey, channel, now.Format("20060102"))

	count, err := c.rdb.Incr(ctx, key).Result()
	if err != nil {
		// Quota is a protection rail, not a hard dependency — on Redis
		// failure the send proceeds. Idempotency (also Redis-backed) will
		// surface the outage loudly.
		return nil
	}
	if count == 1 {
		c.rdb.Expire(ctx, key, 48*time.Hour)
	}
	if count > limit {
		return xcodes.ErrDailyQuotaExceeded.New(fmt.Sprintf(
			"app %s channel %s daily limit %d exceeded", appKey, channel, limit))
	}
	return nil
}
