package classify

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

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

// A push for a later move that arrives before the move's first link waits for its chain, keeping
// the head run open: the review App's red tests h4..h5 arrive first, then h3 arrives over h2..h3,
// then h5 past h4, and the handoff h3..h4 last. Only that handoff completes the chain
// h3..h4..h5, which closes the run at the same moves the pushes arriving first would have made.
func TestAPushForALaterMoveWaitsForItsChain(t *testing.T) {
	red := record.PullRequest{HeadSHA: "h2", CheckedHead: "h2", Failing: []string{"ci"},
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}}
	first := record.ClassifiedPush{SHA: "h3", Before: "h2", HandoffOnly: true}
	link := record.ClassifiedPush{SHA: "h4", Before: "h3", HandoffOnly: true}
	tests := record.ClassifiedPush{SHA: "h5", Before: "h4", ByReviewApp: true}
	early := AdvancePullRequestHead(ApplyPush(ApplyPush(ApplyPush(red, first), link), tests), Arrival{}, "h3")
	early = AdvancePullRequestHead(early, Arrival{}, "h5")
	late := AdvancePullRequestHead(ApplyPush(ApplyPush(red, tests), first), Arrival{}, "h3")
	late = AdvancePullRequestHead(late, Arrival{}, "h5")
	if late.Run == nil {
		t.Fatal("h5 arrived before its chain with no head run open, want it waiting for the link")
	}
	late = ApplyPush(late, link)
	if early.Run != nil || late.Run != nil || JudgementChanged(early, late) {
		t.Errorf("the link last = %+v, want the pushes-first %+v with no run open", judgementOf(late), judgementOf(early))
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

// Dispatch's redelivery sweep can deliver a push up to three days late, after its head arrived and
// the branch moved on, under a delivery of its own. The push is the move's the record already
// judged, and it changes nothing: not the moves made or the settlement that stands, and not the
// pushes the record keeps, whether it is a push the record spent or one it keeps (a handoff
// carries an approval for good) and recorded once under its two heads. A push so old that the
// record has let its move go (maxMoves) waits for a link no head brings and, with no head run
// open, judges nothing and is no code on its way.
func TestALatePushOfAMoveJudgedSinceChangesNothing(t *testing.T) {
	red := record.PullRequest{HeadSHA: "h0", CheckedHead: "h0", Failing: []string{"ci"},
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1}
	code := record.ClassifiedPush{SHA: "h1", Before: "h0"}
	tests := record.ClassifiedPush{SHA: "h2", Before: "h1", ByReviewApp: true}
	handoff := record.ClassifiedPush{SHA: "h3", Before: "h2", HandoffOnly: true}
	pr := red
	for i, p := range []record.ClassifiedPush{code, tests, handoff} {
		pr = stored(t, AdvancePullRequestHead(ApplyPush(pr, p), Arrival{}, p.SHA))
		pr = stored(t, settles(p.SHA, int64(i+2), "ci").apply(pr))
	}
	if pr.Run != nil {
		t.Fatal("after the three moves a head run is open, want every move judged with its chain and no run")
	}
	for _, late := range []record.ClassifiedPush{code, tests, handoff} {
		got := stored(t, ApplyPush(pr, late))
		if JudgementChanged(pr, got) || got.Run != nil || !slices.Equal(got.Pushes, pr.Pushes) {
			t.Fatalf("the late push %s..%s changed the record: judgement %+v pushes %+v run open %v; was %+v pushes %+v",
				late.Before, late.SHA, judgementOf(got), got.Pushes, got.Run != nil, judgementOf(pr), pr.Pushes)
		}
	}
	old := pr
	for i := 4; i < 4+maxMoves; i++ {
		p := record.ClassifiedPush{SHA: fmt.Sprintf("h%d", i), Before: fmt.Sprintf("h%d", i-1)}
		old = stored(t, AdvancePullRequestHead(ApplyPush(old, p), Arrival{}, p.SHA))
	}
	if moveIndex(old.Moves, code.Before, code.SHA) >= 0 {
		t.Fatalf("after %d more moves the record still remembers the move %s..%s", maxMoves, code.Before, code.SHA)
	}
	if got := stored(t, ApplyPush(old, code)); JudgementChanged(old, got) || got.Run != nil || CodeOnItsWay(got) {
		t.Fatalf("the push of a move the record let go changed the judgement %+v to %+v, run open %v, code on its way %v",
			judgementOf(old), judgementOf(got), got.Run != nil, CodeOnItsWay(got))
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

func TestApplySettlementUsesTheExportedSettlementClassifiers(t *testing.T) {
	prior := record.PullRequest{HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: "unit", ID: 1}}}
	got, applied := ApplySettlement(prior, SettlementCandidate{
		CheckRuns: []record.AttemptRun{{Name: "unit", ID: 2}}, Generation: 1, Snapshot: "snapshot", Failing: []string{"unit"}, Cancelled: []string{"e2e"},
	})
	if !applied || got.Generation != 1 || got.Snapshot != "snapshot" || len(got.Failing) != 1 || len(got.Cancelled) != 1 {
		t.Fatalf("settlement = %#v applied %t, want red candidate applied", got, applied)
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
