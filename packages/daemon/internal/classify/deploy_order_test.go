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

// The release before this one (#2379) judged these histories, every arrival naming no move, to
// these digests: the run's own fuzz (randomHistory, with pushes lost, delivered twice and in random
// order, settlements, reopens and required reads) and the fuzz of moves made again (movesMadeAgain,
// in the reference order and in one random or bounded order each). A digest is the SHA-256 of one
// line per history: its index, whether a head run is open, and its judgement (judgementOf) less
// the moves made, whose record this release extends. LEGION_PRINT_RELEASE_DIGESTS prints this
// tree's digests, which is how these were made: by this test, its helpers copied to the release
// before (main at ec0263277), with no move named, as that release named none.
var releaseBeforeDigests = map[string]string{
	"run-1":          "10d09bc5b7092579c106aa940b30dd959cdbde2d9ecd83e76f6707d01f8ee5d6",
	"run-7":          "f670e5f0717d84a8ccd925a1eac0f308120ee2afe2725d078f406eeec3579d2b",
	"move-1-random":  "7609f3f4a48203e0535b2ff920378f58a332542b473f43d1960c07741e4a488e",
	"move-1-bounded": "ad0c79f885cb28ecd1996193a17a4cfdc1c936c2f8360bd8aa62029fc03ae3ec",
	"move-7-random":  "6fc60031e73feb2135b3d0085c1436c443c0668f4db6d87344bedcb5227b370d",
	"move-7-bounded": "199af17d66f671cff86fc45007d6ef8f26de588892df0e865c2d16fb4a6063ed",
}

// A daemon of this release behind a listener that does not yet send a synchronize's before - an
// arrival that names no move, everywhere - judges every history of both fuzzes exactly as the
// release before did, and leaves the same head runs open: the daemon may deploy before the
// listener. Its behaviour changes only once a listener that sends before is deployed.
func TestAnArrivalThatNamesNoMoveIsJudgedAsTheReleaseBefore(t *testing.T) {
	got := noMoveDigests(t, 2000)
	if os.Getenv("LEGION_PRINT_RELEASE_DIGESTS") != "" {
		for _, kind := range slices.Sorted(maps.Keys(got)) {
			t.Logf("%q: %q,", kind, got[kind])
		}
	}
	for kind, want := range releaseBeforeDigests {
		if got[kind] != want {
			t.Errorf("%s: histories with no move named digest to %s, want the release before's %s", kind, got[kind], want)
		}
	}
}

// noMoveDigests judges each fuzz's first iters histories with every arrival naming no move.
func noMoveDigests(t *testing.T, iters int) map[string]string {
	digests := map[string]string{}
	judge := func(kind string, histories func(it int) (record.PullRequest, []runFact)) {
		h := sha256.New()
		for it := range iters {
			pr, facts := histories(it)
			for _, f := range facts {
				pr = stored(t, deliver(namingNoMove(f), pr))
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
			red := record.PullRequest{HeadSHA: "h2", CheckedHead: "h2", Failing: []string{"ci"}, Required: []string{"ci"},
				CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, FixAttempts: 2, PlannedRed: rng.IntN(2) == 0}
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
					start.PlannedRed = rng.IntN(2) == 0
					pending = d.order
					return start, d.reference
				}
				return start, pending
			})
		}
	}
	return digests
}

// namingNoMove is f as a listener that does not send a synchronize's before delivers it.
func namingNoMove(f runFact) runFact {
	f.before = ""
	return f
}

// The one change a daemon of this release makes with no move named, on purpose: a head run that a
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
