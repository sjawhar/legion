package shim

import (
	"testing"
	"time"
)

// warmUpDrain is what the role's stop grace leaves once the agent's own grace and the stdout drain
// are spent: with the daemon's defaults (`worker_stop_timeout_seconds` 10 s, which a pod's launcher
// and its shim both get, and the shim's 5 s grace), 4 s; never negative, so a runtime whose stop
// grace leaves nothing waits not at all. The sum the stop can take — Grace, drainTimeout, this —
// must stay inside the stop grace, or the launcher kills the shim with the warm-up's lease held,
// the failure the wait prevents.
func TestWarmUpDrainIsWhatTheStopGraceLeaves(t *testing.T) {
	for name, tc := range map[string]struct {
		stopGrace, grace, want time.Duration
	}{
		"the defaults":                {DefaultStopGrace, DefaultGrace, 4 * time.Second},
		"a pod's own":                 {20 * time.Second, 5 * time.Second, 14 * time.Second},
		"a grace that leaves nothing": {2 * time.Second, 5 * time.Second, 0},
	} {
		t.Run(name, func(t *testing.T) {
			if got := warmUpDrain(tc.stopGrace, tc.grace); got != tc.want {
				t.Fatalf("warmUpDrain(%v, %v) = %v, want %v", tc.stopGrace, tc.grace, got, tc.want)
			}
			if tc.grace+drainTimeout+warmUpDrain(tc.stopGrace, tc.grace) > max(tc.stopGrace, tc.grace+drainTimeout) {
				t.Fatalf("a stop under warmUpDrain(%v, %v) can take %v, past the stop grace", tc.stopGrace, tc.grace, tc.grace+drainTimeout+warmUpDrain(tc.stopGrace, tc.grace))
			}
		})
	}
}
