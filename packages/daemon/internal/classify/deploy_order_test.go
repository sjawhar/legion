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

// The release before this one judged these histories, every arrival naming its before but no
// sender, to these digests (made as releaseBeforeDigests were, at this pull request's e5c72dfb3,
// design (c′) with the record keeping one push of each two heads as the release before did).
var beforeAloneDigests = map[string]string{
	"run-1":          "10d09bc5b7092579c106aa940b30dd959cdbde2d9ecd83e76f6707d01f8ee5d6",
	"run-7":          "f670e5f0717d84a8ccd925a1eac0f308120ee2afe2725d078f406eeec3579d2b",
	"move-1-random":  "0eb3cc26c5274f08d6a1dfc652e13200cea52dcdf9f0158a406b725994852993",
	"move-1-bounded": "ef6d0f509f5901e3d9ee8c086b72047a6778173af097394a7df61cc404a614b4",
	"move-7-random":  "d0ea35c954991b6d3ace2f076441e046f028c79f38ffd957dddba37761a498cb",
	"move-7-bounded": "5c40f8ba3ff4ab32386be4a39fcef876cec1b7b6ded5eeb6d88ec9ed4e7591dd",
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

// The required-checks pass rebuilds a lost push with the whole list GitHub's compare gives
// (daemon's lostPushes), and the push webhook of the same two heads can arrive after it with its
// list cut short, none listed, or none sent: that copy changes nothing. The handoff push h0..h1
// the pass rebuilt stays a handoff push, so once h1 has arrived an approval of h0 still stands for
// it and h0's settlement is still carried to it. The other way round, a pass push whose list was
// cut is replaced by the webhook's handoff push of the same two heads, as before.
func TestALatePushWithItsPathsCutShortKeepsTheHandoffPushThePassRebuilt(t *testing.T) {
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
		t.Errorf("the webhook's handoff push after the pass's cut one: pushes %+v, an approval of h0 stands %v, h0's settlement carried %v, want the handoff push alone and both",
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
