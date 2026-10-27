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
// with clears of the head the required workflows were read at, a reopen at a head and a read of
// the required set; and half the histories reset the branch to a commit it held before, then push
// once more. Each push arrives at a random position in a random order, sometimes twice, and the
// record goes through the store's round trip after every fact. LEGION_RUN_FUZZ_ITERS sets the
// number of histories (the long mode), LEGION_RUN_FUZZ_SEED the seed.
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
		planned := rng.IntN(2) == 0
		red := record.PullRequest{HeadSHA: "h2", CheckedHead: "h2", Failing: []string{"ci"}, Required: []string{"ci"},
			CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, FixAttempts: 2, PlannedRed: planned}
		want := red
		for _, p := range pushes {
			want = stored(t, ApplyPush(want, p))
		}
		for _, f := range facts {
			want = stored(t, deliver(f, want))
		}
		order := append([]record.ClassifiedPush{}, pushes...)
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
		if want.Run != nil || got.Run != nil || JudgementChanged(want, got) {
			failures++
			t.Errorf("history %d, planned %v, pushes %+v\n order %v\n got  fix=%d counted=%q planned=%v checked=%q failing=%v run open=%v\n want fix=%d counted=%q planned=%v checked=%q failing=%v run open=%v",
				it, planned, pushes, seq, got.FixAttempts, got.HeadCounted, got.PlannedRed, got.CheckedHead, got.Failing, got.Run != nil,
				want.FixAttempts, want.HeadCounted, want.PlannedRed, want.CheckedHead, want.Failing, want.Run != nil)
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
		if rng.IntN(10) == 0 {
			facts = append(facts, runFact{cleared: true})
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
	if rng.IntN(3) == 0 {
		at, required := rng.IntN(len(facts)+1), []string{"ci"}
		if rng.IntN(2) == 0 {
			required = []string{}
		}
		facts = append(facts[:at], append([]runFact{{required: required}}, facts[at:]...)...)
	}
	return pushes, facts
}

// deliver applies f as the daemon does: a synchronize to the recorded head moves nothing
// (workflow's pullRequestSynchronized).
func deliver(f runFact, pr record.PullRequest) record.PullRequest {
	if f.head != "" && f.head == pr.HeadSHA {
		return pr
	}
	return f.apply(pr)
}

// A branch that returns to a commit it held before is judged the same in every order: the push
// that returns it, h4 to h3, is a different push from the one that first reached h3, and is not
// spent by h3's first arrival.
func TestABranchThatReturnsToACommitIsJudgedTheSameInEveryOrder(t *testing.T) {
	red := record.PullRequest{HeadSHA: "h2", CheckedHead: "h2", Failing: []string{"ci"}, Required: []string{"ci"},
		CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, FixAttempts: 2, PlannedRed: true}
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
		t.Fatalf("pushes first: fix=%d counted=%q planned=%v run open=%v; interleaved: fix=%d counted=%q planned=%v run open=%v",
			want.FixAttempts, want.HeadCounted, want.PlannedRed, want.Run != nil, got.FixAttempts, got.HeadCounted, got.PlannedRed, got.Run != nil)
	}
}
