package classify

import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// Random histories, delivered in random orders, give the judgement the pushes arriving first give,
// and leave no head run open. A history moves a red h2 along h3..h(2+n), each push a handoff, code,
// the review App's code or handoff, or a forced push; some heads are skipped (followHead past
// them); red and green settlements of the current or an earlier head arrive between the heads,
// with a reopen at a head; and half the histories reset the branch to a commit it held before,
// then push once more. Settlements arrive before the head they settle and again after it, and the
// listener redelivers some. A push webhook is sometimes lost for good, in both orders; the run it
// leaves open then stays open, its judgement the same. Each push arrives at a random position in a
// random order, sometimes twice, and the record goes through the store's round trip after every fact.
// LEGION_RUN_FUZZ_ITERS sets the number of histories (the long mode), LEGION_RUN_FUZZ_SEED the
// seed.
func TestEveryRandomOrderOfThePushesGivesThePushesFirstJudgement(t *testing.T) {
	iters, seed := 20000, uint64(1)
	if s := os.Getenv("LEGION_RUN_FUZZ_ITERS"); s != "" {
		iters, _ = strconv.Atoi(s)
	}
	if s := os.Getenv("LEGION_RUN_FUZZ_SEED"); s != "" {
		seed, _ = strconv.ParseUint(s, 10, 64)
	}
	rng := rand.New(rand.NewPCG(seed, 7))
	failures := 0
	for it := 0; it < iters && failures < 3; it++ {
		pushes, facts := randomHistory(rng)
		// A push webhook lost for good: neither order delivers it, and the run it leaves waiting
		// stays open in both, its judgement the same.
		lost := rng.IntN(5) == 0
		if lost {
			drop := rng.IntN(len(pushes))
			pushes = append(pushes[:drop:drop], pushes[drop+1:]...)
		}
		red := record.PullRequest{HeadSHA: "h2", CheckedHead: "h2", Failing: []string{"ci"},
			CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1}
		want := red
		for _, p := range pushes {
			want = stored(t, ApplyPush(want, p))
		}
		for _, f := range facts {
			want = stored(t, deliver(f, want))
		}
		order := append([]record.ClassifiedPush{}, pushes...)
		if len(order) == 0 {
			continue
		}
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		if rng.IntN(4) == 0 {
			order = append(order, order[rng.IntN(len(order))])
		}
		var seq []runFact
		for f, p := 0, 0; f < len(facts) || p < len(order); {
			if p < len(order) && (f == len(facts) || rng.IntN(2) == 0) {
				seq, p = append(seq, pushed(order[p])), p+1
			} else {
				seq, f = append(seq, facts[f]), f+1
			}
		}
		got := red
		for _, fact := range seq {
			got = stored(t, deliver(fact, got))
		}
		if (!lost && (want.Run != nil || got.Run != nil)) || JudgementChanged(want, got) {
			failures++
			t.Errorf("history %d, pushes %+v\n order %v\n got  checked=%q failing=%v run open=%v\n want checked=%q failing=%v run open=%v",
				it, pushes, seq, got.CheckedHead, got.Failing, got.Run != nil, want.CheckedHead, want.Failing, want.Run != nil)
		}
	}
}

// randomHistory is one history for the order fuzz: its pushes, and its other facts in the order
// they happen.
func randomHistory(rng *rand.Rand) ([]record.ClassifiedPush, []runFact) {
	n := 2 + rng.IntN(4)
	var pushes []record.ClassifiedPush
	for i := 3; i < 3+n; i++ {
		p := record.ClassifiedPush{SHA: fmt.Sprintf("h%d", i), Before: fmt.Sprintf("h%d", i-1)}
		switch rng.IntN(5) {
		case 0:
			p.HandoffOnly = true
		case 1:
			p.ByReviewApp = true
		case 2:
			p.HandoffOnly, p.Forced = rng.IntN(2) == 0, true
		case 3:
			p.HandoffOnly, p.ByReviewApp = true, rng.IntN(2) == 0
		}
		pushes = append(pushes, p)
	}
	var facts []runFact
	id := int64(2)
	settle := func(head string) {
		if rng.IntN(2) == 0 {
			facts = append(facts, settles(head, id, "ci"))
		} else {
			facts = append(facts, settles(head, id))
		}
		id++
	}
	for i := 3; i < 3+n; i++ {
		if i == 3+n-1 || rng.IntN(4) != 0 {
			facts = append(facts, arrives(fmt.Sprintf("h%d", i)))
		}
		for k := rng.IntN(3); k > 0; k-- {
			settle(fmt.Sprintf("h%d", 2+rng.IntN(i-1)))
		}
	}
	if rng.IntN(2) == 0 {
		last, back := fmt.Sprintf("h%d", 2+n), fmt.Sprintf("h%d", 2+rng.IntN(n))
		pushes = append(pushes, record.ClassifiedPush{SHA: back, Before: last, Forced: true, HandoffOnly: rng.IntN(2) == 0})
		facts = append(facts, arrives(back))
		if rng.IntN(2) == 0 {
			settle(back)
		}
		pushes = append(pushes, record.ClassifiedPush{SHA: "hx", Before: back, HandoffOnly: rng.IntN(2) == 0, ByReviewApp: rng.IntN(2) == 0})
		facts = append(facts, arrives("hx"))
		if rng.IntN(2) == 0 {
			settle("hx")
		}
	}
	if rng.IntN(3) == 0 {
		var heads []int
		for i, f := range facts {
			if f.head != "" {
				heads = append(heads, i)
			}
		}
		at := heads[rng.IntN(len(heads))]
		facts[at] = runFact{reopen: facts[at].head}
	}
	// A settlement of a head delivered before that head arrives, and redelivered after it.
	var heads []int
	for i, f := range facts {
		if f.head != "" {
			heads = append(heads, i)
		}
	}
	if len(heads) > 0 && rng.IntN(3) == 0 {
		at := heads[rng.IntN(len(heads))]
		early := settles(facts[at].head, id, "ci")
		if rng.IntN(2) == 0 {
			early = settles(facts[at].head, id)
		}
		id++
		before, after := rng.IntN(at+1), at+1+rng.IntN(len(facts)-at)
		facts = append(facts[:after], append([]runFact{early}, facts[after:]...)...)
		facts = append(facts[:before], append([]runFact{early}, facts[before:]...)...)
	}
	// The listener's redelivery of a settlement, later in the history.
	for k := rng.IntN(3); k > 0; k-- {
		var settled []int
		for i, f := range facts {
			if f.settlement != nil {
				settled = append(settled, i)
			}
		}
		if len(settled) == 0 {
			break
		}
		from := settled[rng.IntN(len(settled))]
		at := from + 1 + rng.IntN(len(facts)-from)
		facts = append(facts[:at], append([]runFact{facts[from]}, facts[at:]...)...)
	}
	return pushes, facts
}

// deliver applies f as the daemon does (workflow's pullRequestSynchronized): a synchronize is late,
// and only names its move while a head run is open, when GitHub's clock puts it before the newest
// applied (LateLifecycle, the workflow's rule) or it moves no head (MovesBy) - whatever the history
// that drew it marked.
func deliver(f runFact, pr record.PullRequest) record.PullRequest {
	if f.head != "" {
		f.late = LateLifecycle(f.at, pr.HeadUpdatedAt) || !MovesBy(pr, f.head)
	}
	return f.apply(pr)
}

// A branch that returns to a commit it held before is judged the same in every order: the push
// that returns it, h4 to h3, is a different push from the one that first reached h3, and is not
// spent by h3's first arrival.
func TestABranchThatReturnsToACommitIsJudgedTheSameInEveryOrder(t *testing.T) {
	red := record.PullRequest{HeadSHA: "h2", CheckedHead: "h2", Failing: []string{"ci"},
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1}
	reach := record.ClassifiedPush{SHA: "h3", Before: "h2", HandoffOnly: true, Forced: true}
	tests := record.ClassifiedPush{SHA: "h4", Before: "h3", ByReviewApp: true}
	reset := record.ClassifiedPush{SHA: "h3", Before: "h4", HandoffOnly: true, Forced: true}
	fix := record.ClassifiedPush{SHA: "hx", Before: "h3"}
	facts := []runFact{arrives("h3"), arrives("h4"), settles("h3", 2, "ci"), arrives("h3"), settles("h3", 3, "ci"), arrives("hx"), settles("hx", 4, "ci")}
	want := red
	for _, p := range []record.ClassifiedPush{reach, tests, reset, fix} {
		want = stored(t, ApplyPush(want, p))
	}
	for _, f := range facts {
		want = stored(t, deliver(f, want))
	}
	got := red
	for _, f := range []runFact{pushed(reset), arrives("h3"), arrives("h4"), pushed(fix), pushed(reach), pushed(tests), settles("h3", 2, "ci"),
		arrives("h3"), settles("h3", 3, "ci"), arrives("hx"), settles("hx", 4, "ci")} {
		got = stored(t, deliver(f, got))
	}
	if want.Run != nil || got.Run != nil || JudgementChanged(want, got) {
		t.Fatalf("pushes first: checked=%q failing=%v run open=%v; interleaved: checked=%q failing=%v run open=%v",
			want.CheckedHead, want.Failing, want.Run != nil, got.CheckedHead, got.Failing, got.Run != nil)
	}
}

// A settlement of h4 delivered before h4 arrives stands nowhere; its redelivery after h4 arrives
// stands for h4. The redelivery is a step of its own while the run is open, so h4 is red in both
// orders. A redelivery with no head between it and its copy takes no step.
func TestARedeliveredSettlementAfterItsHeadArrivesStandsInEveryOrder(t *testing.T) {
	red := record.PullRequest{HeadSHA: "h2", CheckedHead: "h2", Failing: []string{"ci"},
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1}
	p3, p4 := record.ClassifiedPush{SHA: "h3", Before: "h2"}, record.ClassifiedPush{SHA: "h4", Before: "h3"}
	h4red := settles("h4", 3, "ci")
	want := red
	for _, f := range []runFact{pushed(p3), pushed(p4), arrives("h3"), h4red, arrives("h4"), h4red} {
		want = stored(t, deliver(f, want))
	}
	got := red
	for _, f := range []runFact{arrives("h3"), h4red, arrives("h4"), h4red} {
		got = stored(t, deliver(f, got))
	}
	steps := len(got.Run.Steps)
	if again := stored(t, deliver(h4red, got)); len(again.Run.Steps) != steps {
		t.Fatalf("a redelivery with no head since its copy took a step: %d steps, want %d", len(again.Run.Steps), steps)
	}
	for _, p := range []record.ClassifiedPush{p3, p4} {
		got = stored(t, deliver(pushed(p), got))
	}
	if want.CheckedHead != "h4" || len(want.Failing) != 1 || got.Run != nil || JudgementChanged(want, got) {
		t.Fatalf("pushes first: checked=%q failing=%v; pushes last: checked=%q failing=%v run open=%v",
			want.CheckedHead, want.Failing, got.CheckedHead, got.Failing, got.Run != nil)
	}
}

// A reopen at the same head starts its settlement afresh, so a settlement redelivered after the
// reopen is a step of its own, as it is after a head's arrival: h3's settlement, a reopen at h3 and
// the listener's redelivery leave h3 red whether h3's push came first or last.
func TestASettlementRedeliveredAfterAReopenStandsInEveryOrder(t *testing.T) {
	red := record.PullRequest{HeadSHA: "h2", CheckedHead: "h2", Failing: []string{"ci"},
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1}
	p3, h3red, reopen := record.ClassifiedPush{SHA: "h3", Before: "h2"}, settles("h3", 3, "ci"), runFact{reopen: "h3"}
	want := red
	for _, f := range []runFact{pushed(p3), arrives("h3"), h3red, reopen, h3red} {
		want = stored(t, deliver(f, want))
	}
	got := red
	for _, f := range []runFact{arrives("h3"), h3red, reopen, h3red, pushed(p3)} {
		got = stored(t, deliver(f, got))
	}
	if want.CheckedHead != "h3" || len(want.Failing) != 1 || got.Run != nil || JudgementChanged(want, got) {
		t.Fatalf("pushes first: checked=%q failing=%v; pushes last: checked=%q failing=%v run open=%v",
			want.CheckedHead, want.Failing, got.CheckedHead, got.Failing, got.Run != nil)
	}
}

// A push redelivered while a head run is open, whose move was judged before the run opened, is
// spent at the run's anchor as it is spent when it arrives with no run open: h2..h3 (the review
// App's code) and h3..h4 bring the branch to h4; it returns to h3 by a push never delivered, which
// opens a run; then h2..h3 is delivered again. It left h3 long ago and settles nothing there.
func TestAPushRedeliveredIntoARunAfterItsMoveIsSpent(t *testing.T) {
	red := record.PullRequest{HeadSHA: "h2", CheckedHead: "h2", Failing: []string{"ci"},
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1}
	p3, p4 := record.ClassifiedPush{SHA: "h3", Before: "h2", ByReviewApp: true}, record.ClassifiedPush{SHA: "h4", Before: "h3"}
	want := red
	for _, f := range []runFact{pushed(p3), pushed(p4), arrives("h4"), arrives("h3")} {
		want = stored(t, deliver(f, want))
	}
	got := red
	for _, f := range []runFact{pushed(p4), pushed(p3), arrives("h4"), arrives("h3"), pushed(p3)} {
		got = stored(t, deliver(f, got))
	}
	if got.Run == nil || JudgementChanged(want, got) {
		t.Fatalf("delivered once: checked=%q failing=%v; redelivered into the run: checked=%q failing=%v run open=%v",
			want.CheckedHead, want.Failing, got.CheckedHead, got.Failing, got.Run != nil)
	}
}
