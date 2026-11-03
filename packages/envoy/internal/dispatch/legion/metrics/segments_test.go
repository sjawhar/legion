package metrics

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// segmentHours is one segment's hours of a bar, so a test names a segment rather than a field.
func segmentHours(segments Segments, segment Segment) float64 {
	switch segment {
	case SegmentWaitingOnPerson:
		return segments.WaitingOnPerson
	case SegmentInATurn:
		return segments.InATurn
	case SegmentCIWait:
		return segments.CIWait
	case SegmentWaitingOnChildren:
		return segments.WaitingOnChildren
	case SegmentIdle:
		return segments.Idle
	}
	return 0
}

// segmentOrder is the precedence as the spec writes it, which the table below holds the code to:
// this list is written out here rather than read from `precedence`, so a reordering of the code's
// one list fails these cases instead of moving with them.
var segmentOrder = []Segment{
	SegmentWaitingOnPerson, SegmentInATurn, SegmentCIWait, SegmentWaitingOnChildren, SegmentIdle,
}

// holdSegment makes a segment hold over [hour(1), hour(2)) of a root whose implementing phase runs
// from hour(0) to hour(3). Idle holds wherever nothing else does, so it arranges nothing.
var holdSegment = map[Segment]func(root, child *Issue){
	SegmentWaitingOnPerson: func(root, _ *Issue) {
		root.asked(hour(1), hour(2))
	},
	SegmentInATurn: func(root, _ *Issue) {
		root.observeString(legion.ClaimFieldPrefix+"implementer", hour(1), "working").
			observeString(legion.ClaimFieldPrefix+"implementer", hour(2), "idle")
	},
	SegmentCIWait: func(root, _ *Issue) {
		root.observeString(legion.FieldPullRequestState, hour(1), "open").
			observe(legion.FieldPullRequestRequiresChecks, hour(1), "true").
			observeString(legion.FieldPullRequestState, hour(2), "closed")
	},
	SegmentWaitingOnChildren: func(_, child *Issue) {
		child.observeString(legion.FieldPhase, hour(1), "implementing").
			observeString(legion.FieldPhase, hour(2), "done")
	},
	SegmentIdle: func(_, _ *Issue) {},
}

// claimedHour is the report of a root whose implementing phase runs hour(0) to hour(3), with each
// named segment made to hold over hour(1) to hour(2).
func claimedHour(t *testing.T, segments ...Segment) RoleBar {
	t.Helper()
	root := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(3)).
		observeString(legion.FieldPhase, hour(0), "implementing").
		observeString(legion.FieldPhase, hour(3), "done")
	// The child does nothing of its own until the waiting-on-children rule gives it a phase.
	child := newIssue("INFRA-2").under("INFRA-1")
	for _, segment := range segments {
		holdSegment[segment](root, child)
	}
	population := fixture(hour(-24), hour(24), root, child)
	return barOf(t, Compute(population, ScopeAll, hour(24)), "implementer")
}

// TestEachOrderedPairOfSegmentsGivesTheHourToTheEarlierOne is the precedence itself, over the ten
// ordered pairs of the five segments: with both of a pair holding through the same hour of one
// phase, that hour belongs to whichever the spec names first ("Each hour goes to the first that
// holds"), and the two hours no segment holds through are idle.
func TestEachOrderedPairOfSegmentsGivesTheHourToTheEarlierOne(t *testing.T) {
	pairs := 0
	for first := range segmentOrder {
		for second := first + 1; second < len(segmentOrder); second++ {
			earlier, later := segmentOrder[first], segmentOrder[second]
			pairs++
			t.Run(string(earlier)+" before "+string(later), func(t *testing.T) {
				bar := claimedHour(t, earlier, later)
				want := Segments{Idle: 2}
				switch earlier {
				case SegmentWaitingOnPerson:
					want.WaitingOnPerson = 1
				case SegmentInATurn:
					want.InATurn = 1
				case SegmentCIWait:
					want.CIWait = 1
				case SegmentWaitingOnChildren:
					want.WaitingOnChildren = 1
				}
				if bar.Segments != want {
					t.Fatalf("with %s and %s both holding, the bar is %+v, want the hour on %s: %+v",
						earlier, later, bar.Segments, earlier, want)
				}
				// Idle is the two hours no rule holds through, so only a rule is held to zero.
				if later != SegmentIdle && segmentHours(bar.Segments, later) != 0 {
					t.Errorf("%s claimed %g hours of a phase %s already holds through",
						later, segmentHours(bar.Segments, later), earlier)
				}
			})
		}
	}
	if pairs != 10 {
		t.Fatalf("the five segments make %d ordered pairs, want 10", pairs)
	}
}

// TestASegmentAloneClaimsItsHour: each of the four rules claims its hour when it is the only one
// holding, so the pair cases above are not all passing for want of any claim at all.
func TestASegmentAloneClaimsItsHour(t *testing.T) {
	for _, segment := range segmentOrder {
		t.Run(string(segment), func(t *testing.T) {
			bar := claimedHour(t, segment)
			want := 1.0
			if segment == SegmentIdle {
				want = 3
			}
			if got := segmentHours(bar.Segments, segment); got != want {
				t.Fatalf("%s alone claimed %g hours, want %g: %+v", segment, got, want, bar.Segments)
			}
		})
	}
}

// TestCIWaitNeedsAnOpenPullRequestARequiredCheckAndNoVerdict: the three facts of segment 3, each
// withheld in turn. A base branch that requires no check is a wait on nothing, and a verdict the
// daemon recorded ends the wait.
func TestCIWaitNeedsAnOpenPullRequestARequiredCheckAndNoVerdict(t *testing.T) {
	for _, one := range []struct {
		name    string
		arrange func(root *Issue)
		want    float64
	}{
		{"open, required and unjudged", func(root *Issue) {
			root.observeString(legion.FieldPullRequestState, hour(1), "open").
				observe(legion.FieldPullRequestRequiresChecks, hour(1), "true")
		}, 2},
		{"no required check", func(root *Issue) {
			root.observeString(legion.FieldPullRequestState, hour(1), "open").
				observe(legion.FieldPullRequestRequiresChecks, hour(1), "false")
		}, 0},
		{"the daemon sent no requiresChecks at all", func(root *Issue) {
			root.observeString(legion.FieldPullRequestState, hour(1), "open")
		}, 0},
		{"a verdict arrived", func(root *Issue) {
			root.observeString(legion.FieldPullRequestState, hour(1), "open").
				observe(legion.FieldPullRequestRequiresChecks, hour(1), "true").
				observeString(legion.FieldPullRequestChecksVerdict, hour(2), "green")
		}, 1},
		{"the pull request is closed", func(root *Issue) {
			root.observeString(legion.FieldPullRequestState, hour(1), "closed").
				observe(legion.FieldPullRequestRequiresChecks, hour(1), "true")
		}, 0},
	} {
		t.Run(one.name, func(t *testing.T) {
			root := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(3)).
				observeString(legion.FieldPhase, hour(0), "implementing").
				observeString(legion.FieldPhase, hour(3), "done")
			one.arrange(root)
			bar := barOf(t, Compute(fixture(hour(-24), hour(24), root), ScopeAll, hour(24)), "implementer")
			if bar.Segments.CIWait != one.want {
				t.Fatalf("CI wait = %g hours, want %g: %+v", bar.Segments.CIWait, one.want, bar.Segments)
			}
		})
	}
}

// TestTheRootRecordedInAwaitingPersonClaimsItsChildsHours is segment 1's other half: the wait the
// daemon recorded on the root, which claims the hours of a child in a phase of its own.
func TestTheRootRecordedInAwaitingPersonClaimsItsChildsHours(t *testing.T) {
	root := newIssue("INFRA-1").worked("architect", hour(0)).
		observeString(legion.FieldPhase, hour(0), "planning").
		observeString(legion.FieldPhase, hour(1), "awaiting_person").
		observeString(legion.FieldPhase, hour(2), "planning")
	child := newIssue("INFRA-2").under("INFRA-1").worked("implementer", hour(0)).closedAt(hour(3)).
		observeString(legion.FieldPhase, hour(0), "implementing").
		observeString(legion.FieldPhase, hour(3), "done")
	bar := barOf(t, Compute(fixture(hour(-24), hour(24), root, child), ScopeAll, hour(24)), "implementer")
	if bar.Segments.WaitingOnPerson != 1 || bar.Segments.Idle != 2 {
		t.Fatalf("the child's bar is %+v, want the root's awaiting_person hour and two idle", bar.Segments)
	}
}

// TestAnAskTurnEndsAtTheWaitTheDaemonRecordedAsAnswered: an ask nothing answered in the event log
// still ends where the daemon's own `waitingOn.answeredAt` says the person answered.
func TestAnAskTurnEndsAtTheWaitTheDaemonRecordedAsAnswered(t *testing.T) {
	answered := hour(2).Format(time.RFC3339)
	root := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(3)).
		observeString(legion.FieldPhase, hour(0), "implementing").
		observeString(legion.FieldPhase, hour(3), "done").
		observe(legion.FieldWaitingOn, hour(1), `{"since":"`+hour(1).Format(time.RFC3339)+`","answeredAt":"`+answered+`"}`).
		asked(hour(1), hour(3))
	bar := barOf(t, Compute(fixture(hour(-24), hour(24), root), ScopeAll, hour(24)), "implementer")
	if bar.Segments.WaitingOnPerson != 1 || bar.Segments.Idle != 2 {
		t.Fatalf("the bar is %+v, want the one hour to the answer and the rest idle", bar.Segments)
	}
}
