package metrics

import (
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// TestNoQueueOrParkedHourEntersTheWorkTime: the work time runs from the issue's first worker
// event to its close, less the hours it sat in backlog or icebox and the hours its observed
// phase was `admitted` with no slot held.
func TestNoQueueOrParkedHourEntersTheWorkTime(t *testing.T) {
	issue := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(10)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(2), "backlog").
		statusAt(hour(4), "in_progress").
		observeString(legion.FieldPhase, hour(-2), "admitted").
		observeString(legion.FieldPhase, hour(0), "planning").
		observeString(legion.FieldPhase, hour(5), "admitted").
		observeString(legion.FieldPhase, hour(6), "implementing").
		observeString(legion.FieldPhase, hour(10), "done")
	report := Compute(fixture(hour(-24), hour(24), issue), ScopeAll, hour(24))
	if report.Finished != 1 || len(report.WorkTime.Issues) != 1 {
		t.Fatalf("the report counted %d finished issues and %d rows, want one of each", report.Finished, len(report.WorkTime.Issues))
	}
	row := report.WorkTime.Issues[0]
	if row.Hours != 7 || row.ParkedHours != 2 || row.QueuedHours != 1 {
		t.Fatalf("work time = %g hours (parked %g, queued %g), want 7 of 10 hours with 2 parked and 1 back in the queue",
			row.Hours, row.ParkedHours, row.QueuedHours)
	}
	if report.WorkTime.MedianHours != 7 {
		t.Errorf("the median work time = %g, want 7", report.WorkTime.MedianHours)
	}
	// Queue time is every observed stretch in `admitted` with no slot, whenever it falls (the
	// spec's "Rule details — Queue time, one definition"), so this issue's is three hours: the
	// two before it was ever given work and the one it went back for. Only the second is inside
	// the work time, which is why the row above reports one.
	if queued := totalOf(fixture(hour(-24), hour(24), issue).queuedSpans(issue)); queued.Hours() != 3 {
		t.Errorf("the queued spans total %v, want the three hours the issue was admitted holding no slot", queued)
	}
}

// TestTheSevenBarsOfAnIssueAddUpToItsWorkTime: "An hour that work time to merge leaves out is on
// no bar of phase time: an hour in `backlog` or `icebox`, an hour back in the queue, and any hour
// before the issue's first worker event. So the bars of an issue add up to its work time, and the
// two cards agree" (the spec's "Rule details — Parked and pre-work hours in phase time"). Both
// timelines are read: the fallback, whose first phase is the tester's own event because the
// `in_progress` of admission starts none, and whose handoff leaves an hour on no phase at all,
// and the observed one, which goes back to the queue mid-life.
func TestTheSevenBarsOfAnIssueAddUpToItsWorkTime(t *testing.T) {
	// The fallback: an `in_progress` at hour -3 that starts no phase, the tester's own event at
	// hour 2 — the first worker event, where the work time and the first phase both start —
	// testing ended by its handoff at hour 3, the reviewer's own event an hour later, and two
	// parked hours from hour 5.
	fallback := newIssue("INFRA-1").
		statusAt(hour(-3), "in_progress").
		worked("tester", hour(2)).
		handedOff("test", hour(3)).
		worked("reviewer", hour(4)).
		statusAt(hour(5), "backlog").
		statusAt(hour(7), "needs_review").
		closedAt(hour(9))
	// The observed: a design hour before the first worker event, implementing from it, two hours
	// back in the queue from hour 4 with the slot given up, and one parked hour from hour 7.
	observed := newIssue("INFRA-2").worked("implementer", hour(0)).closedAt(hour(10)).
		observeString(legion.FieldPhase, hour(-1), "admitted").
		observe(legion.FieldSlot, hour(-1), "1").
		observeString(legion.FieldPhase, hour(0), "implementing").
		observeString(legion.FieldPhase, hour(4), "admitted").
		observe(legion.FieldSlot, hour(4), "null").
		observeString(legion.FieldPhase, hour(6), "implementing").
		statusAt(hour(7), "backlog").
		statusAt(hour(8), "in_progress").
		observeString(legion.FieldPhase, hour(10), "done")
	for _, one := range []struct {
		issue     *Issue
		workHours float64
		parked    float64
		queued    float64
		bars      map[string]float64
	}{
		// Seven hours of life less the two parked ones. The three implementing hours before the
		// first worker event are on no bar, the `test` handoff starts the reviewer's phase an
		// hour before the reviewer's own first event, and the two parked hours of reviewing are
		// gone.
		{fallback, 5, 2, 0, map[string]float64{
			"architect": 0, "planner": 0, "implementer": 0, "tester": 1, "reviewer": 4, "merger": 0, barNoRole: 0,
		}},
		// Ten hours of life less the hour parked and the two back in the queue. The design hour
		// before the first worker event is on no bar, and so are the queued hours.
		{observed, 7, 1, 2, map[string]float64{
			"architect": 0, "planner": 0, "implementer": 7, "tester": 0, "reviewer": 0, "merger": 0, barNoRole: 0,
		}},
	} {
		t.Run(one.issue.Key, func(t *testing.T) {
			report := Compute(fixture(hour(-24), hour(24), one.issue), ScopeAll, hour(24))
			row := report.WorkTime.Issues[0]
			if row.Hours != one.workHours || row.ParkedHours != one.parked || row.QueuedHours != one.queued {
				t.Fatalf("the work time = %g hours (parked %g, queued %g), want %g (%g, %g)",
					row.Hours, row.ParkedHours, row.QueuedHours, one.workHours, one.parked, one.queued)
			}
			hoursPerBar := map[string]float64{}
			var whole float64
			for _, bar := range report.PhaseTime.Bars {
				hoursPerBar[bar.Role] = bar.Hours
				whole += bar.Hours
			}
			if !reflect.DeepEqual(hoursPerBar, one.bars) {
				t.Fatalf("the bars are %v, want %v", hoursPerBar, one.bars)
			}
			if whole != row.Hours {
				t.Fatalf("the seven bars hold %g hours against a work time of %g: the two cards of one page disagree",
					whole, row.Hours)
			}
		})
	}
}

// TestAnIssueThatClosedWithNoLegionWorkIsNotCounted: the population is the issues that closed in
// the window with Legion phase work — an event of a session titled `Legion <role> · <KEY>` for
// their own key, or a handoff — and nothing else that closed.
func TestAnIssueThatClosedWithNoLegionWorkIsNotCounted(t *testing.T) {
	worked := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(2))
	handedOff := newIssue("INFRA-2").handedOff("plan", hour(1)).closedAt(hour(3))
	bare := newIssue("INFRA-3").closedAt(hour(4))
	early := newIssue("INFRA-4").worked("implementer", hour(-100)).closedAt(hour(-50))
	report := Compute(fixture(hour(-24), hour(24), worked, handedOff, bare, early), ScopeAll, hour(24))
	var counted []string
	for _, row := range report.WorkTime.Issues {
		counted = append(counted, row.Key)
	}
	if !reflect.DeepEqual(counted, []string{"INFRA-1", "INFRA-2"}) {
		t.Fatalf("counted %v, want the worked issue and the one with a handoff", counted)
	}
}

// TestTheScopeTogglePicksRootsOrChildren: a tree's root and its child, each counted alone under
// its own scope and both under all. The root is read from the parent chain, since no daemon has
// sent a `tree` yet.
func TestTheScopeTogglePicksRootsOrChildren(t *testing.T) {
	root := newIssue("INFRA-1").worked("architect", hour(0)).closedAt(hour(9))
	child := newIssue("INFRA-2").under("INFRA-1").worked("implementer", hour(1)).closedAt(hour(8))
	// An issue under a parent no Legion session ever worked is a root of its own: a Legion tree
	// does not climb into the tracking issues above it.
	tracking := newIssue("INFRA-9")
	free := newIssue("INFRA-3").under("INFRA-9").worked("implementer", hour(2)).closedAt(hour(7))
	population := fixture(hour(-24), hour(24), root, child, tracking, free)
	for _, one := range []struct {
		scope Scope
		want  []string
	}{
		{ScopeAll, []string{"INFRA-1", "INFRA-2", "INFRA-3"}},
		{ScopeRoots, []string{"INFRA-1", "INFRA-3"}},
		{ScopeChildren, []string{"INFRA-2"}},
	} {
		t.Run(string(one.scope), func(t *testing.T) {
			var counted []string
			for _, row := range Compute(population, one.scope, hour(24)).WorkTime.Issues {
				counted = append(counted, row.Key)
			}
			if !reflect.DeepEqual(counted, one.want) {
				t.Fatalf("scope %s counted %v, want %v", one.scope, counted, one.want)
			}
		})
	}
}

// TestEachPhaseStartsAtItsRolesFirstEventAndEndsWhereTheNextStarts is the fallback timeline, for
// an issue the sampler never observed: each role's phase starts at the first event of its
// session or at the handoff of the phase before it, whichever comes first, accepting starts at
// the review handoff, and the merger's phase ends at the close.
func TestEachPhaseStartsAtItsRolesFirstEventAndEndsWhereTheNextStarts(t *testing.T) {
	root := newIssue("INFRA-1").
		worked("architect", hour(0)).
		worked("planner", hour(1)).
		handedOff("plan", hour(2)).
		worked("implementer", hour(3)).
		worked("tester", hour(5)).
		worked("reviewer", hour(7)).
		handedOff("review", hour(8)).
		worked("merger", hour(9)).
		closedAt(hour(10))
	population := fixture(hour(-24), hour(24), root)
	want := []string{
		"design 0-1", "planning 1-2", "implementing 2-5", "testing 5-7", "reviewing 7-8",
		"accepting 8-9", "retro 9-10",
	}
	if got := timelineOf(population, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("the fallback timeline is %v, want %v", got, want)
	}
	report := Compute(population, ScopeAll, hour(24))
	hoursPerBar := map[string]float64{}
	for _, bar := range report.PhaseTime.Bars {
		hoursPerBar[bar.Role] = bar.Hours
	}
	want2 := map[string]float64{
		"architect": 2, "planner": 1, "implementer": 3, "tester": 2, "reviewer": 1, "merger": 1, barNoRole: 0,
	}
	if !reflect.DeepEqual(hoursPerBar, want2) {
		t.Fatalf("the bars are %v, want %v: the architect's design and accepting, and the implementer's phase from the plan handoff rather than from its own first event, which leaves the bar of no role empty", hoursPerBar, want2)
	}
	// Nothing was observed, so segments 2 and 3 can hold through nothing and every hour is idle.
	if report.PhaseTime.IdleShare != 1 {
		t.Errorf("the idle share = %g, want 1 before any observation exists", report.PhaseTime.IdleShare)
	}
}

// TestAPhaseStartsAtTheHandoffOrStatusThatMarksItWhenItsRoleWroteNoEvent: a role whose session
// left no event on the issue still starts its phase, at the handoff the phase before it ended
// with or at the status the workflow set.
func TestAPhaseStartsAtTheHandoffOrStatusThatMarksItWhenItsRoleWroteNoEvent(t *testing.T) {
	root := newIssue("INFRA-1").
		worked("planner", hour(0)).
		handedOff("plan", hour(1)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(4), "testing").
		closedAt(hour(6))
	want := []string{"planning 0-1", "implementing 1-4", "testing 4-6"}
	if got := timelineOf(fixture(hour(-24), hour(24), root), root); !reflect.DeepEqual(got, want) {
		t.Fatalf("the fallback timeline is %v, want %v", got, want)
	}
}

// TestAnIssuesOwnArchitectEventsGiveItADesignPhase: "An issue has a design phase when a session
// titled `Legion architect · <its own key>` wrote an event on it ... whether or not it has a
// parent. The design phase runs from the issue's first slot, or else from that architect's first
// event, to the first planner event. An issue with no such events has no design phase, and its
// architect bar is its accepting phase only" (the spec's "Which issues have a design phase").
func TestAnIssuesOwnArchitectEventsGiveItADesignPhase(t *testing.T) {
	root := newIssue("INFRA-1").
		worked("architect", hour(0)).
		worked("planner", hour(2)).
		handedOff("review", hour(7)).
		closedAt(hour(9))
	// A child with an architect of its own: a sub-architect titles its session by the child's
	// own key, so the child has a design phase as its root does.
	child := newIssue("INFRA-2").under("INFRA-1").
		worked("architect", hour(3)).
		worked("planner", hour(4)).
		handedOff("review", hour(6)).
		closedAt(hour(8))
	// A child no architect session of its own ever wrote on: its architect bar is its accepting
	// phase alone, from its review handoff to its close.
	plain := newIssue("INFRA-3").under("INFRA-1").
		worked("implementer", hour(1)).
		handedOff("review", hour(3)).
		closedAt(hour(5))
	// The sampler recorded a slot an hour before this one's architect spoke, which is where its
	// design phase begins.
	admitted := newIssue("INFRA-4").
		observe(legion.FieldSlot, hour(1), "2").
		worked("architect", hour(2)).
		worked("planner", hour(4)).
		closedAt(hour(6))
	population := fixture(hour(-24), hour(24), root, child, plain, admitted)
	for _, one := range []struct {
		issue *Issue
		want  []string
	}{
		{root, []string{"design 0-2", "planning 2-7", "accepting 7-9"}},
		{child, []string{"design 3-4", "planning 4-6", "accepting 6-8"}},
		{plain, []string{"implementing 1-3", "accepting 3-5"}},
		{admitted, []string{"design 1-4", "planning 4-6"}},
	} {
		t.Run(one.issue.Key, func(t *testing.T) {
			if got := timelineOf(population, one.issue); !reflect.DeepEqual(got, one.want) {
				t.Fatalf("the timeline of %s is %v, want %v", one.issue.Key, got, one.want)
			}
		})
	}
	// The child with its own architect holds a design hour and two accepting ones; the one
	// without holds its two accepting hours and no design hour at all.
	if bar := barOf(t, Compute(population, ScopeChildren, hour(24)), "architect"); bar.Hours != 5 {
		t.Fatalf("the two children's architect bar = %g hours, want 5: one design hour and two accepting of INFRA-2, and the two accepting of INFRA-3", bar.Hours)
	}
	if bar := barOf(t, Compute(fixture(hour(-24), hour(24), root, plain), ScopeChildren, hour(24)), "architect"); bar.Hours != 2 {
		t.Fatalf("the architect bar of a child with no architect events = %g hours, want its two accepting hours", bar.Hours)
	}
}

// TestAHandoffStartsTheNextPhaseAndLeavesNoHourBetweenThem: "Each phase ends, and the next one
// starts, at the earlier of the phase's own handoff and the next role's first event ... So a
// handoff starts the next phase, and no hour between phases is left over" (the spec's "Phase
// boundaries before observations"). The implementer here hands off two hours before the tester
// speaks and the tester three before the reviewer does, and neither gap is left on no bar.
func TestAHandoffStartsTheNextPhaseAndLeavesNoHourBetweenThem(t *testing.T) {
	root := newIssue("INFRA-1").
		worked("implementer", hour(0)).
		handedOff("implement", hour(2)).
		worked("tester", hour(4)).
		handedOff("test", hour(5)).
		worked("reviewer", hour(8)).
		closedAt(hour(10))
	population := fixture(hour(-24), hour(24), root)
	want := []string{"implementing 0-2", "testing 2-5", "reviewing 5-10"}
	if got := timelineOf(population, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("the fallback timeline is %v, want %v: each handoff starts the next phase", got, want)
	}
	report := Compute(population, ScopeAll, hour(24))
	hoursPerBar := map[string]float64{}
	var whole float64
	for _, bar := range report.PhaseTime.Bars {
		hoursPerBar[bar.Role] = bar.Hours
		whole += bar.Hours
	}
	want2 := map[string]float64{
		"architect": 0, "planner": 0, "implementer": 2, "tester": 3, "reviewer": 5, "merger": 0, barNoRole: 0,
	}
	if !reflect.DeepEqual(hoursPerBar, want2) {
		t.Fatalf("the bars are %v, want %v: the two hours after the implement handoff are the tester's and the three after the test handoff are the reviewer's", hoursPerBar, want2)
	}
	if row := report.WorkTime.Issues[0]; whole != row.Hours {
		t.Fatalf("the seven bars hold %g hours against a work time of %g: a handoff left an hour between two phases", whole, row.Hours)
	}
}

// TestTheNoRoleBarHoldsAwaitingPersonAndHeldAlone: "A seventh bar, 'no role', holds the phases no
// role works: `awaiting_person` and `held`", and the spec's "Phase boundaries before
// observations" holds it to those two. Every other hour of the work time is on a role's bar.
func TestTheNoRoleBarHoldsAwaitingPersonAndHeldAlone(t *testing.T) {
	issue := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(6)).
		observeString(legion.FieldPhase, hour(0), "implementing").
		observeString(legion.FieldPhase, hour(1), "awaiting_person").
		observeString(legion.FieldPhase, hour(2), "held").
		observeString(legion.FieldPhase, hour(3), "implementing").
		observeString(legion.FieldPhase, hour(6), "done")
	report := Compute(fixture(hour(-24), hour(24), issue), ScopeAll, hour(24))
	bar := barOf(t, report, barNoRole)
	if bar.Hours != 2 {
		t.Fatalf("the no-role bar = %g hours, want the awaiting_person hour and the held one", bar.Hours)
	}
	// The hour the daemon recorded the issue in `awaiting_person` is segment 1 of that bar; the
	// held hour is no wait on a person, so it is idle.
	if bar.Segments != (Segments{WaitingOnPerson: 1, Idle: 1}) {
		t.Fatalf("the no-role bar is %+v, want the awaiting_person hour waiting on a person and the held one idle", bar.Segments)
	}
	if hours := barOf(t, report, "implementer").Hours; hours != 4 {
		t.Fatalf("the implementer bar = %g hours, want the four hours of implementing", hours)
	}
}

// TestAfterASendBackTheDispatchStatusSetsThePhase: "After a send-back, the Dispatch status sets
// the phase: `in_progress` after `testing` or `needs_review` is implementing, `testing` is
// testing, and `needs_review` is reviewing until the `review` handoff of that round, then
// accepting until the status changes" (the spec's "Phase boundaries before observations"). The
// round below is the second one: no role writes a first event twice, so the statuses are the
// only record of it.
func TestAfterASendBackTheDispatchStatusSetsThePhase(t *testing.T) {
	issue := newIssue("INFRA-1").
		statusAt(hour(-1), "in_progress").
		worked("implementer", hour(0)).
		worked("tester", hour(1)).
		statusAt(hour(1), "testing").
		worked("reviewer", hour(2)).
		statusAt(hour(2), "needs_review").
		// The reviewer asked for changes: implementing, testing and reviewing again, each named
		// by the status alone, and the round's review handoff starts the accepting that follows.
		statusAt(hour(3), "in_progress").
		statusAt(hour(4), "testing").
		statusAt(hour(5), "needs_review").
		handedOff("review", hour(6)).
		statusAt(hour(8), "done").
		closedAt(hour(8))
	population := fixture(hour(-24), hour(24), issue)
	// The admission's `in_progress` at hour -1 starts no phase ("A status write such as the
	// `in_progress` of admission starts no phase", the spec's "What starts planning"), and this
	// issue has no planner event, so its timeline opens at its implementer's first event.
	want := []string{
		"implementing 0-1", "testing 1-2", "reviewing 2-3", "implementing 3-4",
		"testing 4-5", "reviewing 5-6", "accepting 6-8",
	}
	if got := timelineOf(population, issue); !reflect.DeepEqual(got, want) {
		t.Fatalf("the fallback timeline is %v, want %v", got, want)
	}
	report := Compute(population, ScopeAll, hour(24))
	hoursPerBar := map[string]float64{}
	var whole float64
	for _, bar := range report.PhaseTime.Bars {
		hoursPerBar[bar.Role] = bar.Hours
		whole += bar.Hours
	}
	// The hour before the first worker event is on no bar, whatever phase named it.
	want2 := map[string]float64{
		"architect": 2, "planner": 0, "implementer": 2, "tester": 2, "reviewer": 2, "merger": 0, barNoRole: 0,
	}
	if !reflect.DeepEqual(hoursPerBar, want2) {
		t.Fatalf("the bars are %v, want %v", hoursPerBar, want2)
	}
	if row := report.WorkTime.Issues[0]; whole != row.Hours {
		t.Fatalf("the seven bars hold %g hours against a work time of %g", whole, row.Hours)
	}
	row := report.Rework.Issues[0]
	if row.ReviewRounds != 2 || !row.SentBack {
		t.Fatalf("the issue has %d review rounds and sent_back=%t, want two rounds and the send-back its statuses record", row.ReviewRounds, row.SentBack)
	}
	if row.ImplementingHoursAfterFirst != 1 || row.TestingHoursAfterFirst != 1 {
		t.Fatalf("after the first review: %g implementing and %g testing hours, want the hour of each the second round holds",
			row.ImplementingHoursAfterFirst, row.TestingHoursAfterFirst)
	}
}

// TestPlanningStartsAtTheFirstPlannerEventAndNotTheAdmissionWrite: "For an issue with no design
// phase, [planning] starts at the first planner event, as every other role's phase starts at its
// role's first event. A status write such as the `in_progress` of admission starts no phase" (the
// spec's "What starts planning"). The issue below has no architect event, so it has no design
// phase, and the workflow's admission write two hours before its planner spoke opens nothing: its
// first phase is the planning its planner's own first event starts.
func TestPlanningStartsAtTheFirstPlannerEventAndNotTheAdmissionWrite(t *testing.T) {
	issue := newIssue("INFRA-1").
		statusAt(hour(-2), "in_progress").
		worked("planner", hour(0)).
		worked("implementer", hour(2)).
		statusAt(hour(4), "done").
		closedAt(hour(4))
	population := fixture(hour(-24), hour(24), issue)
	want := []string{"planning 0-2", "implementing 2-4"}
	if got := timelineOf(population, issue); !reflect.DeepEqual(got, want) {
		t.Fatalf("the fallback timeline is %v, want %v", got, want)
	}
	report := Compute(population, ScopeAll, hour(24))
	hoursPerBar := map[string]float64{}
	var whole float64
	for _, bar := range report.PhaseTime.Bars {
		hoursPerBar[bar.Role] = bar.Hours
		whole += bar.Hours
	}
	want2 := map[string]float64{
		"architect": 0, "planner": 2, "implementer": 2, "tester": 0, "reviewer": 0, "merger": 0, barNoRole: 0,
	}
	if !reflect.DeepEqual(hoursPerBar, want2) {
		t.Fatalf("the bars are %v, want %v", hoursPerBar, want2)
	}
	if row := report.WorkTime.Issues[0]; whole != row.Hours {
		t.Fatalf("the seven bars hold %g hours against a work time of %g", whole, row.Hours)
	}
}

// TestWorkHoursBeforeTheFirstPhaseMarkAreOnThatPhasesBar: "When an issue's first worker event
// comes before the start of its first phase, the work hours in between go on the bar of that
// first phase. So every work-time hour stays on a bar" (the spec's "Work hours before any phase
// mark"). The issue below opens its work time with a handoff record whose word names no phase —
// retro, the merger's READY and the architect's sign-off write none — an hour before its planner
// speaks, and the admission's `in_progress` before that opens nothing, so its first phase starts
// after its work time does.
func TestWorkHoursBeforeTheFirstPhaseMarkAreOnThatPhasesBar(t *testing.T) {
	issue := newIssue("INFRA-1").
		statusAt(hour(-2), "in_progress").
		handedOff("retro", hour(0)).
		worked("planner", hour(1)).
		worked("implementer", hour(3)).
		closedAt(hour(5))
	population := fixture(hour(-24), hour(24), issue)
	// The planning the planner's event starts at hour 1 reaches back to the handoff that opened
	// the work time, so the hour between them is on the planner's bar and not on none.
	want := []string{"planning 0-3", "implementing 3-5"}
	if got := timelineOf(population, issue); !reflect.DeepEqual(got, want) {
		t.Fatalf("the fallback timeline is %v, want %v", got, want)
	}
	report := Compute(population, ScopeAll, hour(24))
	hoursPerBar := map[string]float64{}
	var whole float64
	for _, bar := range report.PhaseTime.Bars {
		hoursPerBar[bar.Role] = bar.Hours
		whole += bar.Hours
	}
	want2 := map[string]float64{
		"architect": 0, "planner": 3, "implementer": 2, "tester": 0, "reviewer": 0, "merger": 0, barNoRole: 0,
	}
	if !reflect.DeepEqual(hoursPerBar, want2) {
		t.Fatalf("the bars are %v, want %v", hoursPerBar, want2)
	}
	row := report.WorkTime.Issues[0]
	if row.Hours != 5 {
		t.Fatalf("the work time = %g hours, want the five from the handoff that opened it to the close", row.Hours)
	}
	if whole != row.Hours {
		t.Fatalf("the seven bars hold %g hours against a work time of %g: an hour of the work time is on no bar",
			whole, row.Hours)
	}
}

// TestWorkHoursBeforeTheFirstObservedMarkAreOnThatPhasesBar: the same rule on the other timeline.
// The sampler reads a daemon once a minute and records a phase only when it changes, so an issue
// whose first worker event comes before the sampler ever saw it has work hours ahead of every
// observation. They are on the bar of the phase it is first seen in, as the fallback's are, or
// the seven bars would not add up to the work time for any issue the sampler met late.
func TestWorkHoursBeforeTheFirstObservedMarkAreOnThatPhasesBar(t *testing.T) {
	issue := newIssue("INFRA-1").
		worked("implementer", hour(0)).
		observeString(legion.FieldPhase, hour(2), "implementing").
		observeString(legion.FieldPhase, hour(4), "testing").
		closedAt(hour(6))
	population := fixture(hour(-24), hour(24), issue)
	want := []string{"implementing 0-4", "testing 4-6"}
	if got := timelineOf(population, issue); !reflect.DeepEqual(got, want) {
		t.Fatalf("the observed timeline is %v, want %v", got, want)
	}
	report := Compute(population, ScopeAll, hour(24))
	hoursPerBar := map[string]float64{}
	var whole float64
	for _, bar := range report.PhaseTime.Bars {
		hoursPerBar[bar.Role] = bar.Hours
		whole += bar.Hours
	}
	want2 := map[string]float64{
		"architect": 0, "planner": 0, "implementer": 4, "tester": 2, "reviewer": 0, "merger": 0, barNoRole: 0,
	}
	if !reflect.DeepEqual(hoursPerBar, want2) {
		t.Fatalf("the bars are %v, want %v", hoursPerBar, want2)
	}
	row := report.WorkTime.Issues[0]
	if row.Hours != 6 {
		t.Fatalf("the work time = %g hours, want the six from the first worker event to the close", row.Hours)
	}
	if whole != row.Hours {
		t.Fatalf("the seven bars hold %g hours against a work time of %g: an hour of the work time is on no bar",
			whole, row.Hours)
	}
}

// TestAnAdmittedIssueHoldingASlotIsDesignAndOneHoldingNoneIsQueue: the daemon has one `admitted`
// phase; the view's Design column is the part of it an issue holds a slot through, and the rest is
// the waiting line, whose hours are on no bar.
func TestAnAdmittedIssueHoldingASlotIsDesignAndOneHoldingNoneIsQueue(t *testing.T) {
	root := newIssue("INFRA-1").worked("architect", hour(0)).closedAt(hour(4)).
		observeString(legion.FieldPhase, hour(0), "admitted").
		observe(legion.FieldSlot, hour(1), "2").
		observeString(legion.FieldPhase, hour(3), "planning").
		observeString(legion.FieldPhase, hour(4), "done")
	population := fixture(hour(-24), hour(24), root)
	want := []string{"queued 0-1", "design 1-3", "planning 3-4"}
	if got := timelineOf(population, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("the observed timeline is %v, want %v", got, want)
	}
	report := Compute(population, ScopeAll, hour(24))
	if bar := barOf(t, report, "architect"); bar.Hours != 2 {
		t.Fatalf("the architect bar = %g hours, want the two design hours and no queue hour", bar.Hours)
	}
	var whole float64
	for _, bar := range report.PhaseTime.Bars {
		whole += bar.Hours
	}
	if whole != 3 {
		t.Fatalf("the bars hold %g hours of a four-hour life, want 3: the queued hour is on no bar", whole)
	}
}

// TestReviewRoundsAndSendBacksComeFromThePhaseTimeline: metric 3 over an observed issue that was
// reviewed twice and sent back once, with the implementing and testing hours after its first
// review.
func TestReviewRoundsAndSendBacksComeFromThePhaseTimeline(t *testing.T) {
	issue := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(6)).
		observeString(legion.FieldPhase, hour(0), "implementing").
		observeString(legion.FieldPhase, hour(1), "testing").
		observeString(legion.FieldPhase, hour(2), "reviewing").
		observeString(legion.FieldPhase, hour(3), "implementing").
		observeString(legion.FieldPhase, hour(4), "testing").
		observeString(legion.FieldPhase, hour(5), "reviewing").
		observeString(legion.FieldPhase, hour(6), "done")
	report := Compute(fixture(hour(-24), hour(24), issue), ScopeAll, hour(24))
	row := report.Rework.Issues[0]
	if row.ReviewRounds != 2 || !row.SentBack {
		t.Fatalf("the issue has %d review rounds and sent_back=%t, want two rounds and a send-back", row.ReviewRounds, row.SentBack)
	}
	if row.ImplementingHoursAfterFirst != 1 || row.TestingHoursAfterFirst != 1 {
		t.Fatalf("after the first review: %g implementing and %g testing hours, want one of each",
			row.ImplementingHoursAfterFirst, row.TestingHoursAfterFirst)
	}
	if report.Rework.MedianReviewRounds != 2 || report.Rework.SentBackShare != 1 {
		t.Fatalf("median rounds %g and sent-back share %g, want 2 and 1", report.Rework.MedianReviewRounds, report.Rework.SentBackShare)
	}
}

// TestBeforeObservationsReviewRoundsAreEntriesIntoNeedsReview: an issue the sampler never saw
// counts its review rounds from the status the workflow set, and its send-back from the same
// statuses: the move out of `needs_review` into `in_progress` at hour 3 is the reviewer asking
// for changes, which no timeline drawn from first events alone can hold.
func TestBeforeObservationsReviewRoundsAreEntriesIntoNeedsReview(t *testing.T) {
	issue := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(8)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(2), "needs_review").
		statusAt(hour(3), "in_progress").
		statusAt(hour(5), "needs_review").
		statusAt(hour(8), "done")
	report := Compute(fixture(hour(-24), hour(24), issue), ScopeAll, hour(24))
	row := report.Rework.Issues[0]
	if row.ReviewRounds != 2 || !row.SentBack {
		t.Fatalf("the issue has %d review rounds and sent_back=%t, want two rounds and the one send-back its statuses record", row.ReviewRounds, row.SentBack)
	}
}

// TestEachClassOfFiledIssue is metric 4: a child filed before the filer's first implementer event,
// one filed after it, an issue filed outside the filer's tree, and one the controller filed, each
// counted in its own week against the issues that finished.
func TestEachClassOfFiledIssue(t *testing.T) {
	root := newIssue("INFRA-1").
		worked("architect", hour(0)).
		worked("implementer", hour(5)).
		closedAt(hour(20))
	atDesign := newIssue("INFRA-2").under("INFRA-1")
	later := newIssue("INFRA-3").under("INFRA-1")
	outside := newIssue("CORE-9")
	filed := newIssue("INFRA-4")
	population := fixture(hour(-24), hour(48), root, atDesign, later, outside, filed)
	population.Filed = []Filed{
		{Key: "INFRA-2", At: hour(1), Filer: legion.SessionTitle{Role: "architect", Issue: "INFRA-1"}, Parent: "INFRA-1", Ancestors: []string{"INFRA-1"}},
		{Key: "INFRA-3", At: hour(6), Filer: legion.SessionTitle{Role: "implementer", Issue: "INFRA-1"}, Parent: "INFRA-1", Ancestors: []string{"INFRA-1"}},
		{Key: "CORE-9", At: hour(7), Filer: legion.SessionTitle{Role: "implementer", Issue: "INFRA-1"}},
		{Key: "INFRA-4", At: hour(8), Filer: legion.SessionTitle{Role: "controller", Project: "INFRA", Controller: true}},
	}
	report := Compute(population, ScopeAll, hour(48))
	classOf := map[string]string{}
	phaseOf := map[string]string{}
	for _, row := range report.Splitting.Filed {
		classOf[row.Key], phaseOf[row.Key] = row.Class, row.FilerPhase
	}
	want := map[string]string{
		"INFRA-2": ClassInTreeAtDesign, "INFRA-3": ClassInTreeLater,
		"CORE-9": ClassOutOfTree, "INFRA-4": ClassByTheController,
	}
	if !reflect.DeepEqual(classOf, want) {
		t.Fatalf("the filings are classed %v, want %v", classOf, want)
	}
	if phaseOf["INFRA-3"] != legion.PhaseImplementing {
		t.Errorf("the later filing names the filer's phase %q, want implementing", phaseOf["INFRA-3"])
	}
	if len(report.Splitting.Weeks) != 1 {
		t.Fatalf("the filings fall in %d weeks, want one", len(report.Splitting.Weeks))
	}
	week := report.Splitting.Weeks[0]
	if week.Finished != 1 || week.Filed[ClassInTreeAtDesign] != 1 || week.Filed[ClassByTheController] != 1 {
		t.Fatalf("the week is %+v, want one finished issue and one filing of each class", week)
	}
	if report.Splitting.FinishedRoots != 1 || report.Splitting.ChildrenPerRoot != 2 {
		t.Fatalf("finished roots %d with %g children each, want one root with its two children",
			report.Splitting.FinishedRoots, report.Splitting.ChildrenPerRoot)
	}
	if report.Splitting.MedianHoursRootsWithChildren != 20 || report.Splitting.MedianHoursRootsNoChildren != 0 {
		t.Fatalf("the medians are %g with children and %g without, want 20 and 0",
			report.Splitting.MedianHoursRootsWithChildren, report.Splitting.MedianHoursRootsNoChildren)
	}
}

// TestAMedianInterpolatesBetweenTheClosestRanks: "Medians and p90s use linear interpolation
// between the closest ranks".
func TestAMedianInterpolatesBetweenTheClosestRanks(t *testing.T) {
	for _, one := range []struct {
		name   string
		values []float64
		median float64
		p90    float64
	}{
		{"none", nil, 0, 0},
		{"one", []float64{4}, 4, 4},
		{"two", []float64{1, 4}, 2.5, 3.7},
		{"four", []float64{1, 2, 3, 4}, 2.5, 3.7},
	} {
		t.Run(one.name, func(t *testing.T) {
			if got := quantile(one.values, 0.5); got != one.median {
				t.Errorf("median of %v = %g, want %g", one.values, got, one.median)
			}
			if got := quantile(one.values, 0.9); got != one.p90 {
				t.Errorf("p90 of %v = %g, want %g", one.values, got, one.p90)
			}
		})
	}
}
