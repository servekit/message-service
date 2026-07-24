// Package idempotency provides a Redis-backed idempotency check for send RPCs.
//
// The check is a three-phase reservation:
//   - Reserve: atomic SETNX claims the key for one caller
//   - Complete: store the response payload (overwrites the PENDING marker)
//   - Release: delete the key so the next caller can retry (used on failure)
//
// Failures are not cached — vendor rejection or transient error releases the
// reservation so the caller can retry the same idempotency_key. The only
// state we keep is "in flight" (PENDING marker) and "completed" (JSON payload).
package idempotency

import (
	"context"
	"time"
)

// Config is the resolved configuration consumed by NewRedisChecker. TTLs are
// per-channel (email vs sms); KeyPrefix namespaces the Redis key space.
type Config struct {
	KeyPrefix string
	EmailTTL  time.Duration
	SMSTTL    time.Duration
}

// Checker reserves an idempotency key and stores the outcome payload.
// Implementations must be safe for concurrent use. TTL is resolved from
// Config (per-channel) at construction time — no per-call override.
type Checker interface {
	// Reserve atomically claims the key.
	//
	//   - acquired=true, payload=nil: reservation succeeded; caller MUST call
	//     Complete (success) or Release (failure) afterwards.
	//   - acquired=false, payload!=nil: key already completed within TTL;
	//     caller returns the cached payload as the response.
	//   - acquired=false, payload=nil: another caller holds the reservation
	//     (in-flight); caller returns ErrIdempotencyConflict (defined in
	//     pkg/xcodes — kept out of this package to avoid a cycle).
	//
	// channel is "email" or "sms", used to resolve per-channel TTL and in
	// the Redis key namespace.
	Reserve(ctx context.Context, channel, senderID, key string) (acquired bool, payload []byte, err error)

	// Complete stores the payload and overwrites the PENDING marker set by
	// Reserve. Safe to call only after a successful Reserve on the same key.
	Complete(ctx context.Context, channel, senderID, key string, payload []byte) error

	// Release deletes the reservation. Used when the send fails so the next
	// caller can retry the same key. No-op if the key does not exist.
	Release(ctx context.Context, channel, senderID, key string) error
}
