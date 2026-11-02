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
// before (main at ec0263277), with no move named, as that release named none. Every push of these
// histories is the implementer's or the review App's, so main at ec0263277, which also read
// handoff-only pushes, and this branch's tree before it took #2436 (b5d2a262) give the same digests.
var releaseBeforeDigests = map[string]string{
	"run-1":          "0fc445683dd6d4a02eebca6786b14f6826e9ad3123007d3b66edd10562a683a4",
	"run-7":          "94bb2ea3532aa8a2cf1652759a24adc18cf5d52748fe1b24335b85b8f3b19a1b",
	"move-1-random":  "b47c62228f9cf8e581fbfcc059efa619d854bea96b75c2031c0382b0c28cbc3c",
	"move-1-bounded": "f53b89c3c8c79a48bf24ba21e11c2265ffa7d6aa547a9ce7cbe1c6264f39c30e",
	"move-7-random":  "73fd22e557eb89404ca78a6f194ef3c3298cc46b75a3948670ef31423a96ed39",
	"move-7-bounded": "8657eddcb777a2f94b2ab1a19d3803dd838515ab5f352e8f5fd3f836b6988872",
}

// The release before this one judged these histories, every arrival naming its before but no
// sender, to these digests (made as releaseBeforeDigests were, at #2436's e5c72dfb3, design (c′)
// with the record keeping one push of each two heads as the release before did).
var beforeAloneDigests = map[string]string{
	"run-1":          "0fc445683dd6d4a02eebca6786b14f6826e9ad3123007d3b66edd10562a683a4",
	"run-7":          "94bb2ea3532aa8a2cf1652759a24adc18cf5d52748fe1b24335b85b8f3b19a1b",
	"move-1-random":  "1e0a8420eacc4a013af8fe4d2a1f7becec9f634db9abf971d1bbc8085287d4a3",
	"move-1-bounded": "c0a7f52443a1eff91077e9c0b7fe89f06f6b6336c56a20bd7cd825d1079e6c77",
	"move-7-random":  "6588b63ddfc379954ce3246fdcf317805b2f4405ad939ce44f55fa4340d1f930",
	"move-7-bounded": "6d5461d42272c832bfe60b7c7ea91ebc0d8066b68d94fd0cfaae0432d5ce6f85",
}

// A daemon of this release behind a listener that sends a synchronize's before but not its sender
// judges every history of both fuzzes as design (c′) did: an arrival that names no sender takes no
// push by it and names no move late (NameMove), so the sender is what changes the judgement.
func TestAnArrivalThatNamesNoSenderIsJudgedAsBeforeAlone(t *testing.T) {
	got := fuzzDigests(t, 2000, func(f runFact) runFact {
		f.sender = ""
		return f
	})
	for kind, want := range beforeAloneDigests {
		if got[kind] != want {
			t.Errorf("%s: histories with no sender named digest to %s, want design (c′)'s %s", kind, got[kind], want)
		}
	}
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

// namingNoMove is f as a listener that sends neither a synchronize's before nor its sender
// delivers it.
func namingNoMove(f runFact) runFact {
	f.before, f.sender = "", ""
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
