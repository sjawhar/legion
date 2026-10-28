package classify

import (
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// syncedAt is a synchronize that names its move from before and its sender, at minute of GitHub's
// clock.
func syncedAt(before, head, sender string, minute int) runFact {
	return runFact{head: head, before: before, sender: sender, at: time.Date(2026, 10, 1, 0, minute, 0, 0, time.UTC)}
}

// lateAt is syncedAt delivered late: after a newer synchronize was applied.
func lateAt(before, head, sender string, minute int) runFact {
	f := syncedAt(before, head, sender, minute)
	f.late = true
	return f
}

// A move made twice by different senders is judged by each one's own push, delivered in either
// order: h0..h1 by the implementer, a forced return h1..h0 by the review App, and h0..h1 by the
// review App - its red tests pushed again. The two pushes of h0..h1 differ only in who pushed
// them, which a rule over the pushes alone cannot order (R1); each synchronize names its sender, so
// the second arrival of h1 takes the review App's push and plans the red, whichever push of h0..h1
// GitHub delivers first.
func TestASynchronizesSenderTellsTwoPushesOfTheSameTwoHeadsApart(t *testing.T) {
	dev, app := record.ClassifiedPush{SHA: "h1", Before: "h0", Sender: "dev"}, record.ClassifiedPush{SHA: "h1", Before: "h0", ByReviewApp: true, Sender: "app"}
	back := record.ClassifiedPush{SHA: "h0", Before: "h1", ByReviewApp: true, Sender: "app"}
	heads := []runFact{syncedAt("h0", "h1", "dev", 1), settles("h1", 2, "ci"), syncedAt("h1", "h0", "app", 2), settles("h0", 3, "ci"), syncedAt("h0", "h1", "app", 3), settles("h1", 4, "ci")}
	const want = `fix=2 counted="h1" planned=true sendsBack=false run=false`
	for _, tc := range []struct {
		name   string
		pushes []runFact
	}{
		{"made in order", []runFact{pushed(dev), pushed(back), pushed(app)}},
		{"the review App's push first", []runFact{pushed(app), pushed(back), pushed(dev)}},
	} {
		for _, late := range []bool{false, true} {
			facts := append(append([]runFact{}, tc.pushes...), heads...)
			if late {
				facts = append(append([]runFact{}, heads...), tc.pushes...)
			}
			if got := judged(delivered(t, redAtH0(), facts...), "", ""); got != want {
				t.Errorf("%s, pushes after their heads %v: %s, want %s", tc.name, late, got, want)
			}
		}
	}
}

// A synchronize to the head the record holds is no move, as the release before read it, whatever
// it names: the record holds c6, red; the implementer resets the branch to c5, its synchronize late,
// and pushes c6 again. The synchronize c5>c6 counts no attempt, as on main: the branch back at the
// recorded head after a move whose synchronize was skipped is a loop in a skipped gap (the loop
// floor), which the comparison with main on both oracles judges better read as no move.
func TestASynchronizeBackToTheRecordedHeadIsNoMove(t *testing.T) {
	start := record.PullRequest{HeadSHA: "c6", CheckedHead: "c6", Failing: []string{"ci"}, Required: []string{"ci"},
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, FixAttempts: 1, HeadUpdatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	got := delivered(t, start, pushed(record.ClassifiedPush{SHA: "c5", Before: "c6", Sender: "dev"}), pushed(record.ClassifiedPush{SHA: "c6", Before: "c5", Sender: "dev"}),
		syncedAt("c5", "c6", "dev", 2), lateAt("c6", "c5", "dev", 1))
	if got.FixAttempts != 1 || got.HeadSHA != "c6" || got.Run != nil {
		t.Errorf("c6 pushed again: %s at %s, want fix=1 at c6 with no run open, as the release before", judged(got, "", ""), got.HeadSHA)
	}
	if MovesBy(start, "c6") || !MovesBy(start, "c7") {
		t.Error("MovesBy: want no move to the recorded head and a move to another")
	}
}

// An arrival whose synchronize names a head the record never held waits for the moves between,
// which the late synchronizes name, and is judged by their pushes in the order GitHub's clock puts
// them: h0..c1 by the implementer, c1..c2 by the review App, the first synchronize late. Delivered
// before it or after it, the late one closes the head run with the implementer's push counted.
func TestAnArrivalTakesTheMovesALateSynchronizeNames(t *testing.T) {
	code, tests := record.ClassifiedPush{SHA: "c1", Before: "h0", Sender: "dev"}, record.ClassifiedPush{SHA: "c2", Before: "c1", ByReviewApp: true, Sender: "app"}
	for _, tc := range []struct {
		name  string
		facts []runFact
	}{
		{"pushes first", []runFact{pushed(code), pushed(tests), syncedAt("c1", "c2", "app", 2), lateAt("h0", "c1", "dev", 1)}},
		{"pushes last", []runFact{syncedAt("c1", "c2", "app", 2), lateAt("h0", "c1", "dev", 1), pushed(tests), pushed(code)}},
	} {
		got := delivered(t, redAtH0(), tc.facts...)
		if got.Run != nil || got.FixAttempts != 2 || !got.PlannedRed {
			t.Errorf("%s: run open %v, fix=%d planned=%v, want closed, 2 (the implementer's push counts) and planned (the review App's push last)", tc.name, got.Run != nil, got.FixAttempts, got.PlannedRed)
		}
	}
	waiting := delivered(t, redAtH0(), pushed(code), pushed(tests), syncedAt("c1", "c2", "app", 2))
	if waiting.Run == nil {
		t.Error("the late synchronize not yet delivered: the head run is closed, want it waiting for the move it names")
	}
}

// NameMove changes nothing - the record returned as it was - with no head run open, with one none
// of whose arrivals it can be a skipped move of, when it names no sender, or when it was named
// already; and records the move otherwise.
func TestNameMoveRecordsOnlyAMoveAnArrivalWaitsFor(t *testing.T) {
	open := delivered(t, redAtH0(), syncedAt("c1", "c2", "app", 5))
	if open.Run == nil {
		t.Fatal("no head run waits for the move to c1")
	}
	at := func(minute int) time.Time { return time.Date(2026, 10, 1, 0, minute, 0, 0, time.UTC) }
	named := record.Named{Before: "h0", SHA: "c1", Sender: "dev", At: at(3)}
	open.HeadUpdatedAt = at(5)
	recorded := NameMove(open, named)
	for _, tc := range []struct {
		name string
		pr   record.PullRequest
		move record.Named
	}{
		{"with no head run open", redAtH0(), named},
		{"older than the head run", func() record.PullRequest { pr := open; run := *pr.Run; run.Since = at(4); pr.Run = &run; return pr }(), named},
		{"newer than every arrival", open, record.Named{Before: "h0", SHA: "c1", Sender: "dev", At: at(6)}},
		{"naming no sender", open, record.Named{Before: "h0", SHA: "c1", At: at(3)}},
		{"named already", recorded, named},
	} {
		if got := NameMove(tc.pr, tc.move); fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", tc.pr) {
			t.Errorf("%s: the record changed", tc.name)
		}
	}
	if recorded.Run == nil || len(recorded.Run.Names) != 1 || recorded.HeadSHA != open.HeadSHA {
		t.Errorf("the move the arrival waits for: run %+v, head %s, want it named and the head where it was", recorded.Run, recorded.HeadSHA)
	}
}

// A move queues the later pushes of its two heads oldest first, and past maxRepeats lets the
// oldest go: seventeen pushes of h0..h1 by seventeen senders after the move was made keep the
// newest sixteen in the order they arrived, and name the first as the one let go.
func TestAMoveLetsItsOldestRepeatGoFirst(t *testing.T) {
	pr := delivered(t, redAtH0(), pushed(record.ClassifiedPush{SHA: "h1", Before: "h0", Sender: "s0"}), syncedAt("h0", "h1", "s0", 1))
	var dropped Dropped
	for i := 1; i <= maxRepeats+1; i++ {
		pr, dropped = ApplyPushDropping(pr, record.ClassifiedPush{SHA: "h1", Before: "h0", Sender: fmt.Sprintf("s%d", i)})
		if (dropped.Repeat != nil) != (i == maxRepeats+1) {
			t.Fatalf("repeat %d let go %+v", i, dropped.Repeat)
		}
	}
	if dropped.Repeat.Sender != "s1" {
		t.Errorf("let go %s's push, want s1's, the oldest", dropped.Repeat.Sender)
	}
	i := moveIndex(pr.Moves, "h0", "h1")
	if i < 0 || len(pr.Moves[i].Repeats) != maxRepeats {
		t.Fatalf("moves %+v, want h0..h1 with %d repeats", pr.Moves, maxRepeats)
	}
	for k, p := range pr.Moves[i].Repeats {
		if want := fmt.Sprintf("s%d", k+2); p.Sender != want {
			t.Errorf("repeat %d is %s's, want %s's", k, p.Sender, want)
		}
	}
}

// secondAt is a synchronize naming its move and sender at second of GitHub's clock, late or not.
func secondAt(before, head, sender string, second int, late bool) runFact {
	return runFact{head: head, before: before, sender: sender, at: time.Date(2026, 10, 1, 0, 0, second, 0, time.UTC), late: late}
}

// GitHub's clock is to the second, so a skipped move can share its second with the record's head,
// and two skipped moves can share one second and be delivered in either order. Each closes the
// head run the moment the moves are named, judged as the same history in distinct seconds.
func TestSkippedMovesInOneSecondCloseTheirHeadRun(t *testing.T) {
	dev, app := "dev", "app"
	pushes := []runFact{pushed(record.ClassifiedPush{SHA: "h1", Before: "h0", Sender: dev}), pushed(record.ClassifiedPush{SHA: "c1", Before: "h1", Sender: dev}),
		pushed(record.ClassifiedPush{SHA: "c2", Before: "c1", ByReviewApp: true, Sender: app}), pushed(record.ClassifiedPush{SHA: "c3", Before: "c2", Sender: dev})}
	for _, tc := range []struct {
		name          string
		tied, control []runFact
	}{
		{"a skipped move in the second of the record's head",
			[]runFact{secondAt("h0", "h1", dev, 1, false), settles("h1", 2, "ci"), secondAt("c2", "c3", dev, 3, false), secondAt("c1", "c2", app, 2, true), secondAt("h1", "c1", dev, 1, true)},
			[]runFact{secondAt("h0", "h1", dev, 1, false), settles("h1", 2, "ci"), secondAt("c2", "c3", dev, 4, false), secondAt("c1", "c2", app, 3, true), secondAt("h1", "c1", dev, 2, true)}},
		{"two skipped moves in one second, delivered in the reverse of their order",
			[]runFact{secondAt("h0", "h1", dev, 1, false), settles("h1", 2, "ci"), secondAt("c2", "c3", dev, 3, false), secondAt("c1", "c2", app, 2, true), secondAt("h1", "c1", dev, 2, true)},
			[]runFact{secondAt("h0", "h1", dev, 1, false), settles("h1", 2, "ci"), secondAt("c2", "c3", dev, 4, false), secondAt("c1", "c2", app, 3, true), secondAt("h1", "c1", dev, 2, true)}},
	} {
		got := delivered(t, redAtH0(), append(append([]runFact{}, pushes...), tc.tied...)...)
		want := delivered(t, redAtH0(), append(append([]runFact{}, pushes...), tc.control...)...)
		if got.Run != nil || JudgementChanged(want, got) || got.HeadSHA != "c3" {
			t.Errorf("%s: run open %v, %s at %s; want closed and %s", tc.name, got.Run != nil, judged(got, "", ""), got.HeadSHA, judged(want, "", ""))
		}
	}
}

// Moves of one second that lead from the record's head to before in two orders leave the path
// unled, and the arrival waits for good, judged meanwhile by the newest push of each two heads:
// h0..c1 and c1..h0 in one second, then c1 arriving from h0's later move, cannot tell which loop
// the branch made.
func TestTiedMovesInTwoOrdersLeaveThePathUnled(t *testing.T) {
	names := []record.Named{
		{Before: "h0", SHA: "c1", Sender: "dev", At: time.Date(2026, 10, 1, 0, 0, 2, 0, time.UTC)},
		{Before: "c1", SHA: "h0", Sender: "dev", At: time.Date(2026, 10, 1, 0, 0, 2, 0, time.UTC)},
		{Before: "h0", SHA: "c1", Sender: "app", At: time.Date(2026, 10, 1, 0, 0, 2, 0, time.UTC)},
	}
	if path, ok := gapPath(names, nil, "h0", "c1"); ok {
		t.Errorf("gapPath %v led, want unled: two orders of one second lead from h0 to c1", path)
	}
	if path, ok := gapPath(names[:2], nil, "h0", "h0"); !ok || len(path) != 2 || path[0].Before != "h0" {
		t.Errorf("gapPath %v, %v; want h0>c1>h0, the one order that leads", path, ok)
	}
}

// Of the paths a window's moves lead by, the one taking the fewest moves of the second the window
// starts or ends in is the path: an arrival from the recorded head, c2>c3, in the second of two
// later skipped moves c3>c2 and c2>c3 - the next arrivals' - takes the empty path, not the loop
// c2>c3>c2 they also make.
func TestAPathTakesTheFewestMovesOfItsWindowsSeconds(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 2, 0, time.UTC)
	optional := []record.Named{{Before: "c3", SHA: "c2", Sender: "bob", At: at}, {Before: "c2", SHA: "c3", Sender: "bob", At: at}}
	if path, ok := gapPath(nil, optional, "c2", "c2"); !ok || len(path) != 0 {
		t.Errorf("gapPath %v, %v; want the empty path", path, ok)
	}
	if path, ok := gapPath(optional[:1], optional[1:], "c3", "c2"); !ok || len(path) != 1 {
		t.Errorf("gapPath %v, %v; want c3>c2 alone, the required move", path, ok)
	}
}

// A second copy of the synchronize that reached the recorded head - GitHub can send two for one
// push - is no move, whatever its clock: h0>h1 applied, then a copy of h0>h1 a second later,
// counts no fix attempt and opens no head run.
func TestACopyOfTheSynchronizeThatReachedTheHeadIsNoMove(t *testing.T) {
	code := record.ClassifiedPush{SHA: "h1", Before: "h0", Sender: "dev"}
	start := delivered(t, redAtH0(), pushed(code), secondAt("h0", "h1", "dev", 1, false), settles("h1", 2, "ci"))
	copied := delivered(t, start, secondAt("h0", "h1", "dev", 2, false))
	if copied.Run != nil || copied.FixAttempts != start.FixAttempts {
		t.Errorf("a copy of h0>h1 a second later: run open %v, fix %d; want closed and %d", copied.Run != nil, copied.FixAttempts, start.FixAttempts)
	}
}

// Pushes of the same two heads on their way, before their move is made, are bounded as they arrive
// (maxCopies): the first, which the move will take, and the newest maxRepeats, the oldest after the
// first let go and named (Dropped.Copy). One hundred pushes of h1..h2 while a head run waits leave
// at most maxCopies in the record and in the run.
func TestCopiesOfTwoHeadsOnTheirWayAreBounded(t *testing.T) {
	pr := delivered(t, redAtH0(), arrivesFrom("h0", "h1"))
	if pr.Run == nil {
		t.Fatal("no head run waits for h0..h1")
	}
	var named []string
	for i := range 100 {
		var dropped Dropped
		pr, dropped = ApplyPushDropping(pr, record.ClassifiedPush{SHA: "h2", Before: "h1", Sender: fmt.Sprintf("s%d", i)})
		if dropped.Copy != nil {
			named = append(named, dropped.Copy.Sender)
		}
	}
	count := func(pushes []record.ClassifiedPush) int {
		n := 0
		for _, p := range pushes {
			if p.Before == "h1" && p.SHA == "h2" {
				n++
			}
		}
		return n
	}
	if got := count(pr.Pushes); got != maxCopies {
		t.Errorf("the record holds %d pushes of h1..h2, want %d", got, maxCopies)
	}
	if pr.Run == nil || count(pr.Run.Pushes) != maxCopies {
		t.Errorf("the head run holds %v pushes of h1..h2, want %d", pr.Run, maxCopies)
	}
	if len(named) != 100-maxCopies || named[0] != "s1" || pr.Pushes[0].Sender != "s0" {
		t.Errorf("let go %d (first %v), the record's first is %s's; want %d, s1's first, and s0's kept", len(named), named[:1], pr.Pushes[0].Sender, 100-maxCopies)
	}
}

// A loop in a skipped gap is read as the release before reads it, by the shortest chain of its
// pushes (withoutLoops): the record at h1, red; the review App's h1..c1, the implementer's code
// c1..c2, the review App's forced return c2..c1 and its c1..c3, every synchronize but c1..c3's late,
// the loop's first, so the path from h1 takes the loop. The loop c1>c2>c1 is erased, so the move is the review App's alone: no fix attempt, as with no
// move named. Counting the loop's pushes, the implementer's code would count one.
func TestALoopInASkippedGapIsReadAsTheReleaseBeforeReadsIt(t *testing.T) {
	start := record.PullRequest{HeadSHA: "h1", CheckedHead: "h1", Failing: []string{"ci"}, Required: []string{"ci"},
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, FixAttempts: 1, HeadUpdatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	facts := []runFact{
		pushed(record.ClassifiedPush{SHA: "c1", Before: "h1", ByReviewApp: true, Sender: "app"}),
		pushed(record.ClassifiedPush{SHA: "c2", Before: "c1", Sender: "dev"}),
		pushed(record.ClassifiedPush{SHA: "c1", Before: "c2", ByReviewApp: true, Sender: "app"}),
		pushed(record.ClassifiedPush{SHA: "c3", Before: "c1", ByReviewApp: true, Sender: "app"}),
		syncedAt("c1", "c3", "app", 4), syncedAt("c1", "c2", "dev", 2), syncedAt("c2", "c1", "app", 3), syncedAt("h1", "c1", "app", 1),
	}
	got := delivered(t, start, facts...)
	unnamed := make([]runFact, len(facts))
	for i, f := range facts {
		unnamed[i] = namingNoMove(f)
	}
	asBefore := delivered(t, start, unnamed...)
	if got.Run != nil || got.HeadSHA != "c3" || JudgementChanged(asBefore, got) || got.FixAttempts != 1 {
		t.Errorf("the loop in the gap: %s at %s, run open %v; want %s, as with no move named", judged(got, "", ""), got.HeadSHA, got.Run != nil, judged(asBefore, "", ""))
	}
}

// A move after a reset whose synchronize was missed is a move, though GitHub's clock puts it in the
// newest applied second and its head was held before: h0..h1 and h1..h2 arrive, the branch is reset
// h2..h0 - its synchronize missed, its push delivered or missed too - and moves h0..h1 again in the
// same second as h2's. Lateness is GitHub's clock alone, as on main, so the head is h1, as on main,
// and its run waits for the reset's synchronize (the floor of a synchronize never delivered).
func TestAMoveAfterAMissedResetInTheSameSecondIsAMove(t *testing.T) {
	const dev = "dev"
	t1, t2 := time.Date(2026, 10, 1, 0, 0, 1, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 2, 0, time.UTC)
	reset := record.ClassifiedPush{Before: "h2", SHA: "h0", Sender: dev}
	for _, tc := range []struct {
		name  string
		reset []runFact
		open  bool
	}{
		{"the reset's push delivered", []runFact{pushed(reset)}, true},
		{"the reset's push missed too", nil, true},
	} {
		facts := []runFact{pushed(record.ClassifiedPush{Before: "h0", SHA: "h1", Sender: dev}), {head: "h1", before: "h0", sender: dev, at: t1}, settles("h1", 2, "ci"),
			pushed(record.ClassifiedPush{Before: "h1", SHA: "h2", Sender: dev}), {head: "h2", before: "h1", sender: dev, at: t2}, settles("h2", 3, "ci")}
		facts = append(append(facts, tc.reset...), pushed(record.ClassifiedPush{Before: "h0", SHA: "h1", Sender: dev}))
		pr := delivered(t, redAtH0(), facts...)
		pr = delivered(t, pr, runFact{head: "h1", before: "h0", sender: dev, at: t2})
		if pr.HeadSHA != "h1" || (pr.Run != nil) != tc.open {
			t.Errorf("%s: head %s, run open %v; want h1, open %v: the move waits for the reset's synchronize", tc.name, pr.HeadSHA, pr.Run != nil, tc.open)
		}
	}
}

// No synchronize main would apply is dropped: a synchronize in the newest applied second is a move,
// whatever the record knows of its heads (lateness is GitHub's clock alone, LateLifecycle). Two
// histories that a same-second lateness rule read as late and lost, each moving the head as on main:
//
//   - A stale push. h2's synchronize names x, which the record never held, so its run waits for x;
//     a push y..x not on the branch's path is recorded, and the branch, having passed b with its
//     push and synchronize missed, moves b..y in h2's second.
//   - An awaited head reached again. h0..h1's synchronize is missed, h1..h2 arrives, so the run
//     waits for h1; the branch is reset h2..h0 with both its webhooks missed, and moves h0..h1 in
//     h2's second.
func TestNoSynchronizeMainWouldApplyIsDropped(t *testing.T) {
	const dev = "dev"
	second := time.Date(2026, 10, 1, 0, 0, 1, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		facts []runFact
		move  runFact
	}{
		{"a stale push to an awaited head", []runFact{{head: "h2", before: "x", sender: dev, at: second},
			pushed(record.ClassifiedPush{Before: "y", SHA: "x", Sender: dev}), pushed(record.ClassifiedPush{Before: "b", SHA: "y", Sender: dev})},
			runFact{head: "y", before: "b", sender: dev, at: second}},
		{"an awaited head reached again after a missed reset", []runFact{pushed(record.ClassifiedPush{Before: "h0", SHA: "h1", Sender: dev}),
			pushed(record.ClassifiedPush{Before: "h1", SHA: "h2", Sender: dev}), {head: "h2", before: "h1", sender: dev, at: second},
			pushed(record.ClassifiedPush{Before: "h0", SHA: "h1", Sender: dev})},
			runFact{head: "h1", before: "h0", sender: dev, at: second}},
	} {
		pr := delivered(t, redAtH0(), tc.facts...)
		if pr.Run == nil {
			t.Fatalf("%s: no head run waits", tc.name)
		}
		if pr = delivered(t, pr, tc.move); pr.HeadSHA != tc.move.head {
			t.Errorf("%s: the record is at %s, want %s: the synchronize moves the head, as on main", tc.name, pr.HeadSHA, tc.move.head)
		}
	}
}
