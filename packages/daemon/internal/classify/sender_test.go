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
	back := record.ClassifiedPush{SHA: "h0", Before: "h1", Forced: true, ByReviewApp: true, Sender: "app"}
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

// A synchronize to the head the record holds, from another head and newer than the newest applied,
// is a move (MovesBy): the branch left the head and came back, the synchronize of the move away
// delivered late. The record holds c6, red; the implementer resets the branch to c5 and pushes c6
// again. Its synchronize c5>c6 counts the attempt, as the move to a fresh commit c6b does - c6's
// own red still stands for it, being the same commit; with no sender named it is no move, as the
// release before read it.
func TestASynchronizeBackToTheRecordedHeadIsAMove(t *testing.T) {
	start := record.PullRequest{HeadSHA: "c6", CheckedHead: "c6", Failing: []string{"ci"}, Required: []string{"ci"},
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, FixAttempts: 1, HeadUpdatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	history := func(again string) []runFact {
		return []runFact{pushed(record.ClassifiedPush{SHA: "c5", Before: "c6", Forced: true, Sender: "dev"}), pushed(record.ClassifiedPush{SHA: again, Before: "c5", Sender: "dev"}),
			syncedAt("c5", again, "dev", 2), lateAt("c6", "c5", "dev", 1)}
	}
	got, control := delivered(t, start, history("c6")...), delivered(t, start, history("c6b")...)
	if got.FixAttempts != 2 || got.FixAttempts != control.FixAttempts || got.HeadCounted != "c6" || control.HeadCounted != "c6b" || got.Run != nil {
		t.Errorf("c6 pushed again: %s, a fresh commit c6b: %s, want both counted once more at their head", judged(got, "", ""), judged(control, "", ""))
	}
	if MovesBy(start, Arrival{Before: "c5", At: start.HeadUpdatedAt.Add(time.Minute)}, "c6") {
		t.Error("a synchronize to the recorded head that names no sender is a move, want none")
	}
	if MovesBy(start, Arrival{Before: "c5", Sender: "dev", At: start.HeadUpdatedAt}, "c6") {
		t.Error("a synchronize to the recorded head at the newest applied clock is a move, want none: it is that synchronize delivered again")
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

// A second copy of the synchronize that reached the recorded head - GitHub can send two for one
// push - is no move, whatever its clock: the record knows of no move away from the head to the head
// the copy names. h0>h1 applied, then a copy of h0>h1 a second later, counts no fix attempt and
// opens no head run; a return to h1 whose move away, h1..h0, was pushed is a move.
func TestACopyOfTheSynchronizeThatReachedTheHeadIsNoMove(t *testing.T) {
	code := record.ClassifiedPush{SHA: "h1", Before: "h0", Sender: "dev"}
	start := delivered(t, redAtH0(), pushed(code), secondAt("h0", "h1", "dev", 1, false), settles("h1", 2, "ci"))
	copied := delivered(t, start, secondAt("h0", "h1", "dev", 2, false))
	if copied.Run != nil || copied.FixAttempts != start.FixAttempts {
		t.Errorf("a copy of h0>h1 a second later: run open %v, fix %d; want closed and %d", copied.Run != nil, copied.FixAttempts, start.FixAttempts)
	}
	if MovesBy(start, Arrival{Before: "h0", Sender: "dev", At: time.Date(2026, 10, 1, 0, 0, 2, 0, time.UTC)}, "h1") {
		t.Error("a copy of h0>h1 with no move away known is a move, want none")
	}
	away := delivered(t, start, pushed(record.ClassifiedPush{SHA: "h0", Before: "h1", Forced: true, Sender: "dev"}))
	if !MovesBy(away, Arrival{Before: "h0", Sender: "dev", At: time.Date(2026, 10, 1, 0, 0, 3, 0, time.UTC)}, "h1") {
		t.Error("a return to h1 whose move away h1..h0 was pushed is no move, want one")
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
