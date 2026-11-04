package metrics

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// TestTheSevenBarsPartitionTheWorkTimeOnAnObservedTimeline: the spec's "Parked and pre-work hours
// in phase time" ends "So the bars of an issue add up to its work time, and the two cards agree",
// and the whole of metric 2 rests on it — a reader of the Legion section sees the seven bars and
// the work time beside each other.
//
// dispatch://INFRA-979's corpus cannot check that invariant on the observed timeline. Every one of
// its 289 finished issues carries a single trailing `done` mark, so every timeline the A2
// comparison exercises is the event-log fallback. The observed timeline is the one production
// runs on from the sampler's first pass, and it is reached by fixtures alone. This holds the
// invariant over the observed shapes a sampler actually produces, and asserts nothing about how
// the hours are split between the bars, which the spec's other rules decide: a shape that
// disagreed with this test would be losing or double-counting an hour of an issue's life.
func TestTheSevenBarsPartitionTheWorkTimeOnAnObservedTimeline(t *testing.T) {
	cases := []struct {
		name  string
		issue *Issue
	}{
		{
			// The sampler meets the issue after its first worker event: two hours of work time
			// stand before the first observation, which is the spec's "Work hours before any
			// phase mark".
			name: "the first observation follows the first worker event",
			issue: newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(8)).
				observeString(legion.FieldPhase, hour(2), "implementing").
				observeString(legion.FieldPhase, hour(5), "testing"),
		},
		{
			// Parked mid-flight: those hours are on no bar and out of the work time, so the
			// identity must hold over what is left of a broken-up life.
			name: "a park between two observed phases",
			issue: newIssue("INFRA-2").worked("implementer", hour(0)).closedAt(hour(9)).
				observeString(legion.FieldPhase, hour(1), "implementing").
				statusAt(hour(3), "backlog").
				statusAt(hour(6), "in_progress").
				observeString(legion.FieldPhase, hour(7), "reviewing"),
		},
		{
			// An admitted stretch the issue holds a slot through is its design; the rest of it is
			// the waiting line, whose hours are on no bar.
			name: "an admitted stretch, part of it holding a slot",
			issue: newIssue("INFRA-3").worked("architect", hour(0)).closedAt(hour(8)).
				observeString(legion.FieldPhase, hour(0), "admitted").
				observe(legion.FieldSlot, hour(2), "1").
				observeString(legion.FieldPhase, hour(4), "planning").
				observeString(legion.FieldPhase, hour(6), "implementing"),
		},
		{
			// Back in the queue: the issue leaves `admitted` and returns to it, which the spec's
			// "Back in the queue" rule takes out of the work time with observations present.
			name: "back in the queue after it first left",
			issue: newIssue("INFRA-4").worked("architect", hour(0)).closedAt(hour(10)).
				observeString(legion.FieldPhase, hour(0), "admitted").
				observe(legion.FieldSlot, hour(0), "0").
				observeString(legion.FieldPhase, hour(2), "implementing").
				observe(legion.FieldSlot, hour(4), "null").
				observeString(legion.FieldPhase, hour(4), "admitted").
				observeString(legion.FieldPhase, hour(7), "implementing"),
		},
		{
			// A phase no role works, which is the one thing the "no role" bar holds.
			name: "a stretch waiting on a person",
			issue: newIssue("INFRA-5").worked("implementer", hour(0)).closedAt(hour(8)).
				observeString(legion.FieldPhase, hour(1), "implementing").
				observeString(legion.FieldPhase, hour(3), "awaiting_person").
				observeString(legion.FieldPhase, hour(6), "implementing"),
		},
	}

	for _, each := range cases {
		t.Run(each.name, func(t *testing.T) {
			population := fixture(hour(-24), hour(24), each.issue)
			report := Compute(population, ScopeAll, hour(24))
			if len(report.WorkTime.Issues) != 1 {
				t.Fatalf("work-time rows = %d, want the one issue", len(report.WorkTime.Issues))
			}
			work := report.WorkTime.Issues[0].Hours

			var bars, segments float64
			for _, bar := range report.PhaseTime.Bars {
				bars += bar.Hours
				segments += bar.Segments.WaitingOnPerson + bar.Segments.InATurn +
					bar.Segments.CIWait + bar.Segments.WaitingOnChildren + bar.Segments.Idle
			}
			if bars != work {
				t.Errorf("the seven bars hold %g h against a work time of %g h: %g h of this issue's life is on no bar, or on two",
					bars, work, bars-work)
			}
			if segments != bars {
				t.Errorf("the five segments hold %g h against the seven bars' %g h: each hour of a bar goes to exactly one segment",
					segments, bars)
			}
		})
	}
}
