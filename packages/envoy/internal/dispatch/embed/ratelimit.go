package embed

import (
	"context"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/retry"
)

const (
	// rateLimitFloor is the fastest RateLimitedEmbedder ever paces calls: the steady-state ceiling
	// it climbs back to after a run of successful batches. Chosen to leave headroom below the
	// account's sustainable rate for a live search's own query embedding, which never goes
	// through this limiter at all (cmd/dispatch wires the unwrapped *Client there). The slowest
	// this limiter ever paces calls to, reached only after several consecutive throttled batches
	// in a row, is retry.MaxDelay (not a duplicate constant of its own): the same ceiling
	// embedqueue's own batchBackoff already uses, so the two mechanisms agree on how bad
	// "sustained throttling" gets before both are maxed out.
	rateLimitFloor = 750 * time.Millisecond
	// rateLimitRecoveryStep is how much one successful (non-throttled) call shortens the interval
	// by - additive increase, the slow half of AIMD: recovering one small step at a time, rather
	// than snapping back to rateLimitFloor the moment Bedrock answers once, means a provider that
	// is still only barely keeping up does not immediately get hit with a burst back at full rate.
	rateLimitRecoveryStep = 50 * time.Millisecond
	// rateLimitBackoffFactor is how much one throttled call lengthens the interval by -
	// multiplicative decrease, the fast half of AIMD: back off hard and immediately on the first
	// sign the account's shared Bedrock quota is exceeded, the same shape TCP congestion control
	// uses for the same reason (a shared resource signalled "too much", not a per-caller limit).
	rateLimitBackoffFactor = 2
)

// RateLimitedEmbedder paces calls to an inner Embedder so Dispatch's background embedding work -
// internal/dispatch/embedqueue's poller and a backfill run, its only two callers - never
// saturates the account's Bedrock quota to the point a live search request's own query embedding
// gets caught in the same throttling. cmd/dispatch wires this wrapper only around the Embedder it
// hands to embedqueue.Deps; api.Deps keeps the unwrapped Client directly, so a live query is never
// paced by this limiter and never queues behind background work, however deep into backoff that
// work currently is - bounded instead only by its own ordinary searchEmbedTimeout.
//
// The pacing is AIMD (additive increase, multiplicative decrease): it starts at rateLimitFloor,
// lengthens its interval by rateLimitBackoffFactor the moment a call comes back throttled (fast,
// immediate backoff on the first sign of shared-resource pressure), and shortens it by
// rateLimitRecoveryStep on every call that was not throttled (slow, incremental recovery, so a
// provider that is only barely keeping up is not immediately hit with a burst back at full rate).
// rateLimitFloor is deliberately below the account's sustainable rate, which is what
// "leaves headroom" means in practice: background work's own steady-state ceiling stops short of
// the full quota, leaving room for a live query's call - never gated by this limiter at all - to
// get through even while background work is actively pacing itself near that ceiling.
type RateLimitedEmbedder struct {
	inner Embedder

	mu       sync.Mutex
	interval time.Duration
	last     time.Time
}

// NewRateLimitedEmbedder wraps inner for background callers; see RateLimitedEmbedder's own
// doc comment for who should (embedqueue) and should not (search.go's live query path) hold one.
func NewRateLimitedEmbedder(inner Embedder) *RateLimitedEmbedder {
	return &RateLimitedEmbedder{inner: inner, interval: rateLimitFloor}
}

// Embed paces itself to the limiter's current interval, then calls the inner Embedder and
// adjusts that interval from the result: longer (multiplicative) on a throttled error, shorter
// (additive, floored at rateLimitFloor) on anything else - an ordinary success or a non-throttled
// failure both count as "Bedrock was not signalling capacity pressure this time."
func (r *RateLimitedEmbedder) Embed(ctx context.Context, texts []string, inputType InputType) ([][]float32, error) {
	if !r.pace(ctx) {
		return nil, ctx.Err()
	}
	vectors, err := r.inner.Embed(ctx, texts, inputType)
	r.adjust(err)
	return vectors, err
}

// pace blocks until this call is at least Interval() after the previous one started, or returns
// false without waiting out the rest of it if ctx ends first. It reserves its own slot under the
// lock - advancing last by interval (not to time.Now()) when a wait is owed, so a second caller
// computing its own wait immediately after sees the reservation already made - then releases the
// lock before actually sleeping: holding the lock across the sleep would mean one caller's
// cancellation (ctx.Done() firing) still has to wait for the mutex a completely unrelated
// caller's own, unexpired sleep is holding, which defeats ctx ending this call promptly. A caller
// the way embedqueue uses this (one poller goroutine, one backfill goroutine, never both against
// the same limiter at once today) never contends it, but correctness here does not depend on
// that - two concurrent callers reserving distinct, evenly-spaced slots up front is correct
// regardless of how many goroutines ever call Embed concurrently.
func (r *RateLimitedEmbedder) pace(ctx context.Context) bool {
	r.mu.Lock()
	wait := r.interval - time.Since(r.last)
	if wait > 0 {
		r.last = r.last.Add(r.interval)
	} else {
		r.last = time.Now()
	}
	r.mu.Unlock()
	if wait <= 0 {
		return true
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (r *RateLimitedEmbedder) adjust(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if IsThrottled(err) {
		r.interval *= rateLimitBackoffFactor
		if r.interval > retry.MaxDelay {
			r.interval = retry.MaxDelay
		}
		return
	}
	r.interval -= rateLimitRecoveryStep
	if r.interval < rateLimitFloor {
		r.interval = rateLimitFloor
	}
}

// Interval reports the limiter's current pacing interval, for logging and tests.
func (r *RateLimitedEmbedder) Interval() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interval
}
