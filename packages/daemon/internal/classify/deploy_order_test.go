package classify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
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
	"run-1":          "766fbd213a31cf9c05dc3144b1a0c4f5388803fa849b83a6a2c7233fdede0b1d",
	"run-7":          "ef6c5b97ecbfc20e021c1bc6fe243c6ed18c5af5ae8027643e39eb86bbe17a1f",
	"move-1-random":  "4335ce7e08f659833a709cda0ceb068f8e7138fba459901fe5d098636c88d4c7",
	"move-1-bounded": "c7b409783de5e9d58e651d0210051ccca0f39f281d6183a8418785fb432b46f4",
	"move-7-random":  "20cab3e755524b93e8355dcad36a68492897b186d9262fed851ac207daa35454",
	"move-7-bounded": "ea6c805dc3e6c01642afed7cc4dcad7dd5fddee3bca1f9f88c79c87740a5c83f",
}

// The same histories, every arrival naming its before but no sender, judge to these digests, made
// as noMoveNamedDigests were. The run fuzz's are the no-move ones - its synchronizes name no
// sender either way - and the move fuzz's are not: there the sender is what tells two pushes of
// the same two heads apart.
var noSenderNamedDigests = map[string]string{
	"run-1":          "766fbd213a31cf9c05dc3144b1a0c4f5388803fa849b83a6a2c7233fdede0b1d",
	"run-7":          "ef6c5b97ecbfc20e021c1bc6fe243c6ed18c5af5ae8027643e39eb86bbe17a1f",
	"move-1-random":  "89200283a3b20a127c13aae45628706f8daf13f666904aa81aeae61eaab52861",
	"move-1-bounded": "de66fdbccfd11787ee1e1a0a1a2a6e6bf300eaaeaa507ed0ff34d698bee32570",
	"move-7-random":  "881a012880d0f58147ab10e75a83ed01ae2e94e248011437f65510814fb354f0",
	"move-7-bounded": "59205c380ed8d18029751ed247a843272057f04f983bebb5374089917e44d1c2",
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
