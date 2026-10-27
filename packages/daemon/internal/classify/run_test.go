package classify

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// runFact is one fact the property test delivers: a head's arrival, a CI settlement, a read of
// what the base branch requires, the clearing of the head the required workflows were read at, a
// reopen at a head, or a push.
type runFact struct {
	head       string
	settlement *SettlementCandidate
	required   []string
	cleared    bool
	reopen     string
	push       *record.ClassifiedPush
}

func (f runFact) String() string {
	switch {
	case f.head != "":
		return "head " + f.head
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
	case f.head != "":
		return AdvancePullRequestHead(pr, f.head)
	case f.settlement != nil:
		pr, _ = ApplyCheckSettlement(pr, *f.settlement)
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
// runs as they were, nil apart from empty; its head run through JSON.
func stored(t *testing.T, pr record.PullRequest) record.PullRequest {
	t.Helper()
	pr.Failing, pr.Cancelled = append([]string{}, pr.Failing...), append([]string{}, pr.Cancelled...)
	pr.Pushes, pr.Workflows = append([]record.ClassifiedPush{}, pr.Pushes...), append([]record.RequiredWorkflow{}, pr.Workflows...)
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
	prior := stored(t, AdvancePullRequestHead(red, "h3"))
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

// A pull request whose heads each arrive before their pushes keeps a bounded head run and record:
// the run's front folds into its anchor as each push arrives, so the stored run stays as long as
// the pushes lag, here three heads. A push lost for good keeps the run open at its head; the run is
// cut to its newest maxRunSteps steps and stays that size, however many heads follow. Every second
// push is handoff-only, which the record keeps for good (it carries an approval across) and the
// run does not copy; each settlement holds ten check runs and is delivered twice, and the second
// delivery takes no step.
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
		maxSteps, maxPushes int
	}{
		{"every push three heads late", -1, 8, 4},
		{"the fifth push lost", 5, maxRunSteps, maxRunSteps},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := record.PullRequest{HeadSHA: "c0", CheckedHead: "c0", Failing: []string{"check-0"}, Required: []string{"check-0"}, CheckRuns: tenChecks(0)}
			largestRun, largestMoves := 0, 0
			for i := 1; i <= 200; i++ {
				pr = AdvancePullRequestHead(pr, fmt.Sprintf("c%d", i))
				settlement := SettlementCandidate{Head: fmt.Sprintf("c%d", i), CheckRuns: tenChecks(i), Generation: int64(i + 1),
					Snapshot: fmt.Sprintf("s%d", i), Failing: []string{"check-0"}}
				pr, _ = ApplyCheckSettlement(pr, settlement)
				steps := 0
				if pr.Run != nil {
					steps = len(pr.Run.Steps)
				}
				if pr, _ = ApplyCheckSettlement(pr, settlement); pr.Run != nil && len(pr.Run.Steps) != steps {
					t.Fatalf("after %d heads the redelivered settlement took a step: %d steps, want %d", i, len(pr.Run.Steps), steps)
				}
				if late := i - 3; late >= 1 && late != tc.lost {
					pr = ApplyPush(pr, record.ClassifiedPush{SHA: fmt.Sprintf("c%d", late), Before: fmt.Sprintf("c%d", late-1), HandoffOnly: late%2 == 0})
				}
				if pr.Run != nil {
					largestRun = max(largestRun, encoded(pr.Run))
					if len(pr.Run.Steps) > tc.maxSteps || len(pr.Run.Pushes) > tc.maxPushes {
						t.Fatalf("after %d heads the head run holds %d steps and %d pushes, want at most %d and %d",
							i, len(pr.Run.Steps), len(pr.Run.Pushes), tc.maxSteps, tc.maxPushes)
					}
				}
				if len(pr.Moves) > maxMoves {
					t.Fatalf("after %d heads the record holds %d moves, want at most %d", i, len(pr.Moves), maxMoves)
				}
				largestMoves = max(largestMoves, encoded(pr.Moves))
			}
			t.Logf("largest stored head run: %d bytes; largest stored moves: %d bytes; pushes kept at the end: %d (%d bytes)",
				largestRun, largestMoves, len(pr.Pushes), encoded(pr.Pushes))
		})
	}
}

// BenchmarkReplayAtTheBound replays a head run of maxRunSteps steps - heads and settlements of ten
// check runs alternating, every head waiting for its push - which is the most a push replays.
func BenchmarkReplayAtTheBound(b *testing.B) {
	pr := record.PullRequest{HeadSHA: "c0", CheckedHead: "c0", Failing: []string{"check-0"}, Required: []string{"check-0"}, CheckRuns: tenChecks(0)}
	for i := 1; pr.Run == nil || len(pr.Run.Steps) < maxRunSteps; i++ {
		pr = AdvancePullRequestHead(pr, fmt.Sprintf("c%d", i))
		pr, _ = ApplyCheckSettlement(pr, SettlementCandidate{Head: fmt.Sprintf("c%d", i), CheckRuns: tenChecks(i),
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
