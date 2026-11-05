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
//
// Contract 48 took `fixAttempts` and `headCounted` out of the judgement (dispatch://INFRA-956 R4),
// which changes the bytes every digest is taken over, so these were re-made from this tree. What
// they hold is what they always held less those two keys: a history's judgement does not depend on
// the order its facts arrive in, and an arrival naming no move is judged as one naming its before.
var releaseBeforeDigests = map[string]string{
	"run-1":          "b35092795466e37b4edaff40662752cb14963207422aa524793cf94d994683f5",
	"run-7":          "5be0c529f78c85369357c222dd2955a50e7db121620e463951b1ec560ac14435",
	"move-1-random":  "c8b66accd9ea0878b0e6fe2e040d248383f815a90a3c91765a0f769c68e900ce",
	"move-1-bounded": "6a9c972c916c1d47fc7afddf47043bfa47ca5b99949d98b664d3e74cf52ebdb3",
	"move-7-random":  "32f53be12b5fec4e7b03a808af2ad37c11c1240f6534e6170b785baad805e850",
	"move-7-bounded": "f0bac72bee10cbd86ceed9ed9a4f8c48c889bf30841b6f3b49fe85c81dcef577",
}

// The release before this one judged these histories, every arrival naming its before but no
// sender, to these digests (made as releaseBeforeDigests were, at #2436's e5c72dfb3, design (c′)
// with the record keeping one push of each two heads as the release before did), and they were
// re-made from this tree when contract 48 removed two keys from the judgement, as above.
var beforeAloneDigests = map[string]string{
	"run-1":          "b35092795466e37b4edaff40662752cb14963207422aa524793cf94d994683f5",
	"run-7":          "5be0c529f78c85369357c222dd2955a50e7db121620e463951b1ec560ac14435",
	"move-1-random":  "58cd396a98953164503cbca6cb3e209526b09646774463b97f310a63b473843f",
	"move-1-bounded": "b2c00ae2f8e145959add2b7bfdf809a483838d36eeaf831775d1c51c7cfb47e1",
	"move-7-random":  "7dde35c1d3a84a392449bf2e0d2d6667ec21cc57e8651d36a53cb07bb4917eb5",
	"move-7-bounded": "d7a11a010fbeb0e7b6f5bcb0bb169e71e9e7a28244cf4c6543d2e853ff5bc435",
}

// A daemon of this release behind a listener that sends a synchronize's before but not its sender
// judges every history of both fuzzes as design (c′) did: an arrival that names no sender takes no
// push by it and names no move late (NameMove), so the sender is what changes the judgement.
func TestAnArrivalThatNamesNoSenderIsJudgedAsBeforeAlone(t *testing.T) {
	got := fuzzDigests(t, 2000, func(f runFact) runFact {
		f.sender = ""
		return f
	})
	if os.Getenv("LEGION_PRINT_RELEASE_DIGESTS") != "" {
		for _, kind := range slices.Sorted(maps.Keys(got)) {
			t.Logf("before-alone %q: %q,", kind, got[kind])
		}
	}
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
				CheckRuns: []record.AttemptRun{{Name: "ci", ID: 1}}, Generation: 1, PlannedRed: rng.IntN(2) == 0}
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
