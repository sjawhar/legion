package classify

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// runFact is one fact the property test delivers: a head's arrival, a CI settlement, a read of
// what the base branch requires, the clearing of the head the required workflows were read at, a
// reopen at a head, or a push.
type runFact struct {
	head       string
	before     string
	sender     string
	at         time.Time
	late       bool // a synchronize that moves no head (NameMove): deliver decides it as the workflow does; a history may mark one it delivers late, for display
	made       int  // a synchronize's place among the moves of its history, 1 for the first; 0 when unknown
	settlement *SettlementCandidate
	required   []string
	cleared    bool
	reopen     string
	push       *record.ClassifiedPush
}

func (f runFact) String() string {
	switch {
	case f.head != "" && f.late:
		return fmt.Sprintf("late %s>%s by %s", f.before, f.head, f.sender)
	case f.head != "":
		return fmt.Sprintf("head %s>%s by %s", f.before, f.head, f.sender)
	case f.settlement != nil:
		return fmt.Sprintf("settle %s %v", f.settlement.Head, f.settlement.Failing)
	case f.required != nil:
		return fmt.Sprintf("require %v", f.required)
	case f.cleared:
		return "clear workflows head"
	case f.reopen != "":
		return "reopen " + f.reopen
	default:
		return "push " + f.push.Before + ".." + f.push.SHA
	}
}

func (f runFact) apply(pr record.PullRequest) record.PullRequest {
	switch {
	case f.head != "" && f.late:
		pr = NameMove(pr, record.Named{Before: f.before, SHA: f.head, Sender: f.sender, At: f.at})
		pr.HeadUpdatedAt = LatestClock(pr.HeadUpdatedAt, f.at)
		return pr
	case f.head != "":
		pr = AdvancePullRequestHead(pr, Arrival{Before: f.before, Sender: f.sender, At: f.at}, f.head)
		pr.HeadUpdatedAt = LatestClock(pr.HeadUpdatedAt, f.at)
		return pr
	case f.settlement != nil:
		pr, _, _ = ApplyCheckSettlement(pr, *f.settlement)
		return pr
	case f.required != nil:
		return ApplyRequiredRead(pr, f.required, nil, "")
	case f.cleared:
		return ClearWorkflowsHead(pr)
	case f.reopen != "":
		return Reopen(pr, f.reopen)
	default:
		return ApplyPush(pr, *f.push)
	}
}

// stored is pr as the store reads it back (record.PutPullRequest, then scanPullRequest): its
// failing and cancelled checks and its pushes as lists, never nil; its required set and its check
// runs as they were, nil apart from empty; its moves and its head run through JSON.
func stored(t *testing.T, pr record.PullRequest) record.PullRequest {
	t.Helper()
	pr.Failing, pr.Cancelled = append([]string{}, pr.Failing...), append([]string{}, pr.Cancelled...)
	pr.Pushes, pr.Workflows = append([]record.ClassifiedPush{}, pr.Pushes...), append([]record.RequiredWorkflow{}, pr.Workflows...)
	moves, err := json.Marshal(append([]record.Move{}, pr.Moves...))
	if err != nil {
		t.Fatal(err)
	}
	pr.Moves = nil
	if err := json.Unmarshal(moves, &pr.Moves); err != nil {
		t.Fatal(err)
	}
	if pr.Run != nil {
		encoded, err := json.Marshal(pr.Run)
		if err != nil {
			t.Fatal(err)
		}
		pr.Run = nil
		if err := json.Unmarshal(encoded, &pr.Run); err != nil {
			t.Fatal(err)
		}
	}
	return pr
}

func settles(head string, id int64, failing ...string) runFact {
	return runFact{settlement: &SettlementCandidate{Head: head, CheckRuns: []record.AttemptRun{{Name: "ci", ID: id}}, Generation: id,
		Snapshot: fmt.Sprintf("%s-%d", head, id), Failing: append([]string{}, failing...)}}
}

func arrives(head string) runFact { return runFact{head: head} }

// arrivesFrom is a synchronize that names the head GitHub moved from.
func arrivesFrom(before, head string) runFact { return runFact{head: head, before: before} }

func pushed(p record.ClassifiedPush) runFact { return runFact{push: &p} }

// interleavings is every sequence that keeps facts in order and places each order of pushes at
// every position among them.
func interleavings(facts []runFact, pushes []record.ClassifiedPush) [][]runFact {
	var all [][]runFact
	for _, order := range pushOrders(pushes) {
		var place func(done []runFact, f, p int)
		place = func(done []runFact, f, p int) {
			if f == len(facts) && p == len(order) {
				all = append(all, append([]runFact{}, done...))
				return
			}
			if f < len(facts) {
				place(append(done, facts[f]), f+1, p)
			}
			if p < len(order) {
				place(append(done, pushed(order[p])), f, p+1)
			}
		}
		place(nil, 0, 0)
	}
	return all
}

// The count, its mark, the planned mark, the settlement and the required set are the same however
// a pull request's pushes interleave with its other facts: every order of the pushes at every
// position gives what the pushes arriving first give, and closes the head run. The shapes move a
// red h2 past h3 to h4 - the order a second head arriving before the first head's pushes broke
// before the head run - by the review App's red tests, handoffs, the implementer's code and forced
// pushes, with settlements of h3 and h4 red and green, a new required set, the clearing of the
// head the workflows were read at and a reopen between them. The record goes through the store's
// round trip between facts, as the daemon reads it back.
func TestEveryOrderOfThePushesGivesTheJudgementOfThePushesFirst(t *testing.T) {
	shapes := []struct {
		name   string
		pushes []record.ClassifiedPush
	}{
		{"red tests, then a handoff", []record.ClassifiedPush{{SHA: "h3", Before: "h2", ByReviewApp: true}, {SHA: "h4", Before: "h3", HandoffOnly: true}}},
		{"two handoffs", []record.ClassifiedPush{{SHA: "h3", Before: "h2", HandoffOnly: true}, {SHA: "h4", Before: "h3", HandoffOnly: true}}},
		{"a handoff, then code", []record.ClassifiedPush{{SHA: "h3", Before: "h2", HandoffOnly: true}, {SHA: "h4", Before: "h3"}}},
		{"code, then a handoff", []record.ClassifiedPush{{SHA: "h3", Before: "h2"}, {SHA: "h4", Before: "h3", HandoffOnly: true}}},
		{"code, then code", []record.ClassifiedPush{{SHA: "h3", Before: "h2"}, {SHA: "h4", Before: "h3"}}},
		{"red tests, then code", []record.ClassifiedPush{{SHA: "h3", Before: "h2", ByReviewApp: true}, {SHA: "h4", Before: "h3"}}},
		{"a forced handoff, then a handoff", []record.ClassifiedPush{{SHA: "h3", Before: "h2", HandoffOnly: true, Forced: true}, {SHA: "h4", Before: "h3", HandoffOnly: true}}},
		{"red tests past an unrecorded h3", []record.ClassifiedPush{{SHA: "h3", Before: "h2", ByReviewApp: true}, {SHA: "h4", Before: "h3", HandoffOnly: true}}},
	}
	sequences := []struct {
		name  string
		facts []runFact
	}{
		{"h3, h4", []runFact{arrives("h3"), arrives("h4")}},
		{"h3 red, h4", []runFact{arrives("h3"), settles("h3", 2, "ci"), arrives("h4")}},
		{"h3 green, h4", []runFact{arrives("h3"), settles("h3", 2), arrives("h4")}},
		{"h3, h4 red", []runFact{arrives("h3"), arrives("h4"), settles("h4", 3, "ci")}},
		{"h3 red, h4 red", []runFact{arrives("h3"), settles("h3", 2, "ci"), arrives("h4"), settles("h4", 3, "ci")}},
		{"h3, lint required, h4 red", []runFact{arrives("h3"), {required: []string{"ci", "lint"}}, arrives("h4"), settles("h4", 3, "ci")}},
		{"h3 red, workflows cleared, h4", []runFact{arrives("h3"), settles("h3", 2, "ci"), {cleared: true}, arrives("h4")}},
		{"h3, reopened at h4, h4 red", []runFact{arrives("h3"), {reopen: "h4"}, settles("h4", 3, "ci")}},
		{"h4 past h3, h4 red", []runFact{arrives("h4"), settles("h4", 3, "ci")}},
	}
	for _, shape := range shapes {
		for _, sequence := range sequences {
			if strings.Contains(shape.name, "unrecorded") != strings.HasPrefix(sequence.name, "h4 past") {
				continue
			}
			for _, planned := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s; %s; planned %v", shape.name, sequence.name, planned), func(t *testing.T) {
					red := record.PullRequest{HeadSHA: "h2", CheckedHead: "h2", Failing: []string{"ci"}, Required: []string{"ci"},
						CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, FixAttempts: 2, PlannedRed: planned}
					want := red
					for _, p := range shape.pushes {
						want = stored(t, ApplyPush(want, p))
					}
					for _, fact := range sequence.facts {
						want = stored(t, fact.apply(want))
					}
					for _, order := range interleavings(sequence.facts, shape.pushes) {
						got := red
						for _, fact := range order {
							got = stored(t, fact.apply(got))
						}
						if got.Run != nil {
							t.Errorf("%v: the head run is open after every push arrived: %+v", order, got.Run)
						}
						if JudgementChanged(want, got) {
							t.Errorf("%v: fixAttempts=%d headCounted=%q plannedRed=%v verdict=%q checkedHead=%q required=%v; pushes first: %d, %q, %v, %q, %q, %v",
								order, got.FixAttempts, got.HeadCounted, got.PlannedRed, HeadVerdict(got), got.CheckedHead, got.Required,
								want.FixAttempts, want.HeadCounted, want.PlannedRed, HeadVerdict(want), want.CheckedHead, want.Required)
						}
					}
				})
			}
		}
	}
}

// A push for a head that has not arrived changes no judgement, also while a head run holding an
// applied settlement is open and the record has been through the store, where an empty list of
// cancelled checks reads back as one: the push handler then does not act on the red that stands
// (workflow's push).
func TestAPushForAHeadNotYetArrivedChangesNoJudgement(t *testing.T) {
	red := record.PullRequest{HeadSHA: "h2", CheckedHead: "h2", Failing: []string{"ci"}, Required: []string{"ci"},
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, FixAttempts: 2}
	prior := stored(t, AdvancePullRequestHead(red, Arrival{}, "h3"))
	prior = stored(t, settles("h3", 2, "ci").apply(prior))
	if prior.Run == nil || HeadVerdict(prior) != "red" {
		t.Fatalf("h3 red before its push = %+v, want its run open and red", prior)
	}
	// The handler compares the record it read with the one the push leaves, before it is stored.
	after := ApplyPush(prior, record.ClassifiedPush{SHA: "h4", Before: "h3"})
	if JudgementChanged(prior, after) {
		t.Fatalf("a push for h4, not arrived, changed the judgement: before %+v, after %+v", judgementOf(prior), judgementOf(after))
	}
}

// tenChecks is the check runs of one settlement on a head with ten required checks, the size of a
// real settlement step.
func tenChecks(head int) []record.AttemptRun {
	runs := make([]record.AttemptRun, 10)
	for i := range runs {
		runs[i] = record.AttemptRun{Name: fmt.Sprintf("check-%d", i), ID: int64(head*10 + i)}
	}
	return runs
}

// sha is the i'th commit, at a real commit SHA's 40 hexadecimal characters, so the sizes below are
// those a stored run reaches.
func sha(i int) string { return fmt.Sprintf("%040x", i) }

// stampHead puts an arrival time on pr's run's last head step, as the workflow does (stampRun).
func stampHead(pr record.PullRequest, at time.Time) record.PullRequest {
	if pr.Run == nil {
		return pr
	}
	run := *pr.Run
	run.Steps = append([]record.RunStep{}, run.Steps...)
	for i := len(run.Steps) - 1; i >= 0; i-- {
		if run.Steps[i].Head != "" {
			if run.Steps[i].At.IsZero() {
				run.Steps[i].At = at
			}
			break
		}
	}
	pr.Run = &run
	return pr
}

// A pull request whose heads each arrive before their pushes keeps a bounded head run and record:
// the run's front folds into its anchor as each push arrives, so the stored run stays as long as
// the pushes lag, here three heads. A push lost for good keeps the run open at its head; the run is
// cut to its newest maxRunSteps steps and stays that size, however many heads follow. Every second
// push is handoff-only, which the record keeps for good (it carries an approval across) and the
// run does not copy; each settlement holds ten check runs and is delivered twice, and the second
// delivery takes no step. Heads are 40-character SHAs and each head step carries its arrival time.
// Whether its push comes before or after its head, each head records its move once, so the record
// remembers the last maxMoves distinct moves.
func TestAHeadRunStaysBounded(t *testing.T) {
	encoded := func(v any) int {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return len(b)
	}
	for _, tc := range []struct {
		name                string
		lost                int
		pushFirst           bool
		maxSteps, maxPushes int
	}{
		{"every push three heads late", -1, false, 8, 4},
		{"the fifth push lost", 5, false, maxRunSteps, maxRunSteps},
		{"every push before its head", -1, true, 8, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := record.PullRequest{HeadSHA: sha(0), CheckedHead: sha(0), Failing: []string{"check-0"}, Required: []string{"check-0"}, CheckRuns: tenChecks(0)}
			largestRun, largestMoves, largestRow := 0, 0, 0
			start := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
			for i := 1; i <= 200; i++ {
				if tc.pushFirst {
					pr = ApplyPush(pr, record.ClassifiedPush{SHA: sha(i), Before: sha(i - 1), HandoffOnly: i%2 == 0})
				}
				pr = stampHead(AdvancePullRequestHead(pr, Arrival{}, sha(i)), start.Add(time.Duration(i)*time.Minute))
				settlement := SettlementCandidate{Head: sha(i), CheckRuns: tenChecks(i), Generation: int64(i + 1),
					Snapshot: fmt.Sprintf("s%d", i), Failing: []string{"check-0"}}
				pr, _, _ = ApplyCheckSettlement(pr, settlement)
				steps := 0
				if pr.Run != nil {
					steps = len(pr.Run.Steps)
				}
				if pr, _, _ = ApplyCheckSettlement(pr, settlement); pr.Run != nil && len(pr.Run.Steps) != steps {
					t.Fatalf("after %d heads the redelivered settlement took a step: %d steps, want %d", i, len(pr.Run.Steps), steps)
				}
				if late := i - 3; !tc.pushFirst && late >= 1 && late != tc.lost {
					pr = ApplyPush(pr, record.ClassifiedPush{SHA: sha(late), Before: sha(late - 1), HandoffOnly: late%2 == 0})
				}
				if pr.Run != nil {
					largestRun = max(largestRun, encoded(pr.Run))
					if len(pr.Run.Steps) > tc.maxSteps || len(pr.Run.Pushes) > tc.maxPushes {
						t.Fatalf("after %d heads the head run holds %d steps and %d pushes, want at most %d and %d",
							i, len(pr.Run.Steps), len(pr.Run.Pushes), tc.maxSteps, tc.maxPushes)
					}
				}
				distinct := map[[2]string]bool{}
				for _, m := range pr.Moves {
					distinct[[2]string{m.Before, m.SHA}] = true
				}
				if len(pr.Moves) > maxMoves || len(distinct) != len(pr.Moves) || len(distinct) != min(i, maxMoves) {
					t.Fatalf("after %d heads the record holds %d moves, %d distinct, want %d, each once", i, len(pr.Moves), len(distinct), min(i, maxMoves))
				}
				largestMoves = max(largestMoves, encoded(pr.Moves))
				largestRow = max(largestRow, encoded(pr.Run)+encoded(pr.Moves)+encoded(pr.Pushes))
			}
			t.Logf("largest stored head run: %d bytes; largest stored moves: %d bytes; largest run, moves and pushes together: %d bytes; pushes kept at the end: %d (%d bytes)",
				largestRun, largestMoves, largestRow, len(pr.Pushes), encoded(pr.Pushes))
		})
	}
}

// A head run that one lost push opened closes at its bound, however many heads follow, each pushed
// before it arrives: h1 arrives with no push of its move - lost for good, or the push of a move made
// again never delivered - and n1, n2, ... follow, each push on its way before its head. h1 waits
// for good, so the run holds it and every head after it until the step past maxRunSteps, which cuts
// h1 off; every head after it was judged with its chain, so the run closes there, and stays closed.
// The run lets a push go only when the record's own bound does (ApplyPush), which counts from the
// record's head and so keeps n1, n2, ...: a bound of the run's own, counting from its anchor, would
// let n1..n16 go and leave those heads judged unchained, the run open for good at its bound. With a
// settlement after each head, each head is two steps, so the bound cuts h1 off half as many heads
// in.
func TestARunOneLostPushOpenedClosesAtItsBound(t *testing.T) {
	red := record.PullRequest{HeadSHA: "h0", CheckedHead: "h0", Failing: []string{"ci"}, Required: []string{"ci"},
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, FixAttempts: 1}
	first, back := record.ClassifiedPush{SHA: "h1", Before: "h0"}, record.ClassifiedPush{SHA: "h0", Before: "h1", Forced: true}
	for _, tc := range []struct {
		name  string
		facts []runFact
	}{
		{"h1's push lost", []runFact{arrivesFrom("h0", "h1")}},
		{"the push of h1 made again lost", []runFact{pushed(first), arrivesFrom("h0", "h1"), pushed(back), arrivesFrom("h1", "h0"), arrivesFrom("h0", "h1")}},
	} {
		for _, settled := range []bool{false, true} {
			pr := red
			for _, f := range tc.facts {
				pr = stored(t, deliver(f, pr))
			}
			if pr.Run == nil {
				t.Fatalf("%s: no head run waits for h1", tc.name)
			}
			// h1 is the run's first step and n_i its step i+1 (2i and 2i+1 when settled), so the
			// head whose arrival is the step past the bound closes the run.
			want := maxRunSteps
			if settled {
				want = maxRunSteps / 2
			}
			prev, closedAt, reopened := "h1", 0, 0
			for i := 1; i <= 2*maxRunSteps; i++ {
				head := fmt.Sprintf("n%d", i)
				pr = stored(t, deliver(pushed(record.ClassifiedPush{SHA: head, Before: prev}), pr))
				pr = stored(t, deliver(arrivesFrom(prev, head), pr))
				if settled {
					pr = stored(t, deliver(settles(head, int64(i+1), "ci"), pr))
				}
				prev = head
				switch {
				case pr.Run == nil && closedAt == 0:
					closedAt = i
				case pr.Run != nil && closedAt != 0 && reopened == 0:
					reopened = i
				}
			}
			if closedAt != want || reopened != 0 {
				got := fmt.Sprintf("never closed in %d heads", 2*maxRunSteps)
				if closedAt != 0 {
					got = fmt.Sprintf("closed after n%d", closedAt)
				}
				if reopened != 0 {
					got += fmt.Sprintf(", opened again after n%d", reopened)
				}
				if pr.Run != nil {
					got += fmt.Sprintf(", %d steps open at the end", len(pr.Run.Steps))
				}
				t.Errorf("%s, settled %v: the run %s, want it closed after n%d and never again", tc.name, settled, got, want)
			}
		}
	}
}

// One head records one move, its push delivered before the head or after it, and the move records
// its push as spent either way.
func TestOneHeadRecordsItsMoveOnce(t *testing.T) {
	want := []record.Move{{Before: "h0", SHA: "h1", Pushed: true}}
	push := record.ClassifiedPush{SHA: "h1", Before: "h0"}
	fresh := func() record.PullRequest {
		return record.PullRequest{HeadSHA: "h0", CheckedHead: "h0", Failing: []string{}, Required: []string{"ci"}}
	}
	first := AdvancePullRequestHead(ApplyPush(fresh(), push), Arrival{}, "h1")
	if !reflect.DeepEqual(first.Moves, want) {
		t.Errorf("the push before its head left moves %+v, want %+v", first.Moves, want)
	}
	late := ApplyPush(AdvancePullRequestHead(fresh(), Arrival{}, "h1"), push)
	if !reflect.DeepEqual(late.Moves, want) {
		t.Errorf("the push after its head left moves %+v, want %+v", late.Moves, want)
	}
}

// The same two heads pushed again, once the branch moved between them with its push recorded, give
// the move made again its push: h0..h1, a forced return h1..h0, then h0..h1 pushed again, a red
// settling at each head. Whichever order GitHub delivers the pushes in, the second arrival of h1 is
// judged with its chain (arrivalPushes), so no head run stays open and the judgement is the one the
// pushes arriving first give. A push GitHub never delivers leaves the run open, as any lost push
// does. Until h1 arrives again the push made again looks like the first push delivered twice, so
// it is no code on its way from h0; and a push delivered late for a move the record made before its
// push was recorded is still that move's: the first h0..h1, delivered after the branch returned to
// h0, is spent as it arrives.
func TestAMoveMadeAgainTakesItsPushWhicheverArrivesFirst(t *testing.T) {
	red := record.PullRequest{HeadSHA: "h0", CheckedHead: "h0", Failing: []string{"ci"}, Required: []string{"ci"},
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, FixAttempts: 1}
	first, back, again := record.ClassifiedPush{SHA: "h1", Before: "h0"}, record.ClassifiedPush{SHA: "h0", Before: "h1", Forced: true}, record.ClassifiedPush{SHA: "h1", Before: "h0"}
	s1, s2, s3 := settles("h1", 2, "ci"), settles("h0", 3, "ci"), settles("h1", 4, "ci")
	deliverAll := func(facts ...runFact) record.PullRequest {
		pr := red
		for _, f := range facts {
			pr = stored(t, deliver(f, pr))
		}
		return pr
	}
	want := deliverAll(pushed(first), arrivesFrom("h0", "h1"), s1, pushed(back), arrivesFrom("h1", "h0"), s2, pushed(again), arrivesFrom("h0", "h1"), s3)
	if want.Run != nil || want.FixAttempts != 4 || want.HeadCounted != "h1" || HeadVerdict(want) != "red" {
		t.Errorf("pushes first: run open %v, fix=%d counted=%q verdict=%q, want no run, 4, \"h1\" and red", want.Run != nil, want.FixAttempts, want.HeadCounted, HeadVerdict(want))
	}
	for _, tc := range []struct {
		name  string
		facts []runFact
	}{
		{"every push late", []runFact{arrivesFrom("h0", "h1"), s1, pushed(first), arrivesFrom("h1", "h0"), s2, pushed(back), arrivesFrom("h0", "h1"), s3, pushed(again)}},
		{"the push made again late", []runFact{pushed(first), arrivesFrom("h0", "h1"), s1, pushed(back), arrivesFrom("h1", "h0"), s2, arrivesFrom("h0", "h1"), s3, pushed(again)}},
		{"the first push late, after the branch returned", []runFact{arrivesFrom("h0", "h1"), s1, pushed(back), arrivesFrom("h1", "h0"), s2, pushed(first), pushed(again), arrivesFrom("h0", "h1"), s3}},
	} {
		if got := deliverAll(tc.facts...); got.Run != nil || JudgementChanged(want, got) {
			t.Errorf("%s: run open %v, fix=%d counted=%q verdict=%q, want no run and the pushes-first fix=%d counted=%q verdict=%q",
				tc.name, got.Run != nil, got.FixAttempts, got.HeadCounted, HeadVerdict(got), want.FixAttempts, want.HeadCounted, HeadVerdict(want))
		}
	}
	if lost := deliverAll(pushed(first), arrivesFrom("h0", "h1"), s1, pushed(back), arrivesFrom("h1", "h0"), s2, arrivesFrom("h0", "h1"), s3); lost.Run == nil {
		t.Error("the push made again never delivered: the run closed, want it open waiting for that push")
	}
	if twice := deliverAll(pushed(first), arrives("h1"), s1, pushed(back), arrives("h0"), s2, pushed(first)); twice.Run != nil || CodeOnItsWay(twice) || len(twice.Pushes) != 0 {
		t.Errorf("the first push delivered twice, after the branch returned to h0: run open %v, code on its way %v, pushes %+v, want it spent by its move",
			twice.Run != nil, CodeOnItsWay(twice), twice.Pushes)
	}
	late := deliverAll(arrives("h1"), s1, pushed(back), arrives("h0"), s2, pushed(first))
	if late.Run != nil || CodeOnItsWay(late) || len(late.Pushes) != 0 {
		t.Errorf("the first push delivered after the branch returned to h0: run open %v, code on its way %v, pushes %+v, want it spent by its move",
			late.Run != nil, CodeOnItsWay(late), late.Pushes)
	}
}

// BenchmarkReplayAtTheBound replays a head run of maxRunSteps steps - heads and settlements of ten
// check runs alternating, every head waiting for its push - which is the most a push replays.
func BenchmarkReplayAtTheBound(b *testing.B) {
	pr := record.PullRequest{HeadSHA: sha(0), CheckedHead: sha(0), Failing: []string{"check-0"}, Required: []string{"check-0"}, CheckRuns: tenChecks(0)}
	start := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	for i := 1; pr.Run == nil || len(pr.Run.Steps) < maxRunSteps; i++ {
		pr = stampHead(AdvancePullRequestHead(pr, Arrival{}, sha(i)), start.Add(time.Duration(i)*time.Minute))
		pr, _, _ = ApplyCheckSettlement(pr, SettlementCandidate{Head: sha(i), CheckRuns: tenChecks(i),
			Generation: int64(i + 1), Snapshot: fmt.Sprintf("s%d", i), Failing: []string{"check-0"}})
	}
	if len(pr.Run.Steps) != maxRunSteps {
		b.Fatalf("the run holds %d steps, want %d", len(pr.Run.Steps), maxRunSteps)
	}
	b.ResetTimer()
	for b.Loop() {
		replayRun(pr)
	}
}
