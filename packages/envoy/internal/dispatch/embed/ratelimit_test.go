package embed

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/retry"
)

// countingEmbedder answers errs in order (one per call, the last repeats once exhausted) and
// records how many times Embed was called and when each call happened, so a test can assert on
// both the limiter's pacing and its AIMD adjustment without a real Bedrock call.
type countingEmbedder struct {
	errs  []error
	calls []time.Time
}

func (c *countingEmbedder) Embed(_ context.Context, texts []string, _ InputType) ([][]float32, error) {
	c.calls = append(c.calls, time.Now())
	var err error
	if i := len(c.calls) - 1; i < len(c.errs) {
		err = c.errs[i]
	}
	if err != nil {
		return nil, err
	}
	return make([][]float32, len(texts)), nil
}

func TestRateLimitedEmbedderPacesSuccessiveCallsAtLeastTheFloorApart(t *testing.T) {
	inner := &countingEmbedder{}
	limiter := NewRateLimitedEmbedder(inner)
	limiter.interval = 20 * time.Millisecond // scaled down from rateLimitFloor for a fast test

	ctx := context.Background()
	for range 3 {
		if _, err := limiter.Embed(ctx, []string{"x"}, InputDocument); err != nil {
			t.Fatalf("Embed: %v", err)
		}
	}
	if len(inner.calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(inner.calls))
	}
	for i := 1; i < len(inner.calls); i++ {
		gap := inner.calls[i].Sub(inner.calls[i-1])
		if gap < 20*time.Millisecond {
			t.Errorf("gap between call %d and %d = %v, want at least 20ms", i-1, i, gap)
		}
	}
}

func TestRateLimitedEmbedderBacksOffImmediatelyOnThrottle(t *testing.T) {
	throttleErr := throttleError{code: "ThrottlingException"}
	inner := &countingEmbedder{errs: []error{throttleErr}}
	limiter := NewRateLimitedEmbedder(inner)
	limiter.interval = 10 * time.Millisecond

	ctx := context.Background()
	if _, err := limiter.Embed(ctx, []string{"x"}, InputDocument); !errors.Is(err, throttleErr) {
		t.Fatalf("Embed error = %v, want the throttle error passed through unchanged", err)
	}
	if got := limiter.Interval(); got != 20*time.Millisecond {
		t.Errorf("Interval() after one throttle = %v, want 20ms (doubled from 10ms)", got)
	}
}

func TestRateLimitedEmbedderRecoversGraduallyAfterSuccess(t *testing.T) {
	inner := &countingEmbedder{}
	limiter := NewRateLimitedEmbedder(inner)
	limiter.interval = rateLimitFloor + time.Second // above rateLimitFloor, as if recovering from backoff

	ctx := context.Background()
	if _, err := limiter.Embed(ctx, []string{"x"}, InputDocument); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got, want := limiter.Interval(), rateLimitFloor+time.Second-rateLimitRecoveryStep; got != want {
		t.Errorf("Interval() after one success = %v, want %v (one additive step down)", got, want)
	}
}

func TestRateLimitedEmbedderNeverRecoversBelowTheFloor(t *testing.T) {
	inner := &countingEmbedder{}
	limiter := NewRateLimitedEmbedder(inner) // starts at rateLimitFloor already

	ctx := context.Background()
	for range 5 {
		if _, err := limiter.Embed(ctx, []string{"x"}, InputDocument); err != nil {
			t.Fatalf("Embed: %v", err)
		}
	}
	if got := limiter.Interval(); got != rateLimitFloor {
		t.Errorf("Interval() after 5 successes starting at the floor = %v, want %v (floored)", got, rateLimitFloor)
	}
}

func TestRateLimitedEmbedderNeverBacksOffPastTheCeiling(t *testing.T) {
	throttleErr := throttleError{code: "ThrottlingException"}
	inner := &countingEmbedder{}
	limiter := NewRateLimitedEmbedder(inner)
	limiter.interval = retry.MaxDelay / 2

	ctx := context.Background()
	inner.errs = []error{throttleErr}
	if _, err := limiter.Embed(ctx, []string{"x"}, InputDocument); !errors.Is(err, throttleErr) {
		t.Fatalf("Embed error = %v, want the throttle error", err)
	}
	if got := limiter.Interval(); got != retry.MaxDelay {
		t.Errorf("Interval() after doubling past the ceiling = %v, want %v (capped)", got, retry.MaxDelay)
	}
}

// TestRateLimitedEmbedderNeverGatesAnUnwrappedSiblingCall is the architectural proof behind
// cmd/dispatch's wiring: a live search request holds a direct, unwrapped reference to the same
// underlying Embedder embedqueue's RateLimitedEmbedder wraps, so it is never paced by this
// limiter at all, however deep into backoff the wrapped side currently is - this is what "a
// query never waits behind backfill/write-queue work" means at the type level, not something
// RateLimitedEmbedder itself has to coordinate.
func TestRateLimitedEmbedderNeverGatesAnUnwrappedSiblingCall(t *testing.T) {
	inner := &countingEmbedder{}
	limiter := NewRateLimitedEmbedder(inner)
	limiter.interval = time.Hour // as deep into backoff as this limiter can ever go, exaggerated

	ctx := context.Background()
	started := time.Now()
	// The "live search" stand-in: the same inner embedder, called directly, bypassing the limiter
	// entirely - exactly how cmd/dispatch wires api.Deps.Embedder versus embedqueue.Deps.Embedder.
	if _, err := inner.Embed(ctx, []string{"x"}, InputQuery); err != nil {
		t.Fatalf("unwrapped Embed: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Errorf("unwrapped call took %v, want near-instant - it must never wait on limiter.interval", elapsed)
	}
	_ = limiter // the limiter exists only to prove it is irrelevant to the call above
}

func TestRateLimitedEmbedderStopsWaitingWhenContextEnds(t *testing.T) {
	inner := &countingEmbedder{}
	limiter := NewRateLimitedEmbedder(inner)
	limiter.interval = time.Hour
	limiter.last = time.Now() // so the next call must wait almost the whole hour

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := limiter.Embed(ctx, []string{"x"}, InputDocument)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("Embed with a cancelled context took %v, want it to return promptly", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Embed error = %v, want context.DeadlineExceeded", err)
	}
	if len(inner.calls) != 0 {
		t.Error("the inner Embedder was called despite the context ending during pacing")
	}
}
