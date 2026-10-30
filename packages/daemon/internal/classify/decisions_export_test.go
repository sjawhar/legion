package classify

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

func TestAdvancePullRequestHeadCountsRedFixAndConsumesHandoffClassification(t *testing.T) {
	prior := record.PullRequest{
		HeadSHA: "old", CheckedHead: "old", Failing: []string{"ci"}, Required: []string{"ci"}, FixAttempts: 2,
		Pushes: []record.ClassifiedPush{{SHA: "next", Before: "old", HandoffOnly: true}},
	}
	got := AdvancePullRequestHead(prior, Arrival{}, "next")
	if got.FixAttempts != 2 || got.HeadCounted != "" {
		t.Fatalf("handoff-only head = %#v, want unchanged count", got)
	}

	got = AdvancePullRequestHead(record.PullRequest{HeadSHA: "old", CheckedHead: "old", Failing: []string{"ci"}, Required: []string{"ci"}, FixAttempts: 2}, Arrival{}, "next")
	if got.FixAttempts != 3 || got.HeadCounted != "next" || HeadVerdict(got) != "" {
		t.Fatalf("real fix head = %#v, want counted next head", got)
	}
}

// A head counted before its push arrived is taken back when the push proves the move handoff-only:
// the count reported at it is no longer reached, so its report is cleared too.
func TestApplyPushTakesBackACountItsLatePushSpares(t *testing.T) {
	red := record.PullRequest{HeadSHA: "prev", CheckedHead: "prev", Failing: []string{"ci"}, Required: []string{"ci"},
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, FixAttempts: 2}
	counted := AdvancePullRequestHead(red, Arrival{}, "head")
	counted.BlockedAttempts = 3
	if counted.FixAttempts != 3 || counted.HeadCounted != "head" || counted.Run == nil {
		t.Fatalf("a head whose push has not arrived = %#v, want counted, its run open", counted)
	}
	got := ApplyPush(counted, record.ClassifiedPush{SHA: "head", Before: "prev", HandoffOnly: true})
	if got.FixAttempts != 2 || got.BlockedAttempts != 0 || got.HeadCounted != "" || got.Run != nil {
		t.Fatalf("handoff-only take-back = %#v, want decremented unblocked head, its run closed", got)
	}
}

// A push that reached the counted head from another head - the required-checks pass's compare from
// an approved head (daemon's readApprovalCarry) - says only what changed between those two heads,
// not in the move the count counted, which can carry code: here h3 changed code on the approved
// h1, and h4 takes that code out again, so h1 to h4 changes only handoffs while h3 to h4 changes
// code. Only a chain of pushes from the head the count moved from takes the count back, here h4's
// own push from h3.
func TestApplyPushTakesBackOnlyTheCountedMovesOwnPush(t *testing.T) {
	for _, tc := range []struct {
		name string
		push record.ClassifiedPush
	}{
		{"a handoff-only push", record.ClassifiedPush{HandoffOnly: true}},
		{"the review App's push", record.ClassifiedPush{ByReviewApp: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counted := AdvancePullRequestHead(record.PullRequest{HeadSHA: "h3", CheckedHead: "h3", Failing: []string{"ci"}, Required: []string{"ci"},
				FixAttempts: 2, HeadCounted: "h2"}, Arrival{}, "h4")

			if counted.FixAttempts != 3 || counted.HeadCounted != "h4" {
				t.Fatalf("a push onto red h3 = %#v, want h4 counted", counted)
			}
			fromApproved := tc.push
			fromApproved.SHA, fromApproved.Before = "h4", "h1"
			got := ApplyPush(counted, fromApproved)
			if got.FixAttempts != 3 || got.HeadCounted != "h4" || got.Run == nil {
				t.Fatalf("a push from the approved h1 to h4 = fixAttempts=%d headCounted=%q run open %v, want 3, \"h4\" and open: it did not make the counted move",
					got.FixAttempts, got.HeadCounted, got.Run != nil)
			}
			own := tc.push
			own.SHA, own.Before = "h4", "h3"
			got = ApplyPush(got, own)
			if got.FixAttempts != 2 || got.HeadCounted != "h2" || got.Run != nil {
				t.Fatalf("h4's own push from h3 = fixAttempts=%d headCounted=%q run open %v, want 2, \"h2\" and closed: the count taken back",
					got.FixAttempts, got.HeadCounted, got.Run != nil)
			}
		})
	}
}

// A move's count and planned mark do not depend on the order its pushes and its head arrive in:
// every order of the pushes, with the head arriving before, between or after them, leaves the same
// count, counted mark and planned mark. The head can arrive past a head the daemon never recorded
// (followHead's read of GitHub's head, a reopen), h2 to h4 here with h3 between: the move is spared
// when the pushes chain from h2 to h4, each handoff-only or the review App's, and counted otherwise
// - also when the pushes arrive first and h4's own handoff push left a head (h3) the implementer's
// fix made. The planned mark is the chain's last push that may change code: the review App's red
// tests at h3 plan a red at h4 above them, though h4's own push is a handoff. A forced handoff may
// change code whatever its paths say, so it counts, takes nothing back, and decides the mark; a
// forced push of the review App's is spared by who pushed it, as an unforced one is. Each
// case runs from an unplanned red, where the count is decided, and from a planned one, where
// nothing counts and a move of handoffs alone carries the mark.
func TestAMoveCountsAndPlansTheSameInEveryOrderOfItsPushesAndItsHead(t *testing.T) {
	planned, unplanned := true, false
	for _, tc := range []struct {
		name   string
		head   string
		pushes []record.ClassifiedPush
		// counted is whether the move h2 to head counts a fix attempt from an unplanned red; plans is
		// the planned mark it leaves, nil when it carries the mark.
		counted bool
		plans   *bool
	}{
		{"the review App's red tests h2..h3, then a handoff h3..h4, h3 never recorded", "h4", []record.ClassifiedPush{
			{SHA: "h3", Before: "h2", ByReviewApp: true}, {SHA: "h4", Before: "h3", HandoffOnly: true}}, false, &planned},
		{"two handoffs, h3 never recorded", "h4", []record.ClassifiedPush{
			{SHA: "h3", Before: "h2", HandoffOnly: true}, {SHA: "h4", Before: "h3", HandoffOnly: true}}, false, nil},
		{"the implementer's fix, then a handoff, h3 never recorded", "h4", []record.ClassifiedPush{
			{SHA: "h3", Before: "h2"}, {SHA: "h4", Before: "h3", HandoffOnly: true}}, true, &unplanned},
		{"the implementer's fix, then the review App's red tests, h3 never recorded", "h4", []record.ClassifiedPush{
			{SHA: "h3", Before: "h2"}, {SHA: "h4", Before: "h3", ByReviewApp: true}}, true, &planned},
		{"a handoff, then a forced handoff, h3 never recorded", "h4", []record.ClassifiedPush{
			{SHA: "h3", Before: "h2", HandoffOnly: true}, {SHA: "h4", Before: "h3", HandoffOnly: true, Forced: true}}, true, &unplanned},
		{"one handoff", "h3", []record.ClassifiedPush{{SHA: "h3", Before: "h2", HandoffOnly: true}}, false, nil},
		{"one forced handoff", "h3", []record.ClassifiedPush{{SHA: "h3", Before: "h2", HandoffOnly: true, Forced: true}}, true, &unplanned},
		{"one forced push of the review App's", "h3", []record.ClassifiedPush{{SHA: "h3", Before: "h2", ByReviewApp: true, Forced: true}}, false, &planned},
	} {
		for _, wasPlanned := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, from a red planned %v", tc.name, wasPlanned), func(t *testing.T) {
				red := record.PullRequest{HeadSHA: "h2", CheckedHead: "h2", Failing: []string{"ci"}, Required: []string{"ci"},
					CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, FixAttempts: 2, PlannedRed: wasPlanned}
				wantAttempts, wantCounted, wantPlanned := 2, "", wasPlanned
				if tc.counted && !wasPlanned {
					wantAttempts, wantCounted = 3, tc.head
				}
				if tc.plans != nil {
					wantPlanned = *tc.plans
				}
				for _, order := range pushOrders(tc.pushes) {
					for at := 0; at <= len(order); at++ {
						got, steps := red, []string{}
						for i := 0; i <= len(order); i++ {
							if i == at {
								got, steps = AdvancePullRequestHead(got, Arrival{}, tc.head), append(steps, "head "+tc.head)
							}
							if i < len(order) {
								got, steps = ApplyPush(got, order[i]), append(steps, "push "+order[i].Before+".."+order[i].SHA)
							}
						}
						if got.FixAttempts != wantAttempts || got.HeadCounted != wantCounted || got.PlannedRed != wantPlanned || got.Run != nil {
							t.Errorf("%v: fixAttempts=%d headCounted=%q plannedRed=%v run open %v, want %d, %q, %v and closed",
								steps, got.FixAttempts, got.HeadCounted, got.PlannedRed, got.Run != nil, wantAttempts, wantCounted, wantPlanned)
						}
					}
				}
			})
		}
	}
}

// pushOrders is every order of pushes.
func pushOrders(pushes []record.ClassifiedPush) [][]record.ClassifiedPush {
	if len(pushes) <= 1 {
		return [][]record.ClassifiedPush{append([]record.ClassifiedPush{}, pushes...)}
	}
	var orders [][]record.ClassifiedPush
	for i := range pushes {
		rest := append(append([]record.ClassifiedPush{}, pushes[:i]...), pushes[i+1:]...)
		for _, order := range pushOrders(rest) {
			orders = append(orders, append([]record.ClassifiedPush{pushes[i]}, order...))
		}
	}
	return orders
}

// A push for a later move that arrives before the move's first link waits for its chain: the
// review App's red tests h4..h5 arrive first, then h3 arrives over h2..h3, then h5 past h4, and
// the handoff h3..h4 last. h5's arrival counts until that handoff completes the chain h3..h4..h5,
// which takes the count back and plans h5's red, as the pushes arriving first would have left it.
func TestAPushForALaterMoveWaitsForItsChain(t *testing.T) {
	red := record.PullRequest{HeadSHA: "h2", CheckedHead: "h2", Failing: []string{"ci"}, Required: []string{"ci"},
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, FixAttempts: 2}
	first := record.ClassifiedPush{SHA: "h3", Before: "h2", HandoffOnly: true}
	link := record.ClassifiedPush{SHA: "h4", Before: "h3", HandoffOnly: true}
	tests := record.ClassifiedPush{SHA: "h5", Before: "h4", ByReviewApp: true}
	early := AdvancePullRequestHead(ApplyPush(ApplyPush(ApplyPush(red, first), link), tests), Arrival{}, "h3")
	early = AdvancePullRequestHead(early, Arrival{}, "h5")
	late := AdvancePullRequestHead(ApplyPush(ApplyPush(red, tests), first), Arrival{}, "h3")
	late = AdvancePullRequestHead(late, Arrival{}, "h5")
	if late.FixAttempts != 3 || late.HeadCounted != "h5" {
		t.Fatalf("h5 arrived before its chain: fixAttempts=%d headCounted=%q, want 3 and \"h5\"", late.FixAttempts, late.HeadCounted)
	}
	late = ApplyPush(late, link)
	for name, got := range map[string]record.PullRequest{"pushes first": early, "the link last": late} {
		if got.FixAttempts != 2 || got.HeadCounted != "" || !got.PlannedRed {
			t.Errorf("%s: fixAttempts=%d headCounted=%q plannedRed=%v, want 2, \"\" and true", name, got.FixAttempts, got.HeadCounted, got.PlannedRed)
		}
	}
}

// A push is spent once its move is judged: a head that arrives before its push keeps no more than
// the push still to be linked, which the next head's arrival spends, and a head that arrives after
// its push, its chain then known, spends that push at once.
func TestPushesAreSpentOnceTheirMoveIsJudged(t *testing.T) {
	for _, pushFirst := range []bool{false, true} {
		pr, most := record.PullRequest{HeadSHA: "c0"}, 1
		if pushFirst {
			most = 0
		}
		for i := 1; i <= 10; i++ {
			head, push := fmt.Sprintf("c%d", i), record.ClassifiedPush{SHA: fmt.Sprintf("c%d", i), Before: fmt.Sprintf("c%d", i-1)}
			if pushFirst {
				pr = AdvancePullRequestHead(ApplyPush(pr, push), Arrival{}, head)
			} else {
				pr = ApplyPush(AdvancePullRequestHead(pr, Arrival{}, head), push)
			}
			if len(pr.Pushes) > most {
				t.Fatalf("push first %v: after %d moves %d pushes are kept, want at most %d: %+v", pushFirst, i, len(pr.Pushes), most, pr.Pushes)
			}
		}
	}
}

// A push that links into no move the record makes - one of a line of commits the branch never
// takes, or one behind a webhook that never arrives - is spent by no move. The pull request keeps
// at most maxWaitingPushes of them, letting the oldest go first and saying which it let go
// (ApplyPushDropping), and not counting there the pushes a move spent.
func TestPushesWaitingForALinkAreBounded(t *testing.T) {
	pr := record.PullRequest{HeadSHA: "c0"}
	for i := 1; i <= 80; i++ {
		pr = AdvancePullRequestHead(pr, Arrival{}, fmt.Sprintf("c%d", i))
		if i%2 == 0 {
			continue // this move's push webhook is lost
		}
		var dropped Dropped
		pr, dropped = ApplyPushDropping(pr, record.ClassifiedPush{SHA: fmt.Sprintf("c%d", i-1), Before: fmt.Sprintf("c%d", i-2)})
		if len(dropped.Waiting) != 0 || dropped.Repeat != nil {
			t.Fatalf("after %d moves the push let go %+v, want none: a move spent it", i, dropped)
		}
		if len(pr.Pushes) > maxWaitingPushes {
			t.Fatalf("after %d moves %d pushes are kept, want at most %d", i, len(pr.Pushes), maxWaitingPushes)
		}
	}
	// The one push left waiting is c-1..c0, whose move the record never made; the line's pushes
	// then wait behind it, and each past the bound lets the oldest go, never the one just recorded.
	waiting := pushKeys(pr.Pushes)
	for i := 1; i <= 40; i++ {
		push := record.ClassifiedPush{SHA: fmt.Sprintf("a%d", i), Before: fmt.Sprintf("a%d", i-1)}
		var dropped Dropped
		pr, dropped = ApplyPushDropping(pr, push)
		waiting = append(waiting, pushKey(push))
		var want [][2]string
		if len(waiting) > maxWaitingPushes {
			want, waiting = waiting[:1], waiting[1:]
		}
		if got := pushKeys(dropped.Waiting); !slices.Equal(got, want) {
			t.Fatalf("after %d pushes of a line the branch never takes the pushes let go are %v, want %v: the oldest waiting goes first", i, got, want)
		}
		if got := pushKeys(pr.Pushes); !slices.Equal(got, waiting) {
			t.Fatalf("after %d pushes of a line the branch never takes the pushes kept are %v, want %v", i, got, waiting)
		}
	}
}

// The bound counts and lets go only the pushes waiting for a link. A push that carries an approval
// across stays for the life of the pull request, and a push still on its way forward from the head
// stays until its move is made, however many waiting pushes arrive behind them.
func TestTheBoundKeepsApprovalCarriesAndPushesOnTheirWay(t *testing.T) {
	kept := []record.ClassifiedPush{
		{SHA: "r1", Before: "r0", HandoffOnly: true},               // carries an approval of r0 to r1
		{SHA: "r2", Before: "r1", HandoffOnly: true},               // and on to r2
		{SHA: "h1", Before: "h0"},                                  // code on its way from the head
		{SHA: "h2", Before: "h1", HandoffOnly: true},               // a handoff on its way after it
		{SHA: "h3", Before: "h2", HandoffOnly: true, Forced: true}, // a forced push on its way
	}
	pr := record.PullRequest{HeadSHA: "h0"}
	for _, p := range kept {
		pr = ApplyPush(pr, p)
	}
	var waiting [][2]string
	for i := 1; i <= 3*maxWaitingPushes; i++ {
		push := record.ClassifiedPush{SHA: fmt.Sprintf("w%d", i), Before: fmt.Sprintf("w%d", i-1)}
		var dropped Dropped
		pr, dropped = ApplyPushDropping(pr, push)
		waiting = append(waiting, pushKey(push))
		var want [][2]string
		if len(waiting) > maxWaitingPushes {
			want, waiting = waiting[:1], waiting[1:]
		}
		if got := pushKeys(dropped.Waiting); !slices.Equal(got, want) {
			t.Fatalf("after %d waiting pushes the pushes let go are %v, want %v", i, got, want)
		}
		if got, want := pushKeys(pr.Pushes), append(pushKeys(kept), waiting...); !slices.Equal(got, want) {
			t.Fatalf("after %d waiting pushes the pushes kept are %v, want %v: every approval carry and push on its way, and the newest %d waiting", i, got, want, maxWaitingPushes)
		}
	}
}

// pushKeys is the identities of pushes, in order.
func pushKeys(pushes []record.ClassifiedPush) [][2]string {
	keys := make([][2]string, 0, len(pushes))
	for _, p := range pushes {
		keys = append(keys, pushKey(p))
	}
	return keys
}

// The counted mark names the head the count last counted, and stays across every head pushed on
// top of it until one is pushed onto a green verdict: a push onto a head CI has not judged yet is the
// same attempt, not a new one, so it is neither counted nor clears the mark. A head that was not
// counted takes nothing back when its late push turns out handoff-only.
func TestTheCountedMarkStaysUntilAHeadIsPushedOntoGreen(t *testing.T) {
	counted := AdvancePullRequestHead(record.PullRequest{HeadSHA: "h3", CheckedHead: "h3", Failing: []string{"ci"}, Required: []string{"ci"}, FixAttempts: 2}, Arrival{}, "h4")
	if counted.FixAttempts != 3 || counted.HeadCounted != "h4" {
		t.Fatalf("a push onto red = %#v, want counted h4", counted)
	}
	unjudged := AdvancePullRequestHead(counted, Arrival{}, "h5")
	if unjudged.FixAttempts != 3 || unjudged.HeadCounted != "h4" {
		t.Fatalf("a push onto unjudged h4 = %#v, want the count and the mark on h4 kept", unjudged)
	}
	if !FixAttemptCapHolds(record.PullRequest{HeadSHA: "h5", CheckedHead: "h5", Failing: []string{"ci"}, Required: []string{"ci"}, FixAttempts: 3, HeadCounted: "h4"}, 3) {
		t.Fatal("a red at h5, pushed onto unjudged h4, was not held at the exhausted count")
	}
	if got := ApplyPush(unjudged, record.ClassifiedPush{SHA: "h5", Before: "h4", HandoffOnly: true}); got.FixAttempts != 3 || got.HeadCounted != "h4" {
		t.Fatalf("h5's late handoff-only push = %#v, want nothing taken back: h5 was not counted", got)
	}
	// h4's own late push, a handoff from h3, completes the chain that spares h4's count. h5 has
	// arrived on top of h4, and the head run replays both heads, as their pushes arriving first
	// would have judged them: h4 counts nothing, h3's red is carried to it, and h5, whose push has
	// not arrived, counts onto that red.
	if got := ApplyPush(unjudged, record.ClassifiedPush{SHA: "h4", Before: "h3", HandoffOnly: true}); got.FixAttempts != 3 || got.HeadCounted != "h5" {
		t.Fatalf("h4's late handoff-only push after h5 arrived = fixAttempts=%d headCounted=%q, want 3 and \"h5\": the replay spares h4 and counts h5",
			got.FixAttempts, got.HeadCounted)
	}
	unjudged, _ = ApplyCheckSettlement(unjudged, SettlementCandidate{Head: "h5", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, Snapshot: "green", Failing: []string{}})
	onGreen := AdvancePullRequestHead(unjudged, Arrival{}, "h6")
	if onGreen.FixAttempts != 3 || onGreen.HeadCounted != "" {
		t.Fatalf("a push onto green h5 = %#v, want the count kept and the mark cleared", onGreen)
	}
}

// A push onto a settled head whose base branch requires no check and no workflow leaves the
// counted attempt, as a push onto green does, though that head has no verdict: the mark clears and
// the count stays. A head without a verdict for any other reason - the required set unread, no
// settlement standing for it, a required workflow still running - is the same attempt, so the push
// keeps the mark.
func TestAdvancePullRequestHeadLeavesTheAttemptOnAPushOntoABaseThatRequiresNothing(t *testing.T) {
	marked := record.PullRequest{HeadSHA: "h3", CheckedHead: "h3", Failing: []string{}, Required: []string{},
		FixAttempts: 3, HeadCounted: "h3"}
	for _, tc := range []struct {
		name    string
		pr      func(record.PullRequest) record.PullRequest
		cleared bool
	}{
		{"a settled head whose base requires nothing", func(pr record.PullRequest) record.PullRequest { return pr }, true},
		{"a settled head whose required set was never read", func(pr record.PullRequest) record.PullRequest {
			pr.Required = nil
			return pr
		}, false},
		{"a head no settlement stands for, whose base requires nothing", func(pr record.PullRequest) record.PullRequest {
			pr.CheckedHead = "h1"
			return pr
		}, false},
		{"a settled head whose base requires only a workflow still running", func(pr record.PullRequest) record.PullRequest {
			pr.Workflows, pr.WorkflowsHead = []record.RequiredWorkflow{{Path: "review.yml", Result: Pending}}, "h3"
			return pr
		}, false},
		{"a settled head whose base requires only a workflow that succeeded", func(pr record.PullRequest) record.PullRequest {
			pr.Workflows, pr.WorkflowsHead = []record.RequiredWorkflow{{Path: "review.yml", Result: Success}}, "h3"
			return pr
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := AdvancePullRequestHead(tc.pr(marked), Arrival{}, "h4")
			wantCounted := "h3"
			if tc.cleared {
				wantCounted = ""
			}
			if got.FixAttempts != 3 || got.HeadCounted != wantCounted {
				t.Fatalf("fixAttempts=%d headCounted=%q, want 3 and %q", got.FixAttempts, got.HeadCounted, wantCounted)
			}
		})
	}
}

// An uncounted push onto a red inside the counted attempt - the review App's red tests, or a
// handoff-only push - leaves the attempt's mark where it stood, whichever of the push webhook and
// the synchronize arrives first: synchronize first counts the head until its push is classified,
// and the push's replay of the head run leaves the count and the mark as they were.
func TestAnUncountedPushOntoAMarkedRedKeepsTheMarkInEitherOrder(t *testing.T) {
	for _, tc := range []struct {
		name, paths string
		byReviewApp bool
	}{
		{"the review App's red tests", "src/widget_test.go", true},
		{"a handoff-only push", ".legion/LEGION-208/test.json", false},
	} {
		for _, pushFirst := range []bool{true, false} {
			marked := record.PullRequest{HeadSHA: "h2", CheckedHead: "h2", Failing: []string{"ci"}, Required: []string{"ci"},
				CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, FixAttempts: 3, BlockedAttempts: 3, HeadCounted: "h2"}
			got := arrive(marked, "h3", tc.paths, tc.byReviewApp, pushFirst)
			if got.FixAttempts != 3 || got.BlockedAttempts != 3 || got.HeadCounted != "h2" || got.Run != nil {
				t.Errorf("%s, push first %v: fixAttempts=%d blockedAttempts=%d headCounted=%q run open %v, want 3, 3, \"h2\" and closed",
					tc.name, pushFirst, got.FixAttempts, got.BlockedAttempts, got.HeadCounted, got.Run != nil)
			}
		}
	}
}

func TestApplySettlementUsesTheExportedSettlementClassifiers(t *testing.T) {
	prior := record.PullRequest{HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: "unit", ID: 1}}}
	got, applied := ApplySettlement(prior, SettlementCandidate{
		CheckRuns: []record.AttemptRun{{Name: "unit", ID: 2}}, Generation: 1, Snapshot: "snapshot", Failing: []string{"unit"}, Cancelled: []string{"e2e"},
	})
	if !applied || got.Generation != 1 || got.Snapshot != "snapshot" || len(got.Failing) != 1 || len(got.Cancelled) != 1 {
		t.Fatalf("settlement = %#v applied %t, want red candidate applied", got, applied)
	}
}

func TestBlockFixAttemptPublishesOnlyOncePerExhaustedCount(t *testing.T) {
	prior := record.PullRequest{HeadSHA: "head", CheckedHead: "head", Failing: []string{"ci"}, Required: []string{"ci"}, FixAttempts: 3}
	got, blocked := BlockFixAttempt(prior, 3)
	if !blocked || got.BlockedAttempts != 3 {
		t.Fatalf("first exhausted count = %#v blocked %t, want publish", got, blocked)
	}
	_, blocked = BlockFixAttempt(got, 3)
	if blocked {
		t.Fatal("same exhausted count published twice")
	}
}

// A red sends the tree back only while it stands for the head (HeadVerdict): the head's own red, or
// a code head's carried to the handoff-only head above it, whose code is the same; a red a code push
// left behind, whose head no longer carries it, sends nothing back. The tester's planned red (its
// own failing tests) is the one exception, and a head with no verdict - no settlement, or a required
// set never read - sends nothing.
func TestRedSendsBackOnlyOnTheRedThatStandsForTheHead(t *testing.T) {
	for _, tc := range []struct {
		name string
		pr   record.PullRequest
		back bool
	}{
		{"the head's own red", record.PullRequest{HeadSHA: "head", CheckedHead: "head", Failing: []string{"ci"}, Required: []string{"ci"}}, true},
		{"the code head's red carried to the handoff-only head above it", record.PullRequest{HeadSHA: "head", CheckedHead: "code", Failing: []string{"ci"}, Required: []string{"ci"},
			Pushes: []record.ClassifiedPush{{SHA: "head", Before: "code", HandoffOnly: true}}}, true},
		{"a red the tester planned", record.PullRequest{HeadSHA: "head", CheckedHead: "head", Failing: []string{"ci"}, Required: []string{"ci"}, PlannedRed: true}, false},
		{"a green head", record.PullRequest{HeadSHA: "head", CheckedHead: "head", Failing: []string{}, CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Required: []string{"ci"}}, false},
		{"a red that no longer stands for the head", record.PullRequest{HeadSHA: "fix", CheckedHead: "head", Failing: []string{"ci"}, Required: []string{"ci"}}, false},
		{"a head with no settlement", record.PullRequest{HeadSHA: "head", Required: []string{"ci"}}, false},
		{"a red with the required set unread", record.PullRequest{HeadSHA: "head", CheckedHead: "head", Failing: []string{"ci"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedSendsBack(tc.pr); got != tc.back {
				t.Fatalf("RedSendsBack = %t, want %t", got, tc.back)
			}
		})
	}
}

// A red only required workflows make is one no required check makes: every required check passed,
// is pending or has no result, and at least one required workflow's run ended red. A red required
// check beside a red workflow is not that, and a workflow still running is not red.
func TestRedOnlyByWorkflowsNamesARedNoRequiredCheckMakes(t *testing.T) {
	const review, tests = ".github/workflows/review.yml", ".github/workflows/tests.yml"
	code := func(failing []string, cancelled []string, runs []record.AttemptRun, workflows ...record.RequiredWorkflow) record.PullRequest {
		return record.PullRequest{HeadSHA: "head", CheckedHead: "head", Failing: failing, Cancelled: cancelled, CheckRuns: runs, Required: []string{"ci"},
			Workflows: workflows, WorkflowsHead: "head"}
	}
	green := []record.AttemptRun{{Name: "ci", ID: 1}}
	for _, tc := range []struct {
		name          string
		pr            record.PullRequest
		onlyWorkflows bool
	}{
		{"a workflow failed beside a green check", code([]string{}, nil, green, record.RequiredWorkflow{Path: review, Result: "failure"}), true},
		{"a workflow timed out beside a check with no result", code([]string{}, nil, nil, record.RequiredWorkflow{Path: review, Result: "timed_out"}), true},
		{"a workflow failed beside a cancelled check", code([]string{}, []string{"ci"}, nil, record.RequiredWorkflow{Path: review, Result: "failure"}), true},
		{"one workflow failed beside a green one", code([]string{}, nil, green,
			record.RequiredWorkflow{Path: review, Result: "failure"}, record.RequiredWorkflow{Path: tests, Result: Success}), true},
		{"the required check failed beside a green workflow", code([]string{"ci"}, nil, green, record.RequiredWorkflow{Path: review, Result: Success}), false},
		{"the required check and a workflow failed", code([]string{"ci"}, nil, green, record.RequiredWorkflow{Path: review, Result: "failure"}), false},
		{"a workflow still running beside a green check", code([]string{}, nil, green, record.RequiredWorkflow{Path: review, Result: Pending}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedOnlyByWorkflows(tc.pr); got != tc.onlyWorkflows {
				t.Fatalf("RedOnlyByWorkflows = %t, want %t", got, tc.onlyWorkflows)
			}
		})
	}
}

// A READY stands until the head's own CI turns red: a red the head's own settlement and run bring
// withdraws it, a head a .legion/-only push reached included, while a red carried to it from the
// head before it, which stood before the READY and says nothing new about the submitted head, does
// not; nor does any red on a pull request that is no longer open.
func TestRedWithdrawsReadyOnlyOnTheHeadsOwnRed(t *testing.T) {
	handoff := []record.ClassifiedPush{{SHA: "ready", Before: "code", HandoffOnly: true}}
	for _, tc := range []struct {
		name string
		pr   record.PullRequest
		want bool
	}{
		{"the READY head's own red", record.PullRequest{State: record.PullRequestOpen, HeadSHA: "ready", CheckedHead: "ready", Pushes: handoff, Failing: []string{"ci"}, Required: []string{"ci"}}, true},
		{"its own required workflow run red", record.PullRequest{State: record.PullRequestOpen, HeadSHA: "ready", CheckedHead: "ready", Pushes: handoff, Required: []string{},
			Workflows: []record.RequiredWorkflow{{Path: "review.yml", Result: "failure"}}, WorkflowsHead: "ready"}, true},
		{"a red carried from the code head", record.PullRequest{State: record.PullRequestOpen, HeadSHA: "ready", CheckedHead: "code", Pushes: handoff, Failing: []string{"ci"}, Required: []string{"ci"}}, false},
		{"a closed pull request's red", record.PullRequest{State: record.PullRequestClosed, HeadSHA: "ready", CheckedHead: "ready", Failing: []string{"ci"}, Required: []string{"ci"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RedWithdrawsReady(tc.pr); got != tc.want {
				t.Fatalf("RedWithdrawsReady = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestBlockFixAttemptPublishesOnlyOnARedSettlement(t *testing.T) {
	stale := record.PullRequest{HeadSHA: "fix", CheckedHead: "head", Failing: []string{"ci"}, Required: []string{"ci"}, FixAttempts: 3}
	if got, blocked := BlockFixAttempt(stale, 3); blocked || got.BlockedAttempts != 0 {
		t.Fatalf("a red that no longer stands for the head = %#v blocked %t, want no publish", got, blocked)
	}
	exhausted := record.PullRequest{HeadSHA: "head", CheckedHead: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Required: []string{"ci"}, FixAttempts: 3}
	if got, blocked := BlockFixAttempt(exhausted, 3); blocked || got.BlockedAttempts != 0 {
		t.Fatalf("green settlement at an exhausted count = %#v blocked %t, want no publish", got, blocked)
	}
	exhausted.Failing = []string{"ci"}
	if got, blocked := BlockFixAttempt(exhausted, 3); !blocked || got.BlockedAttempts != 3 {
		t.Fatalf("red settlement at an exhausted count = %#v blocked %t, want publish", got, blocked)
	}
}

// TestApplyDesignGateEventTracksCurrentVersionApproval: an approval of the latest version opens
// the gate and keeps its mark, and a newer version closes it while keeping that approval, mark and
// all, as the version a human approved below the latest (the shared fixtures replay the same rule).
func TestApplyDesignGateEventTracksCurrentVersionApproval(t *testing.T) {
	gate := record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact", LatestVersion: 2}
	approved := record.EventMark{Seq: 12}
	opened := ApplyDesignGateEvent(gate, DesignGateApproved, 2, approved)
	if !DesignGateOpen(opened) || opened.ApprovedMark != approved {
		t.Fatalf("approved gate = %#v, want open with the approval's mark", opened)
	}
	closed := ApplyDesignGateEvent(opened, DesignGateVersion, 3, record.EventMark{})
	if DesignGateOpen(closed) || closed.LatestVersion != 3 || closed.ApprovedVersion == nil || *closed.ApprovedVersion != 2 || closed.ApprovedMark != approved {
		t.Fatalf("new version gate = %#v, want closed at v3 still recording the approval of v2 with its mark", closed)
	}
}

// A review at the version the gate last reviewed is stale when the gate's own review of it is
// later: the person reviewed again after it. The recorded review itself, read again with only its
// time or only its sequence, is not stale, and neither is a review no mark places.
func TestStaleReviewIsAReviewTheGateAlreadyFollowedWithAnother(t *testing.T) {
	at := time.Date(2026, 10, 23, 17, 0, 0, 0, time.UTC)
	recorded := record.EventMark{Seq: 12, At: at}
	approved := record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact", LatestVersion: 4, ApprovedVersion: new(4), ApprovedMark: recorded}
	told := RecordChangesTold(record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact", LatestVersion: 4}, 4, "name the rollback path", recorded)
	for _, tc := range []struct {
		name  string
		gate  record.DesignGate
		mark  record.EventMark
		stale bool
	}{
		{name: "an older review of an approved version", gate: approved, mark: record.EventMark{Seq: 11, At: at.Add(time.Minute)}, stale: true},
		{name: "an older review of a told changes request", gate: told, mark: record.EventMark{Seq: 9}, stale: true},
		{name: "a newer review", gate: approved, mark: record.EventMark{Seq: 13}},
		{name: "the recorded review read with its time alone", gate: approved, mark: record.EventMark{At: at}},
		{name: "a review no mark places", gate: approved},
		{name: "a gate that holds no review", gate: record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact", LatestVersion: 4}, mark: record.EventMark{Seq: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StaleReview(tc.gate, 4, tc.mark); got != tc.stale {
				t.Fatalf("StaleReview(%#v, 4, %#v) = %v, want %v", tc.gate, tc.mark, got, tc.stale)
			}
		})
	}
	completed := RecordChangesTold(RecordChangesTold(record.DesignGate{LatestVersion: 4}, 4, "why", record.EventMark{At: at}), 4, "why", record.EventMark{Seq: 12, At: at})
	if completed.ToldChangesMark != recorded {
		t.Fatalf("told record of one review read by time, then by sequence = %#v, want it completed to %#v", completed.ToldChangesMark, recorded)
	}
	if !ReviewAfter(told, 4, record.EventMark{Seq: 10}) || ReviewAfter(told, 4, record.EventMark{Seq: 12}) {
		t.Fatalf("ReviewAfter of the review at sequence 12 = wrong, want it after a read at 10 and not after one at 12")
	}
}

// arrive applies one head's push webhook and its synchronize in the given order, as the engine
// does: the push classifies the head's changed paths for LEGION-208's branch and names whether its
// pusher was the review App, and the synchronize advances the pull request to the head.
func arrive(pr record.PullRequest, sha, changedPaths string, byReviewApp, pushFirst bool) record.PullRequest {
	truncated := "false"
	classification := ClassifyPush(PushPayload{ChangedPaths: &changedPaths, ChangedPathsTruncated: &truncated}, "LEGION-208")
	push := record.ClassifiedPush{SHA: sha, Before: pr.HeadSHA, HandoffOnly: classification.HandoffOnly,
		Unknown: classification.Unknown, ByReviewApp: byReviewApp}
	if pushFirst {
		return AdvancePullRequestHead(ApplyPush(pr, push), Arrival{}, sha)
	}
	return ApplyPush(AdvancePullRequestHead(pr, Arrival{}, sha), push)
}

// One tester round, in both webhook orders, each step reading the pull request the previous one
// handed over: the review App's red tests set the planned mark, its handoff-only head carries it,
// the implementer's fix clears it without counting the planned red, and a later fix whose push
// webhook is lost counts against the unplanned red and carries the cleared mark.
func TestPlannedRedIsSetCarriedAndClearedAcrossATesterRound(t *testing.T) {
	for _, pushFirst := range []bool{true, false} {
		name := "synchronize first"
		if pushFirst {
			name = "push first"
		}
		t.Run(name, func(t *testing.T) {
			pr := record.PullRequest{HeadSHA: "impl", CheckedHead: "impl", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Required: []string{"ci"}}
			check := func(step, head string, plannedRed bool, fixAttempts int, headCounted string) {
				t.Helper()
				if pr.HeadSHA != head || pr.PlannedRed != plannedRed || pr.FixAttempts != fixAttempts || pr.HeadCounted != headCounted {
					t.Errorf("after %s: head %q plannedRed=%v fixAttempts=%d headCounted=%q, want head %q plannedRed=%v fixAttempts=%d headCounted=%q",
						step, pr.HeadSHA, pr.PlannedRed, pr.FixAttempts, pr.HeadCounted, head, plannedRed, fixAttempts, headCounted)
				}
			}

			pr = arrive(pr, "red-tests", "src/widget_test.go", true, pushFirst)
			check("the review App's red tests", "red-tests", true, 0, "")
			pr.Failing, pr.CheckedHead = []string{"ci"}, pr.HeadSHA

			pr = arrive(pr, "tester-handoff", ".legion/LEGION-208/test.json", true, pushFirst)
			check("the tester's handoff-only head", "tester-handoff", true, 0, "")
			pr.Failing, pr.CheckedHead = []string{"ci"}, pr.HeadSHA

			pr = arrive(pr, "fix", "src/widget.go", false, pushFirst)
			check("the implementer's fix", "fix", false, 0, "")
			pr.Failing, pr.CheckedHead = []string{"ci"}, pr.HeadSHA

			pr = AdvancePullRequestHead(pr, Arrival{}, "fix-2")
			check("a later fix whose push webhook is lost", "fix-2", false, 1, "fix-2")
		})
	}
}
