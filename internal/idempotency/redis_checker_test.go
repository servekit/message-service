package idempotency

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/servekit/go-common/redisx"
)

func newTestChecker(t *testing.T) (*RedisChecker, *redis.Client) {
	t.Helper()
	client := redisx.NewTestClient(t)
	return NewRedisChecker(client, &Config{
		KeyPrefix: "msg:idem",
		EmailTTL:  time.Minute,
		SMSTTL:    time.Minute,
	}), client
}

func TestRedisChecker_Reserve_FirstCallAcquires(t *testing.T) {
	c, _ := newTestChecker(t)
	ctx := context.Background()

	acquired, payload, err := c.Reserve(ctx, "email", "user:1", "key-1")
	require.NoError(t, err)
	assert.True(t, acquired, "first call must acquire")
	assert.Nil(t, payload, "first call has no payload")
}

func TestRedisChecker_Reserve_SecondCallAfterCompleteReturnsPayload(t *testing.T) {
	c, _ := newTestChecker(t)
	ctx := context.Background()

	_, _, err := c.Reserve(ctx, "email", "user:1", "key-2")
	require.NoError(t, err)

	payload := []byte(`{"status":"SENT","id":42}`)
	require.NoError(t, c.Complete(ctx, "email", "user:1", "key-2", payload))

	acquired, got, err := c.Reserve(ctx, "email", "user:1", "key-2")
	require.NoError(t, err)
	assert.False(t, acquired, "second call after Complete must not acquire")
	assert.Equal(t, payload, got, "must return cached payload")
}

func TestRedisChecker_Reserve_SecondCallWhilePendingReturnsNil(t *testing.T) {
	c, _ := newTestChecker(t)
	ctx := context.Background()

	_, _, err := c.Reserve(ctx, "sms", "user:1", "key-3")
	require.NoError(t, err)

	// Second call before Complete — should see PENDING marker.
	acquired, got, err := c.Reserve(ctx, "sms", "user:1", "key-3")
	require.NoError(t, err)
	assert.False(t, acquired, "in-flight call must not acquire")
	assert.Nil(t, got, "in-flight call returns nil payload (caller returns ErrIdempotencyConflict)")
}

func TestRedisChecker_Complete_OverwritesPending(t *testing.T) {
	c, client := newTestChecker(t)
	ctx := context.Background()

	_, _, err := c.Reserve(ctx, "email", "user:1", "key-4")
	require.NoError(t, err)

	pending, err := client.Get(ctx, "msg:idem:email:user:1:key-4").Result()
	require.NoError(t, err)
	assert.Equal(t, "PENDING", pending, "Reserve must write PENDING marker")

	payload := []byte(`{"status":"SENT"}`)
	require.NoError(t, c.Complete(ctx, "email", "user:1", "key-4", payload))

	after, err := client.Get(ctx, "msg:idem:email:user:1:key-4").Result()
	require.NoError(t, err)
	assert.Equal(t, string(payload), after, "Complete must overwrite PENDING with payload")
}

func TestRedisChecker_Release_DeletesKey(t *testing.T) {
	c, client := newTestChecker(t)
	ctx := context.Background()

	_, _, err := c.Reserve(ctx, "email", "user:1", "key-5")
	require.NoError(t, err)

	require.NoError(t, c.Release(ctx, "email", "user:1", "key-5"))

	exists, err := client.Exists(ctx, "msg:idem:email:user:1:key-5").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists, "Release must delete key")

	// Subsequent Reserve must succeed (acquired=true) — key was released.
	acquired, _, err := c.Reserve(ctx, "email", "user:1", "key-5")
	require.NoError(t, err)
	assert.True(t, acquired, "Reserve after Release must acquire")
}

func TestRedisChecker_Reserve_DifferentChannelsDoNotCollide(t *testing.T) {
	c, _ := newTestChecker(t)
	ctx := context.Background()

	// Same senderID + key, different channel — must NOT collide.
	acquired1, _, err := c.Reserve(ctx, "email", "user:1", "shared-key")
	require.NoError(t, err)
	require.True(t, acquired1)

	acquired2, _, err := c.Reserve(ctx, "sms", "user:1", "shared-key")
	require.NoError(t, err)
	assert.True(t, acquired2, "different channel must not collide")
}
