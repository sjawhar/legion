package shim

import (
	"testing"
	"time"
)

// warmUpDrain is what the role's stop grace leaves once the agent's own grace and the stdout drain
// are spent — with the daemon's defaults (`worker_stop_timeout_seconds` 10 s, which a pod's
// launcher and its shim both get, and the shim's 5 s grace), 4 s — and never under MinWarmUpDrain,
// the codegraph child's own kill grace plus the second its reaping and the lease's release take.
// Two invariants pin it: (i) the floor holds for every input; (ii) the sum the stop can take —
// DefaultGrace, DrainTimeout, the drain — fits inside the stop grace exactly from MinStopGrace up,
// and exceeds it one second below, so MinStopGrace is the smallest stop grace the stop fits and a
// smaller one is the configuration the daemon refuses rather than one the launcher kills the shim
// through with the warm-up's lease held. A warmUpDrain without `- DrainTimeout` fails (ii):
// (MinStopGrace, DefaultGrace) returns 4 s and the sum is 10 s, past the 9 s grace. One with 0 for
// the floor fails (i): (6s, 5s) returns 0.
func TestWarmUpDrainIsWhatTheStopGraceLeavesAndNeverUnderTheFloor(t *testing.T) {
	for name, tc := range map[string]struct {
		stopGrace, grace, want time.Duration
	}{
		"the defaults":                 {DefaultStopGrace, DefaultGrace, 4 * time.Second},
		"a pod's own":                  {20 * time.Second, 5 * time.Second, 14 * time.Second},
		"the smallest grace that fits": {MinStopGrace, DefaultGrace, 3 * time.Second},
		"a grace that leaves nothing":  {6 * time.Second, 5 * time.Second, 3 * time.Second},
		"a grace under the agent's":    {3500 * time.Millisecond, 3 * time.Second, 3 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			got := warmUpDrain(tc.stopGrace, tc.grace)
			if got != tc.want {
				t.Errorf("warmUpDrain(%v, %v) = %v, want %v", tc.stopGrace, tc.grace, got, tc.want)
			}
			if got < MinWarmUpDrain {
				t.Errorf("invariant (i) broken: warmUpDrain(%v, %v) = %v, under the floor MinWarmUpDrain %v", tc.stopGrace, tc.grace, got, MinWarmUpDrain)
			}
		})
	}

	t.Run("the stop fits inside every grace from MinStopGrace up", func(t *testing.T) {
		for _, g := range []time.Duration{MinStopGrace, DefaultStopGrace, 20 * time.Second} {
			if sum := DefaultGrace + DrainTimeout + warmUpDrain(g, DefaultGrace); sum > g {
				t.Fatalf("invariant (ii) broken: a stop under warmUpDrain(%v, DefaultGrace) can take %v, past the stop grace %v", g, sum, g)
			}
		}
	})
	t.Run("and not inside one second less", func(t *testing.T) {
		g := MinStopGrace - time.Second
		if sum := DefaultGrace + DrainTimeout + warmUpDrain(g, DefaultGrace); sum <= g {
			t.Fatalf("invariant (ii) broken: a stop under warmUpDrain(%v, DefaultGrace) takes %v, inside a stop grace under MinStopGrace %v; MinStopGrace is not the smallest grace the stop fits", g, sum, MinStopGrace)
		}
	})
}
