package classify

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// redAtH0 is where every test of a move made again starts: a red at h0, counted once.
func redAtH0() record.PullRequest {
	return record.PullRequest{HeadSHA: "h0", CheckedHead: "h0", Failing: []string{"ci"}, Required: []string{"ci"},
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, FixAttempts: 1}
}

// delivered is pr once facts are delivered in turn, each through the store's round trip.
func delivered(t *testing.T, pr record.PullRequest, facts ...runFact) record.PullRequest {
	t.Helper()
	for _, f := range facts {
		pr = stored(t, deliver(f, pr))
	}
	return pr
}

// judged is the judgement a test of a move made again compares: the count, the head it counted
// (with a fresh commit's name read as the head made again, so a control compares with it), the
// planned mark, whether the red sends the work back, and whether a head run is open.
func judged(pr record.PullRequest, fresh, again string) string {
	counted := pr.HeadCounted
	if fresh != "" && counted == fresh {
		counted = again
	}
	return fmt.Sprintf("fix=%d counted=%q planned=%v sendsBack=%v run=%v", pr.FixAttempts, counted, pr.PlannedRed, RedSendsBack(pr), pr.Run != nil)
}

// moveAgain is the facts of h0..h1, a forced return h1..h0 and h0..again, a red settling at each
// head, each push delivered before its head or after it (late), each synchronize naming its move.
// by says whose each push was: the review App's or the implementer's.
func moveAgain(by [3]bool, late bool, again string) []runFact {
	var facts []runFact
	for i, s := range []struct {
		push record.ClassifiedPush
		head string
	}{
		{record.ClassifiedPush{SHA: "h1", Before: "h0", ByReviewApp: by[0]}, "h1"},
		{record.ClassifiedPush{SHA: "h0", Before: "h1", ByReviewApp: by[1]}, "h0"},
		{record.ClassifiedPush{SHA: again, Before: "h0", ByReviewApp: by[2]}, again},
	} {
		settle := settles(s.head, int64(i+2), "ci")
		if late {
			facts = append(facts, arrivesFrom(s.push.Before, s.head), settle, pushed(s.push))
		} else {
			facts = append(facts, pushed(s.push), arrivesFrom(s.push.Before, s.head), settle)
		}
	}
	return facts
}

// A move made again is judged by the push that made it again, as a move to a fresh commit is:
// h0..h1, a forced return h1..h0, then h0..h1 pushed again, for every combination of the review
// App's and the implementer's pushes, delivered before their heads and after them, against a
// control whose third push reaches a fresh commit h1b. Judged unchained, as if its push were lost,
// two of them went wrong: the review App's red tests pushed again after the implementer's forced
// return counted a fix attempt and sent the work back, and the implementer's code pushed again
// after the review App's forced return kept the review App's red planned.
func TestAMoveMadeAgainIsJudgedByThePushThatMadeItAgain(t *testing.T) {
	for mask := range 8 {
		by := [3]bool{mask&1 != 0, mask&2 != 0, mask&4 != 0}
		for _, late := range []bool{false, true} {
			got := delivered(t, redAtH0(), moveAgain(by, late, "h1")...)
			control := delivered(t, redAtH0(), moveAgain(by, late, "h1b")...)
			if g, w := judged(got, "", ""), judged(control, "h1b", "h1"); g != w || got.Run != nil {
				t.Errorf("review App's pushes %v, pushes late %v: h1 made again %s, a fresh commit %s", by, late, g, w)
			}
		}
	}
	for _, tc := range []struct {
		name string
		by   [3]bool
		want string
	}{
		{"the review App's red tests pushed again after the implementer's forced return", [3]bool{true, false, true},
			`fix=1 counted="" planned=true sendsBack=false run=false`},
		{"the implementer's code pushed again after the review App's forced return", [3]bool{false, true, false},
			`fix=2 counted="h1" planned=false sendsBack=true run=false`},
	} {
		for _, late := range []bool{false, true} {
			if got := judged(delivered(t, redAtH0(), moveAgain(tc.by, late, "h1")...), "", ""); got != tc.want {
				t.Errorf("%s, pushes late %v: %s, want %s", tc.name, late, got, tc.want)
			}
		}
	}
}

// A repeat stands for the push of the move its arrival's synchronize names, and for nothing else.
// The implementer's h0..h1 is recorded as the move's repeat - delivered twice, or pushed again with
// the synchronizes of its return and of h1 again older than the newest applied, which move nothing
// - and the branch returns to h0; the review App then pushes h0..hx and hx..h1, hx never
// synchronized. h1's synchronize names hx, and no chain reaches hx, so the arrival takes no repeat
// and opens a head run, which the review App's pushes replay whenever they arrive: the review App's
// planned red, counted nothing, in every order, as on main.
func TestARepeatStandsOnlyForTheMoveItsSynchronizeNames(t *testing.T) {
	code, back := record.ClassifiedPush{SHA: "h1", Before: "h0"}, record.ClassifiedPush{SHA: "h0", Before: "h1"}
	tests, onto := record.ClassifiedPush{SHA: "hx", Before: "h0", ByReviewApp: true}, record.ClassifiedPush{SHA: "h1", Before: "hx", ByReviewApp: true}
	const want = `fix=3 counted="h0" planned=true sendsBack=false run=false`
	for _, start := range []struct {
		name  string
		facts []runFact
	}{
		{"delivered twice", []runFact{pushed(code), arrivesFrom("h0", "h1"), settles("h1", 2, "ci"), pushed(code), pushed(back), arrivesFrom("h1", "h0"), settles("h0", 3, "ci")}},
		{"pushed again, its synchronizes stale", []runFact{pushed(code), arrivesFrom("h0", "h1"), settles("h1", 2, "ci"), pushed(back), pushed(code), pushed(back),
			arrivesFrom("h1", "h0"), settles("h0", 3, "ci")}},
	} {
		for _, tail := range []struct {
			name  string
			facts []runFact
		}{
			{"the review App's pushes before h1", []runFact{pushed(tests), pushed(onto), arrivesFrom("hx", "h1"), settles("h1", 4, "ci")}},
			{"h1, its red, then the review App's pushes", []runFact{arrivesFrom("hx", "h1"), settles("h1", 4, "ci"), pushed(tests), pushed(onto)}},
			{"h1, the review App's pushes, then its red", []runFact{arrivesFrom("hx", "h1"), pushed(tests), pushed(onto), settles("h1", 4, "ci")}},
		} {
			if got := judged(delivered(t, redAtH0(), append(append([]runFact{}, start.facts...), tail.facts...)...), "", ""); got != want {
				t.Errorf("%s, %s: %s, want %s", start.name, tail.name, got, want)
			}
		}
	}
}

// An arrival whose synchronize names no move - a reopen, a close at another head, READY, the
// daemon's own read of the head, a listener that predates the field - takes no repeat: h1's second
// arrival naming none waits for a push, as if its push were lost, where the same arrival naming
// its move h0..h1 takes the repeat and closes.
func TestAnArrivalThatNamesNoMoveTakesNoRepeat(t *testing.T) {
	first, back, again := record.ClassifiedPush{SHA: "h1", Before: "h0"}, record.ClassifiedPush{SHA: "h0", Before: "h1"}, record.ClassifiedPush{SHA: "h1", Before: "h0"}
	start := []runFact{pushed(first), arrivesFrom("h0", "h1"), settles("h1", 2, "ci"), pushed(back), arrivesFrom("h1", "h0"), settles("h0", 3, "ci"), pushed(again)}
	if named := delivered(t, redAtH0(), append(append([]runFact{}, start...), arrivesFrom("h0", "h1"))...); named.Run != nil {
		t.Errorf("h1 arriving by the move h0..h1: the run is open (%d steps), want the repeat taken for its push", len(named.Run.Steps))
	}
	if unnamed := delivered(t, redAtH0(), append(append([]runFact{}, start...), arrives("h1"))...); unnamed.Run == nil {
		t.Error("h1 arriving by no move it names: the run is closed, want it open, the repeat not taken")
	}
	if reopened := Reopen(delivered(t, redAtH0(), start...), "h1"); reopened.Run == nil {
		t.Error("a reopen at h1: the run is closed, want it open, the repeat not taken")
	}
}

// An arrival whose synchronize names a head the record did not hold - the synchronizes between were
// missed - takes the repeat on the chain through that head: after h0..h1, a forced return to h0
// and h0..h1 pushed again, the implementer's h1..h2 arrives from h1 with h1's second synchronize
// missed. The chain from the recorded h0 to h1 takes the repeat, and on from h1 to h2 the
// implementer's push, so the run closes, judged as for a fresh commit h1b, with the pushes
// delivered before the head or after it.
func TestARepeatCompletesTheMovesASynchronizeSkipped(t *testing.T) {
	first, back := record.ClassifiedPush{SHA: "h1", Before: "h0", ByReviewApp: true}, record.ClassifiedPush{SHA: "h0", Before: "h1"}
	history := func(again string, late bool) []runFact {
		redo, on := record.ClassifiedPush{SHA: again, Before: "h0", ByReviewApp: true}, record.ClassifiedPush{SHA: "h2", Before: again}
		facts := []runFact{pushed(first), arrivesFrom("h0", "h1"), settles("h1", 2, "ci"), pushed(back), arrivesFrom("h1", "h0"), settles("h0", 3, "ci")}
		if late {
			return append(facts, arrivesFrom(again, "h2"), settles("h2", 4, "ci"), pushed(redo), pushed(on))
		}
		return append(facts, pushed(redo), pushed(on), arrivesFrom(again, "h2"), settles("h2", 4, "ci"))
	}
	for _, late := range []bool{false, true} {
		got, control := delivered(t, redAtH0(), history("h1", late)...), delivered(t, redAtH0(), history("h1b", late)...)
		if g, w := judged(got, "", ""), judged(control, "h1b", "h1"); g != w || got.Run != nil {
			t.Errorf("pushes late %v: h1 made again %s, a fresh commit %s", late, g, w)
		}
	}
}

// A move made again while a head run is open takes its repeat in the run's replay too: the review
// App's c0..h0 opens a run when it is delivered after h0, or holds it open for good when it is lost,
// and the redo h0..h1, forced h1..h0, h0..h1 follows. Every push first, and c0..h0 last, close the
// run judged as for a fresh commit h1b; c0..h0 lost leaves it open, as for h1b.
func TestAMoveMadeAgainInsideAHeadRun(t *testing.T) {
	start := record.PullRequest{HeadSHA: "c0", CheckedHead: "c0", Failing: []string{"ci"}, Required: []string{"ci"},
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, FixAttempts: 1}
	opening := record.ClassifiedPush{SHA: "h0", Before: "c0", ByReviewApp: true}
	first, back := record.ClassifiedPush{SHA: "h1", Before: "h0"}, record.ClassifiedPush{SHA: "h0", Before: "h1"}
	history := func(again string) []runFact {
		redo := record.ClassifiedPush{SHA: again, Before: "h0", ByReviewApp: true}
		return []runFact{arrivesFrom("c0", "h0"), settles("h0", 2, "ci"), pushed(first), arrivesFrom("h0", "h1"), settles("h1", 3, "ci"),
			pushed(back), arrivesFrom("h1", "h0"), settles("h0", 4, "ci"), pushed(redo), arrivesFrom("h0", again), settles(again, 5, "ci")}
	}
	for _, tc := range []struct {
		name string
		with func([]runFact) []runFact
		open bool
	}{
		{"every push first", func(f []runFact) []runFact { return append([]runFact{pushed(opening)}, f...) }, false},
		{"c0..h0 delivered last", func(f []runFact) []runFact { return append(f, pushed(opening)) }, false},
		{"c0..h0 lost", func(f []runFact) []runFact { return f }, true},
	} {
		got, control := delivered(t, start, tc.with(history("h1"))...), delivered(t, start, tc.with(history("h1b"))...)
		if g, w := judged(got, "", ""), judged(control, "h1b", "h1"); g != w || (got.Run != nil) != tc.open {
			t.Errorf("%s: h1 made again %s, a fresh commit %s, want the same and a run open %v", tc.name, g, w, tc.open)
		}
	}
}

// The same move made three times while a head run is open queues each later push of its two heads
// in turn, and each arrival takes the oldest: judged as moves to fresh commits h1, h1b and h1c,
// with c0..h0 delivered last, which closes the run, or lost, which keeps it open.
func TestTheSameMoveMadeThreeTimesInAHeadRun(t *testing.T) {
	start := record.PullRequest{HeadSHA: "c0", CheckedHead: "c0", Failing: []string{"ci"}, Required: []string{"ci"},
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, FixAttempts: 1}
	opening := record.ClassifiedPush{SHA: "h0", Before: "c0", ByReviewApp: true}
	history := func(heads [3]string) []runFact {
		facts := []runFact{arrivesFrom("c0", "h0"), settles("h0", 2, "ci")}
		id := int64(3)
		for i, h := range heads {
			there, back := record.ClassifiedPush{SHA: h, Before: "h0"}, record.ClassifiedPush{SHA: "h0", Before: h}
			facts = append(facts, pushed(there), arrivesFrom("h0", h), settles(h, id, "ci"))
			id++
			if i < 2 {
				facts = append(facts, pushed(back), arrivesFrom(h, "h0"), settles("h0", id, "ci"))
				id++
			}
		}
		return facts
	}
	for _, lost := range []bool{false, true} {
		with := func(f []runFact) []runFact {
			if lost {
				return f
			}
			return append(f, pushed(opening))
		}
		got := delivered(t, start, with(history([3]string{"h1", "h1", "h1"}))...)
		control := delivered(t, start, with(history([3]string{"h1", "h1b", "h1c"}))...)
		if g, w := judged(got, "", ""), judged(control, "h1c", "h1"); g != w || (got.Run != nil) != lost {
			t.Errorf("c0..h0 lost %v: h1 made three times %s, fresh commits %s, want the same and a run open %v", lost, g, w, lost)
		}
	}
}

// Every interleaving of the redo's three pushes with its heads and red settlements, for every
// combination of the review App's and the implementer's pushes, is judged as the same history made
// on a fresh commit h1b, pushes first, and closes its head run - outside R1. R1 is the floor: when
// the two pushes of h0..h1 differ, nothing a push or a synchronize carries says which was made
// first, so a move takes the one delivered first; each order is then judged as the history whose
// two pushes of h0..h1 were made in the order they were delivered, which is the history itself or
// that history with the two swapped.
func TestEveryOrderOfAMoveMadeAgainIsJudgedAsOnAFreshCommit(t *testing.T) {
	truth := func(by [3]bool) string {
		return judged(delivered(t, redAtH0(), moveAgain(by, false, "h1b")...), "h1b", "h1")
	}
	for mask := range 8 {
		by := [3]bool{mask&1 != 0, mask&2 != 0, mask&4 != 0}
		var facts []runFact
		var pushes []record.ClassifiedPush
		for _, f := range moveAgain(by, false, "h1") {
			if f.push != nil {
				pushes = append(pushes, *f.push)
			} else {
				facts = append(facts, f)
			}
		}
		want, swapped := truth(by), truth([3]bool{by[2], by[1], by[0]})
		r1 := by[0] != by[2]
		wrong := 0
		for _, order := range interleavings(facts, pushes) {
			got := judged(delivered(t, redAtH0(), order...), "", "")
			if got == want || (r1 && got == swapped) {
				continue
			}
			if wrong++; wrong <= 3 {
				t.Errorf("review App's pushes %v (R1 %v), order %v: %s, want %s", by, r1, order, got, want)
			}
		}
		if wrong > 3 {
			t.Errorf("review App's pushes %v: %d orders misjudged in all", by, wrong)
		}
	}
}

// A push the record's own bound lets go is let go by an open head run too, so the run never
// decides a head with a push the record no longer holds: h1 arrives with its push lost, h1..x (the
// review App's) and h1..h2 follow, sixteen waiting pushes of a line the branch never takes push the
// oldest of them out of the record, and the branch returns to h1 and moves to x. Delivered first,
// last or never, h0..h1 changes nothing of it.
func TestAPushTheRecordLetsGoIsLetGoByItsHeadRun(t *testing.T) {
	lost := record.ClassifiedPush{SHA: "h1", Before: "h0"}
	facts := []runFact{arrivesFrom("h0", "h1"), settles("h1", 2, "ci"), pushed(record.ClassifiedPush{SHA: "x", Before: "h1", ByReviewApp: true}),
		pushed(record.ClassifiedPush{SHA: "h2", Before: "h1"}), arrivesFrom("h1", "h2"), settles("h2", 3, "ci")}
	for i := 1; i <= maxWaitingPushes; i++ {
		facts = append(facts, pushed(record.ClassifiedPush{SHA: fmt.Sprintf("w%d", i), Before: fmt.Sprintf("w%d", i-1)}))
	}
	facts = append(facts, pushed(record.ClassifiedPush{SHA: "h1", Before: "h2"}), arrivesFrom("h2", "h1"), settles("h1", 4, "ci"), arrivesFrom("h1", "x"))
	const want = `fix=5 counted="x" planned=false sendsBack=false`
	for _, tc := range []struct {
		name  string
		facts []runFact
	}{
		{"h0..h1 first", append([]runFact{pushed(lost)}, facts...)},
		{"h0..h1 last", append(append([]runFact{}, facts...), pushed(lost))},
		{"h0..h1 lost", facts},
	} {
		got := delivered(t, redAtH0(), tc.facts...)
		if g := strings.TrimSuffix(judged(got, "", ""), fmt.Sprintf(" run=%v", got.Run != nil)); g != want {
			t.Errorf("%s: %s, want %s", tc.name, g, want)
		}
		if got.Run != nil && len(got.Run.Pushes) > maxWaitingPushes+2 {
			t.Errorf("%s: the run holds %d pushes, want no more than the record's bound lets it", tc.name, len(got.Run.Pushes))
		}
	}
}

// A head run holds no more pushes than the record's bound leaves it: 300 pushes of a line from an
// earlier head of an open run, and 100 pushes of one move made before, each leave the run at most
// the bound's waiting pushes, the push on its way and the bound's repeats.
func TestAHeadRunHoldsNoMorePushesThanTheRecordsBound(t *testing.T) {
	opened := func() record.PullRequest {
		return delivered(t, redAtH0(), arrivesFrom("h0", "h1"), pushed(record.ClassifiedPush{SHA: "h2", Before: "h1"}), arrivesFrom("h1", "h2"))
	}
	pr := opened()
	for i := 1; i <= 300; i++ {
		before := "h1"
		if i > 1 {
			before = fmt.Sprintf("a%d", i-1)
		}
		pr = delivered(t, pr, pushed(record.ClassifiedPush{SHA: fmt.Sprintf("a%d", i), Before: before}))
	}
	if pr.Run == nil || len(pr.Run.Pushes) > maxWaitingPushes+1 || len(pr.Pushes) > maxWaitingPushes {
		t.Errorf("a line of 300 pushes: the record holds %d pushes and the run %v, want at most %d and %d", len(pr.Pushes), pr.Run, maxWaitingPushes, maxWaitingPushes+1)
	}
	pr = opened()
	for i := 1; i <= 100; i++ {
		pr = delivered(t, pr, pushed(record.ClassifiedPush{SHA: "h2", Before: "h1", ByReviewApp: i%2 == 0}))
	}
	if pr.Run == nil || len(pr.Run.Pushes) > maxRepeats+1 {
		t.Errorf("100 pushes of h1..h2 again: the run %v, want at most %d pushes", pr.Run, maxRepeats+1)
	}
	for _, m := range pr.Moves {
		if len(m.Repeats) > maxRepeats {
			t.Errorf("the move %s..%s queues %d repeats, want at most %d", m.Before, m.SHA, len(m.Repeats), maxRepeats)
		}
	}
}

// Two histories a mutation of a single repeat slot passed: a move first made by a chain of two
// pushes, h0..hx..h1, then made again by one push h0..h1; and the same move made a third time by
// the review App, its push before its head or after it. Each is judged as the same history on a
// fresh commit h1b.
func TestAMoveMadeByAChainAndMadeAgainByAPush(t *testing.T) {
	tests, onto := record.ClassifiedPush{SHA: "hx", Before: "h0", ByReviewApp: true}, record.ClassifiedPush{SHA: "h1", Before: "hx", ByReviewApp: true}
	backByApp := record.ClassifiedPush{SHA: "h0", Before: "h1", ByReviewApp: true}
	back := record.ClassifiedPush{SHA: "h0", Before: "h1"}
	for _, late := range []bool{false, true} {
		tail := func(push record.ClassifiedPush, id int64) []runFact {
			if late {
				return []runFact{arrivesFrom("h0", push.SHA), settles(push.SHA, id, "ci"), pushed(push)}
			}
			return []runFact{pushed(push), arrivesFrom("h0", push.SHA), settles(push.SHA, id, "ci")}
		}
		chained := []runFact{pushed(tests), pushed(onto), arrivesFrom("hx", "h1"), settles("h1", 2, "ci"), pushed(backByApp), arrivesFrom("h1", "h0"), settles("h0", 3, "ci")}
		got := delivered(t, redAtH0(), append(append([]runFact{}, chained...), tail(record.ClassifiedPush{SHA: "h1", Before: "h0"}, 4)...)...)
		control := delivered(t, redAtH0(), append(append([]runFact{}, chained...), tail(record.ClassifiedPush{SHA: "h1b", Before: "h0"}, 4)...)...)
		if g, w := judged(got, "", ""), judged(control, "h1b", "h1"); g != w || got.Run != nil {
			t.Errorf("made by a chain, then by a push, late %v: %s, a fresh commit %s", late, g, w)
		}
		twice := []runFact{pushed(record.ClassifiedPush{SHA: "h1", Before: "h0"}), arrivesFrom("h0", "h1"), settles("h1", 2, "ci"), pushed(back), arrivesFrom("h1", "h0"), settles("h0", 3, "ci"),
			pushed(record.ClassifiedPush{SHA: "h1", Before: "h0"}), arrivesFrom("h0", "h1"), settles("h1", 4, "ci"), pushed(backByApp), arrivesFrom("h1", "h0"), settles("h0", 5, "ci")}
		got = delivered(t, redAtH0(), append(append([]runFact{}, twice...), tail(record.ClassifiedPush{SHA: "h1", Before: "h0", ByReviewApp: true}, 6)...)...)
		control = delivered(t, redAtH0(), append(append([]runFact{}, twice...), tail(record.ClassifiedPush{SHA: "h1b", Before: "h0", ByReviewApp: true}, 6)...)...)
		if g, w := judged(got, "", ""), judged(control, "h1b", "h1"); g != w || got.Run != nil {
			t.Errorf("made a third time by the review App, late %v: %s, a fresh commit %s", late, g, w)
		}
	}
}
