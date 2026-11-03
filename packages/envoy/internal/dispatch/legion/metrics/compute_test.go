package metrics

import (
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
)

// TestNoQueueOrParkedHourEntersTheWorkTime: the work time runs from the issue's first worker event
// to its close, less the hours it sat in backlog or icebox and the hours its observed phase went
// back to `admitted` after it had left it.
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
	// An issue the queue held before it ever left `admitted` is parked time, not queued time: the
	// first stretch of admitted is the wait for a slot, which no metric counts twice.
	if queued := totalOf(fixture(hour(-24), hour(24), issue).queuedSpans(issue)); queued.Hours() != 1 {
		t.Errorf("the queued spans total %v, want the one hour after it first left admitted", queued)
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
// session, a handoff ends its own phase where it comes before the next phase's start, accepting
// starts at the review handoff, and the merger's phase ends at the close.
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
		"design 0-1", "planning 1-2", "implementing 3-5", "testing 5-7", "reviewing 7-8",
		"accepting 8-9", "retro 9-10",
	}
	if got := timelineOf(population.Phases(root)); !reflect.DeepEqual(got, want) {
		t.Fatalf("the fallback timeline is %v, want %v", got, want)
	}
	report := Compute(population, ScopeAll, hour(24))
	hoursPerBar := map[string]float64{}
	for _, bar := range report.PhaseTime.Bars {
		hoursPerBar[bar.Role] = bar.Hours
	}
	want2 := map[string]float64{
		"architect": 2, "planner": 1, "implementer": 2, "tester": 2, "reviewer": 1, "merger": 1, barNoRole: 0,
	}
	if !reflect.DeepEqual(hoursPerBar, want2) {
		t.Fatalf("the bars are %v, want %v: the architect's design and accepting, and the hour between a handoff and the next start on no bar", hoursPerBar, want2)
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
	if got := timelineOf(fixture(hour(-24), hour(24), root).Phases(root)); !reflect.DeepEqual(got, want) {
		t.Fatalf("the fallback timeline is %v, want %v", got, want)
	}
}

// TestAChildHasNoDesignPhaseAndItsArchitectBarIsItsAccepting: a child's architect events belong to
// its root's session title, so a child's architect bar is the accepting phase its review handoff
// starts, and it has no design phase at all.
func TestAChildHasNoDesignPhaseAndItsArchitectBarIsItsAccepting(t *testing.T) {
	root := newIssue("INFRA-1").worked("architect", hour(0))
	child := newIssue("INFRA-2").under("INFRA-1").
		worked("architect", hour(0)).
		worked("implementer", hour(1)).
		handedOff("review", hour(3)).
		closedAt(hour(5))
	population := fixture(hour(-24), hour(24), root, child)
	want := []string{"implementing 1-3", "accepting 3-5"}
	if got := timelineOf(population.Phases(child)); !reflect.DeepEqual(got, want) {
		t.Fatalf("the child's timeline is %v, want %v", got, want)
	}
	if bar := barOf(t, Compute(population, ScopeChildren, hour(24)), "architect"); bar.Hours != 2 {
		t.Fatalf("the child's architect bar = %g hours, want its two accepting hours", bar.Hours)
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
	if got := timelineOf(population.Phases(root)); !reflect.DeepEqual(got, want) {
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
// counts its review rounds from the status the workflow set, and nothing moved it back to
// implementing in a timeline drawn from first events alone.
func TestBeforeObservationsReviewRoundsAreEntriesIntoNeedsReview(t *testing.T) {
	issue := newIssue("INFRA-1").worked("implementer", hour(0)).closedAt(hour(8)).
		statusAt(hour(0), "in_progress").
		statusAt(hour(2), "needs_review").
		statusAt(hour(3), "in_progress").
		statusAt(hour(5), "needs_review").
		statusAt(hour(8), "done")
	report := Compute(fixture(hour(-24), hour(24), issue), ScopeAll, hour(24))
	row := report.Rework.Issues[0]
	if row.ReviewRounds != 2 || row.SentBack {
		t.Fatalf("the issue has %d review rounds and sent_back=%t, want two rounds from the statuses and no send-back", row.ReviewRounds, row.SentBack)
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
		{Key: "INFRA-2", At: hour(1), Filer: SessionTitle{Role: "architect", Issue: "INFRA-1"}, Parent: "INFRA-1", Ancestors: []string{"INFRA-1"}},
		{Key: "INFRA-3", At: hour(6), Filer: SessionTitle{Role: "implementer", Issue: "INFRA-1"}, Parent: "INFRA-1", Ancestors: []string{"INFRA-1"}},
		{Key: "CORE-9", At: hour(7), Filer: SessionTitle{Role: "implementer", Issue: "INFRA-1"}},
		{Key: "INFRA-4", At: hour(8), Filer: SessionTitle{Role: "controller", Project: "INFRA", Controller: true}},
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
	if phaseOf["INFRA-3"] != phaseImplementing {
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

// TestASessionTitleNamesARoleAndItsIssue is the title parsing every rule reads a Legion session
// through (packages/pi-legion/src/session-title.ts).
func TestASessionTitleNamesARoleAndItsIssue(t *testing.T) {
	for _, one := range []struct {
		title string
		want  SessionTitle
		ok    bool
	}{
		{"Legion implementer · INFRA-1077", SessionTitle{Role: "implementer", Issue: "INFRA-1077"}, true},
		{"Legion architect · INFRA-979", SessionTitle{Role: "architect", Issue: "INFRA-979"}, true},
		{"Legion controller · INFRA", SessionTitle{Role: "controller", Project: "INFRA", Controller: true}, true},
		{"Legion gardener · INFRA-1", SessionTitle{}, false},
		{"Legion architect", SessionTitle{}, false},
		{"A person's session", SessionTitle{}, false},
		{"", SessionTitle{}, false},
	} {
		t.Run(one.title, func(t *testing.T) {
			got, ok := ParseSessionTitle(one.title)
			if ok != one.ok || got != one.want {
				t.Fatalf("ParseSessionTitle(%q) = %+v, %t; want %+v, %t", one.title, got, ok, one.want, one.ok)
			}
			if ok && !got.OfProject("INFRA") {
				t.Errorf("%q is not read as INFRA's", one.title)
			}
		})
	}
}
