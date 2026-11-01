package redeliver

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
)

// FixJitter makes every wait Run leaves after a sweep the interval plus draw(interval/10), where
// duty.Jittered draws at random in [0, interval/10), until the test ends.
func FixJitter(t *testing.T, draw func(n time.Duration) time.Duration) {
	t.Helper()
	saved := jittered
	jittered = func(d time.Duration) time.Duration { return d + draw(d/10) }
	t.Cleanup(func() { jittered = saved })
}

// RunUnderLease is Run with exclusive in place of duty.Exclusive and no lease pool, for a test of
// the schedule alone: the loop's waits and the moment each sweep is due, with no statement of a
// real lease between them.
func (s *Sweeper) RunUnderLease(ctx context.Context, interval time.Duration, exclusive duty.ExclusiveFunc) {
	s.run(ctx, interval, nil, exclusive)
}
