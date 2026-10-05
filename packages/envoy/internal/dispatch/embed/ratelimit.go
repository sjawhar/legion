package embed

import (
	"context"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"golang.org/x/time/rate"
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
	limiter  *rate.Limiter
}

// NewRateLimitedEmbedder wraps inner for background callers; see RateLimitedEmbedder's own
// doc comment for who should (embedqueue) and should not (search.go's live query path) hold one.
func NewRateLimitedEmbedder(inner Embedder) *RateLimitedEmbedder {
	return &RateLimitedEmbedder{
		inner:    inner,
		interval: rateLimitFloor,
		limiter:  rate.NewLimiter(rate.Every(rateLimitFloor), 1),
	}
}

// Embed paces itself to the limiter's current interval via golang.org/x/time/rate's own
// reservation bookkeeping (burst 1, so every call waits out the full interval since the last
// one) rather than a hand-rolled elapsed-time computation, then calls the inner Embedder and
// adjusts that interval from the result: longer (multiplicative) on a throttled error, shorter
// (additive, floored at rateLimitFloor) on anything else - an ordinary success or a non-throttled
// failure both count as "Bedrock was not signalling capacity pressure this time." It reserves
// with Reserve rather than calling the package's own ctx-aware Wait, because Wait fails fast with
// its own error the moment it can tell the reservation's delay would outlast ctx's deadline,
// without ever waiting for ctx to actually end - and embedqueue's callers depend on ctx.Err()
// being set by the time this call returns (handleGroupFailure's own ctx.Err() check, which every
// embedRows loop iteration relies on), to tell a cancelled wait apart from every other failure.
// Reserve, by contrast, hands back a delay to wait out ourselves, so the select below only ever
// returns once ctx.Done() has genuinely fired.
func (r *RateLimitedEmbedder) Embed(ctx context.Context, texts []string, inputType InputType) ([][]float32, error) {
	reservation := r.limiter.Reserve()
	if delay := reservation.Delay(); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			reservation.Cancel()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	vectors, err := r.inner.Embed(ctx, texts, inputType)
	r.adjust(err)
	return vectors, err
}

// adjust moves interval (additive recovery, multiplicative backoff, as RateLimitedEmbedder's own
// doc comment describes) and pushes a changed value onto the rate.Limiter that Embed actually
// waits on; interval itself stays the authoritative, exact state (Interval and the tests read it
// directly) rather than being derived back out of the limiter's floating-point Limit.
// SetLimit takes the limiter's own internal mutex and re-derives its token-bucket state even
// when the value is identical, so adjust skips it at the floor and ceiling - the two steady
// states a run of consecutive successes or throttles settles into and stays at call after call.
func (r *RateLimitedEmbedder) adjust(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	before := r.interval
	if IsThrottled(err) {
		r.interval *= rateLimitBackoffFactor
		if r.interval > retry.MaxDelay {
			r.interval = retry.MaxDelay
		}
	} else {
		r.interval -= rateLimitRecoveryStep
		if r.interval < rateLimitFloor {
			r.interval = rateLimitFloor
		}
	}
	if r.interval != before {
		r.limiter.SetLimit(rate.Every(r.interval))
	}
}

// Interval reports the limiter's current pacing interval, for logging and tests.
func (r *RateLimitedEmbedder) Interval() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interval
}

// setInterval forces the limiter's interval (and the rate.Limiter that actually paces Embed) to
// d, bypassing the floor/ceiling and AIMD step adjust applies - test-only, for scaling the floor
// down to something a test can wait out in milliseconds rather than rateLimitFloor's 750ms.
func (r *RateLimitedEmbedder) setInterval(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.interval = d
	r.limiter.SetLimit(rate.Every(d))
}
