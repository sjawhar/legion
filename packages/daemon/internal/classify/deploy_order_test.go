package classify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// This release judges these histories, every arrival naming no move, to these digests: the run's
// own fuzz (randomHistory, with pushes lost, delivered twice and in random order, settlements and
// reopens) and the fuzz of moves made again (movesMadeAgain, in the reference order and in one
// random or bounded order each). A digest is the SHA-256 of one line per history: its index,
// whether a head run is open, and its judgement (judgementOf) less the moves made.
// LEGION_PRINT_RELEASE_DIGESTS prints this tree's digests (printDigests), which is how these were
// made.
//
// They are not the digests of the release before, and cannot be: that release's judgement carried
// the fix-attempt accounting a CI result moved an issue by, which moves nothing now
// (dispatch://INFRA-714/spec R1), and its histories carried reads of what the base branch
// requires, which this release has no fact for. Every digest of both fuzzes moved again when each
// run of the fence gained the head GitHub reported it on and what it concluded
// (record.AttemptRun), which the judgement carries: both fuzzes settle heads, so every judgement
// with a check run in it reads differently, and a settlement that is newer for some names and
// older for others is applied per name now rather than dropped whole (ClassifySettlement), which
// moves what the fences of the run fuzz's several heads come to. What they fence is this release
// against itself: a daemon running ahead of its listener judges as recorded here, so a later
// change that moves that judgement is seen rather than deployed.
var noMoveNamedDigests = map[string]string{
	"run-1":          "95fe2681af7920ce2cc0b6a55a862e8628e2131d1c0d3427a5c19e3c9c2f897d",
	"run-7":          "b676a8896374d0f0509d9a9368d8fdfd596510e55f20c00a222f5c44ddda42fa",
	"move-1-random":  "ebecb476bf4a9bfc0c72bac4a0a161168b5dd0f86cee30908fcc96dccf6182e7",
	"move-1-bounded": "a10d761a320284fbe4ee7452e3e7b6eaf8dbb4adb27e8195eb2244853dedfba8",
	"move-7-random":  "5ccb7f082039de4bb57d5b7c5837b0b82955fd2209d14a68c189fe54b549c7dd",
	"move-7-bounded": "19fe08f73426fbd139a4e5b631cbf4d88dce76293f3d103e24d162922825c0b1",
}

// The same histories, every arrival naming its before but no sender, judge to these digests, made
// as noMoveNamedDigests were. The run fuzz's are the no-move ones - its synchronizes name no
// sender either way - and the move fuzz's are not: there the sender is what tells two pushes of
// the same two heads apart.
var noSenderNamedDigests = map[string]string{
	"run-1":          "95fe2681af7920ce2cc0b6a55a862e8628e2131d1c0d3427a5c19e3c9c2f897d",
	"run-7":          "b676a8896374d0f0509d9a9368d8fdfd596510e55f20c00a222f5c44ddda42fa",
	"move-1-random":  "ee393d5b499e2f98fa2cb0e0a899adf26d95f8a284154d31bceee50df3fb4cee",
	"move-1-bounded": "cbb68ad5774c6d7bed3810fe4699b5a17a6cc3ecbf106537203253ba68da75a9",
	"move-7-random":  "897f374c666b62e9de5c4c0485e1fe2d7ede8a771b5e0fcd8befde6ee9780473",
	"move-7-bounded": "0d7072b9597aa10822e492dec312c7099f472f71df46f38f4d838020fc8aeff1",
}

// A daemon of this release behind a listener that sends a synchronize's before but not its sender
// judges every history of both fuzzes as recorded: an arrival that names no sender takes no push
// by it and names no move late (NameMove), so the sender is what changes the judgement.
func TestAnArrivalThatNamesNoSenderIsJudgedAsRecorded(t *testing.T) {
	got := fuzzDigests(t, 2000, func(f runFact) runFact {
		f.sender = ""
		return f
	})
	printDigests(t, got)
	for kind, want := range noSenderNamedDigests {
		if got[kind] != want {
			t.Errorf("%s: histories with no sender named digest to %s, want the recorded %s", kind, got[kind], want)
		}
	}
}

// A daemon of this release behind a listener that does not yet send a synchronize's before - an
// arrival that names no move, everywhere - judges every history of both fuzzes as recorded, and
// leaves the same head runs open: the daemon may deploy before the listener. Its behaviour
// changes only once a listener that sends before is deployed.
func TestAnArrivalThatNamesNoMoveIsJudgedAsRecorded(t *testing.T) {
	got := noMoveDigests(t, 2000)
	printDigests(t, got)
	for kind, want := range noMoveNamedDigests {
		if got[kind] != want {
			t.Errorf("%s: histories with no move named digest to %s, want the recorded %s", kind, got[kind], want)
		}
	}
}

// printDigests prints digests under LEGION_PRINT_RELEASE_DIGESTS, which is how the maps above are
// made.
func printDigests(t *testing.T, digests map[string]string) {
	if os.Getenv("LEGION_PRINT_RELEASE_DIGESTS") == "" {
		return
	}
	for _, kind := range slices.Sorted(maps.Keys(digests)) {
		t.Logf("%q: %q,", kind, digests[kind])
	}
}

// noMoveDigests judges each fuzz's first iters histories with every arrival naming no move.
func noMoveDigests(t *testing.T, iters int) map[string]string {
	return fuzzDigests(t, iters, namingNoMove)
}

// fuzzDigests judges each fuzz's first iters histories with every fact as arrive makes it.
func fuzzDigests(t *testing.T, iters int, arrive func(runFact) runFact) map[string]string {
	digests := map[string]string{}
	judge := func(kind string, histories func(it int) (record.PullRequest, []runFact)) {
		h := sha256.New()
		for it := range iters {
			pr, facts := histories(it)
			for _, f := range facts {
				pr = stored(t, deliver(arrive(f), pr))
			}
			j := judgementOf(pr)
			j.Moves = nil
			encoded, err := json.Marshal(j)
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(h, "%d run=%v %s\n", it, pr.Run != nil, encoded)
		}
		digests[kind] = hex.EncodeToString(h.Sum(nil))
	}
	for _, seed := range []uint64{1, 7} {
		rng := rand.New(rand.NewPCG(seed, 7))
		judge(fmt.Sprintf("run-%d", seed), func(int) (record.PullRequest, []runFact) {
			pushes, facts := randomHistory(rng)
			if len(pushes) > 0 && rng.IntN(5) == 0 {
				drop := rng.IntN(len(pushes))
				pushes = append(pushes[:drop:drop], pushes[drop+1:]...)
			}
			red := record.PullRequest{HeadSHA: "h2", CheckedHead: "h2", Failing: []string{"ci"},
				CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1}
			order := append([]record.ClassifiedPush{}, pushes...)
			rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			if len(order) > 0 && rng.IntN(4) == 0 {
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
			return red, seq
		})
		for _, bounded := range []bool{false, true} {
			name := "random"
			if bounded {
				name = "bounded"
			}
			rng := rand.New(rand.NewPCG(seed, 3))
			var pending []runFact
			judge(fmt.Sprintf("move-%d-%s", seed, name), func(it int) (record.PullRequest, []runFact) {
				start := redAtH0()
				if it%2 == 0 {
					d := deliveries(rng, movesMadeAgain(rng), bounded)
					pending = d.order
					return start, d.reference
				}
				return start, pending
			})
		}
	}
	return digests
}

// namingNoMove is f as a listener that sends neither a synchronize's before nor its sender
// delivers it.
func namingNoMove(f runFact) runFact {
	f.before, f.sender = "", ""
	return f
}

// One change this release makes to the run with no move named, on purpose: a head run that a
// lost push opened closes at its bound (TestARunOneLostPushOpenedClosesAtItsBound), where the
// release before held it open for good once more heads than its bound on waiting pushes followed
// with no other steps between them. Neither fuzz above reaches that bound.
func TestARunALostPushOpenedClosesAtItsBoundWithNoMoveNamed(t *testing.T) {
	pr := delivered(t, redAtH0(), arrives("h1"))
	prev, closedAt := "h1", 0
	for i := 1; i <= 2*maxRunSteps && closedAt == 0; i++ {
		head := fmt.Sprintf("n%d", i)
		pr = delivered(t, pr, pushed(record.ClassifiedPush{SHA: head, Before: prev}), arrives(head))
		prev = head
		if pr.Run == nil {
			closedAt = i
		}
	}
	if closedAt != maxRunSteps {
		t.Errorf("the run a lost push opened closed after n%d (0: never), want n%d", closedAt, maxRunSteps)
	}
}

// With no move named, a push of two heads already recorded by a push that carries an approval
// across replaces it, as the release before replaced every earlier push of the same two heads: a
// handoff push h0..h1, then h0..h1 read as forced (a listener that predates the forced field, or a
// force push of the same commits), leaves no approval carrier behind, so an approval of h0 does not
// stand for h1 and h0's red is not carried to it once h1 arrives.
func TestAPushReplacesAnApprovalCarrierOfItsTwoHeads(t *testing.T) {
	carrier := record.ClassifiedPush{SHA: "h1", Before: "h0", HandoffOnly: true}
	forced := record.ClassifiedPush{SHA: "h1", Before: "h0", HandoffOnly: true, Forced: true}
	pr := delivered(t, redAtH0(), pushed(carrier), pushed(forced))
	if len(pr.Pushes) != 1 || pr.Pushes[0] != forced {
		t.Fatalf("pushes %+v, want the forced push alone", pr.Pushes)
	}
	pr = delivered(t, pr, arrives("h1"))
	if ApprovalStands(pr, "h0") || len(pr.Pushes) != 0 {
		t.Errorf("after h1: an approval of h0 stands %v, pushes %+v, want neither", ApprovalStands(pr, "h0"), pr.Pushes)
	}
	again := delivered(t, redAtH0(), pushed(forced), pushed(carrier))
	if len(again.Pushes) != 1 || again.Pushes[0] != carrier {
		t.Errorf("the carrier after the forced push: pushes %+v, want the carrier alone", again.Pushes)
	}
}

// The daemon rebuilds a lost push from the whole list GitHub's compare gives when it carries an
// approval across (daemon's carryApproval), and the push webhook of the same two heads can
// arrive after it with its list cut short, none listed, or none sent: that copy changes nothing.
// The handoff push h0..h1 the compare rebuilt stays a handoff push, so once h1 has arrived an
// approval of h0 still stands for it and h0's settlement is still carried to it. The other way
// round, a rebuilt push whose list was cut is replaced by the webhook's handoff push of the same
// two heads, as before.
func TestALatePushWithItsPathsCutShortKeepsTheHandoffPushTheCompareRebuilt(t *testing.T) {
	rebuilt := record.ClassifiedPush{SHA: "h1", Before: "h0", HandoffOnly: true, Sender: "implementer"}
	pr := delivered(t, redAtH0(), pushed(rebuilt), arrivesFrom("h0", "h1"))
	if !ApprovalStands(pr, "h0") || !Settled(pr) {
		t.Fatalf("after the rebuilt handoff push and h1: an approval of h0 stands %v, h0's settlement carried %v, want both", ApprovalStands(pr, "h0"), Settled(pr))
	}
	for _, unknown := range []string{"changed_paths truncated", "no commits listed", ClassifyPush(PushPayload{}, "").Unknown} {
		late := record.ClassifiedPush{SHA: "h1", Before: "h0", Unknown: unknown, Sender: "implementer"}
		got := delivered(t, pr, pushed(late))
		if !reflect.DeepEqual(got, pr) {
			t.Errorf("the webhook's copy (%s) changed the record:\n got %+v\nwant %+v", unknown, got, pr)
		}
		if !ApprovalStands(got, "h0") || !Settled(got) {
			t.Errorf("after the webhook's copy (%s): an approval of h0 stands %v, h0's settlement carried %v, want both", unknown, ApprovalStands(got, "h0"), Settled(got))
		}
	}
	cut := record.ClassifiedPush{SHA: "h1", Before: "h0", Unknown: "changed_paths truncated", Sender: "implementer"}
	again := delivered(t, redAtH0(), pushed(cut), arrivesFrom("h0", "h1"), pushed(rebuilt))
	if len(again.Pushes) != 1 || again.Pushes[0] != rebuilt || !ApprovalStands(again, "h0") || !Settled(again) {
		t.Errorf("the webhook's handoff push after the cut one: pushes %+v, an approval of h0 stands %v, h0's settlement carried %v, want the handoff push alone and both",
			again.Pushes, ApprovalStands(again, "h0"), Settled(again))
	}
}

// The bound on pushes waiting for a link counts their pairs of heads, each once, as the release
// before kept one push of each two heads: sixteen pairs waiting, each pushed twice, are all kept,
// and a seventeenth pair lets go every push of the pair whose newest push is oldest.
func TestTheBoundCountsEachTwoHeadsOnce(t *testing.T) {
	pr := redAtH0()
	for round := range 2 {
		for i := 1; i <= maxWaitingPushes; i++ {
			pr = delivered(t, pr, pushed(record.ClassifiedPush{SHA: fmt.Sprintf("w%d", i), Before: fmt.Sprintf("w%d", i-1), ByReviewApp: round == 1}))
		}
	}
	if len(pr.Pushes) != 2*maxWaitingPushes {
		t.Fatalf("%d pairs pushed twice keep %d pushes, want all %d", maxWaitingPushes, len(pr.Pushes), 2*maxWaitingPushes)
	}
	next := record.ClassifiedPush{SHA: fmt.Sprintf("w%d", maxWaitingPushes+1), Before: fmt.Sprintf("w%d", maxWaitingPushes)}
	got, dropped := ApplyPushDropping(pr, next)
	want := record.ClassifiedPush{SHA: "w1", Before: "w0", ByReviewApp: true}
	if len(dropped.Waiting) != 1 || dropped.Waiting[0] != want || len(got.Pushes) != 2*maxWaitingPushes-1 {
		t.Errorf("the seventeenth pair let go %+v and kept %d pushes, want w0..w1 named once (its newest push) and %d kept", dropped.Waiting, len(got.Pushes), 2*maxWaitingPushes-1)
	}
}
