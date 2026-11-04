package metrics

import "testing"

// TestAnAskCountsOnlyForTheIssueItWasOpenedOn: spec version 8's "Whose ask counts as waiting on
// a person" reads "An ask counts for an issue when it is opened **on that issue** by any Legion
// session of the issue's tree". Two conditions, not one: the opener must be of the issue's tree,
// and the ask must be on that issue. `keepTreeAsks` applies the first and attaches each turn to
// the issue it was opened on, and then `waitingOnPersonSpans` collects the ask turns of every
// issue of the measured issue's tree (`for _, issue := range view.tree()`), so one ask on one
// child is waiting on a person for its root and for every sibling too.
//
// Over dispatch://INFRA-979's corpus that is the difference between 1064.62 h of waiting on a
// person and 381.42 h: 373 asks counted once each against the same 373 spread across their
// trees. The fixture below is the smallest case of it: one ask, on one of two siblings.
func TestAnAskCountsOnlyForTheIssueItWasOpenedOn(t *testing.T) {
	root := newIssue("INFRA-1").
		worked("architect", hour(0)).
		worked("planner", hour(1)).
		closedAt(hour(6))
	// Two children of one tree, each worked by its own implementer over the same hours, so a
	// turn credited tree-wide lands on both.
	asked := newIssue("INFRA-2").under("INFRA-1").
		worked("implementer", hour(0)).
		closedAt(hour(6)).
		asked(hour(2), hour(4))
	sibling := newIssue("INFRA-3").under("INFRA-1").
		worked("implementer", hour(0)).
		closedAt(hour(6))

	report := Compute(fixture(hour(-24), hour(24), root, asked, sibling), ScopeAll, hour(24))

	var person float64
	for _, bar := range report.PhaseTime.Bars {
		person += bar.Segments.WaitingOnPerson
	}
	if person != 2 {
		t.Errorf("waiting on a person over every bar = %g h, want 2: one ask stood on a person's turn for two hours, on INFRA-2 alone, and an ask counts for the issue it was opened on",
			person)
	}
}

// TestAReAdmissionDoesNotReopenTheDesignPhase: spec version 8's "Which issues have a design
// phase" reads "The design phase runs from the issue's first slot, or else from that architect's
// first event, **to the first planner event**". One interval, ending where planning begins, so
// nothing later can reopen it. `statusPhase` instead maps every entry into `in_progress` out of
// nothing, the queue or parked to the design phase whenever the issue has architect events, so an
// issue parked mid-flight and re-admitted goes back to design for the rest of its life. Over
// dispatch://INFRA-979's corpus the architect bar reads 714.65 h against 404.97 h, of which only
// 97.12 h is the one issue whose architect spoke after its planner; the rest is this.
func TestAReAdmissionDoesNotReopenTheDesignPhase(t *testing.T) {
	issue := newIssue("INFRA-1").
		worked("architect", hour(0)).
		worked("planner", hour(1)).
		worked("implementer", hour(2)).
		closedAt(hour(7)).
		// Admitted out of the queue, parked mid-implementing, then admitted again.
		statusAt(hour(0), "in_progress").
		statusAt(hour(3), "backlog").
		statusAt(hour(5), "in_progress").
		statusAt(hour(7), "done")

	report := Compute(fixture(hour(-24), hour(24), issue), ScopeAll, hour(24))

	// Work time is the five hours outside the park: design 0-1, planning 1-2, implementing 2-3
	// and 5-7.
	if got := barOf(t, report, "architect").Hours; got != 1 {
		t.Errorf("the architect bar = %g h, want 1: design runs from the architect's first event to the first planner event, and the re-admission at hour 5 reopens nothing",
			got)
	}
	if got := barOf(t, report, "implementer").Hours; got != 3 {
		t.Errorf("the implementer bar = %g h, want 3: hours 2-3 and the two after the park", got)
	}
}
