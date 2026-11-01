package backoff

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Each wait lies between half its step and the whole of it, the step doubling from base with each
// failure and growing no more after the sixth; the draws spread over that span rather than landing
// on one point.
func TestEachWaitLiesBetweenHalfItsStepAndTheWholeOfIt(t *testing.T) {
	const base = 2 * time.Second
	for failures, step := range map[int]time.Duration{
		1: base, 2: 2 * base, 3: 4 * base, 4: 8 * base, 5: 16 * base, 6: 32 * base, 7: 32 * base, 20: 32 * base,
	} {
		least, most := step, time.Duration(0)
		for range 1000 {
			wait := Wait(base, failures)
			if wait < step/2 || wait > step {
				t.Fatalf("Wait(%s, %d) = %s, want between %s and %s", base, failures, wait, step/2, step)
			}
			least, most = min(least, wait), max(most, wait)
		}
		if most-least < step/4 {
			t.Errorf("1000 draws of Wait(%s, %d) spanned %s to %s, want them spread over %s to %s", base, failures, least, most, step/2, step)
		}
	}
}

// Sleep returns at once when its context has ended, with that context's error, and otherwise after
// the whole wait with none.
func TestSleepEndsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
		t.Fatalf("Sleep on an ended context = %v after %s, want context.Canceled at once", err, time.Since(start))
	}
	start = time.Now()
	if err := Sleep(context.Background(), 20*time.Millisecond); err != nil || time.Since(start) < 20*time.Millisecond {
		t.Fatalf("Sleep(20ms) = %v after %s, want nil after the whole wait", err, time.Since(start))
	}
}
