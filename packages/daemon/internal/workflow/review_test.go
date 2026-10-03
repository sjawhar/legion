package workflow

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A review ends when the reviewer completes it, not when GitHub reports it: the reviewer's handoff
// is part of its phase's contract, and a transition fired on the review event left it nowhere to
// land — the issue had already moved on, so the reviewer's completion was refused as a phase it no
// longer runs and its review.json never recorded. GitHub's decision and the reviewer's completion
// can arrive in either order; whichever lands second moves the issue.
func TestAReviewEndsWhenTheReviewerCompletesIt(t *testing.T) {
	for _, tc := range []struct {
		name          string
		decision      string
		verdict       string
		completeFirst bool
		want          phase.Phase
	}{
		{name: "changes requested, then the reviewer completes", decision: "changes_requested", want: phase.Implementing},
		{name: "the reviewer completes, then changes are requested", decision: "changes_requested", completeFirst: true, want: phase.Implementing},
		{name: "approved on a green head, then the reviewer completes", decision: "approved", verdict: "green", want: phase.Retro},
		{name: "the reviewer completes, then an approval on a green head", decision: "approved", verdict: "green", completeFirst: true, want: phase.Retro},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := context.Background()
			seedReview(t, pool, tc.verdict)
			engine := testEngine(config.DesignGateRootIssues, nil)
			review := func() {
				t.Helper()
				if _, err := intake.ApplyFact(ctx, pool, "github", "review", intake.PullRequestReview{
					Repo: "sjawhar/legion", Number: 42, State: tc.decision, CommitID: "head", Body: "the review"}, engine); err != nil {
					t.Fatalf("apply the review: %v", err)
				}
			}
			complete := func() {
				t.Helper()
				result, err := intake.ApplyFact(ctx, pool, "api", "reviewer-handoff", intake.HandoffComplete{Generation: 1,
					Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"}, engine)
				if err != nil || result.Refusal != nil {
					t.Fatalf("the reviewer's completion = %+v, %v; want it recorded", result.Refusal, err)
				}
			}
			if tc.completeFirst {
				complete()
				if got := issuePhase(t, pool); got != phase.Reviewing {
					t.Fatalf("after the completion alone the issue is in %s, want reviewing until GitHub reports the decision", got)
				}
				review()
			} else {
				review()
				if got := issuePhase(t, pool); got != phase.Reviewing {
					t.Fatalf("after the review alone the issue is in %s, want reviewing until the reviewer completes", got)
				}
				complete()
			}

			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("the issue is in %s, want %s", got, tc.want)
			}
			var commit string
			if err := pool.QueryRow(ctx, "select handoff_commit from phases where issue = $1 and role = $2",
				"LEGION-208", "reviewer").Scan(&commit); err != nil {
				t.Fatal(err)
			}
			if commit != "review-1" {
				t.Fatalf("the reviewer's handoff commit = %q, want its completion's review-1", commit)
			}
		})
	}
}

// An approval does not end the review while the head's checks have not come back green: the
// reviewer's completion waits for them as the approval always did, and their settling moves it.
func TestAnApprovalWaitsForGreenChecksAfterTheReviewerCompletes(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedReview(t, pool, "")
	engine := testEngine(config.DesignGateRootIssues, nil)
	if _, err := intake.ApplyFact(ctx, pool, "github", "review", intake.PullRequestReview{
		Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "head"}, engine); err != nil {
		t.Fatalf("apply the review: %v", err)
	}
	if result, err := intake.ApplyFact(ctx, pool, "api", "reviewer-handoff", intake.HandoffComplete{Generation: 1,
		Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"}, engine); err != nil || result.Refusal != nil {
		t.Fatalf("the reviewer's completion = %+v, %v", result.Refusal, err)
	}
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("with the checks unsettled the issue is in %s, want reviewing", got)
	}
}

func seedReview(t *testing.T, pool *pgxpool.Pool, verdict string) {
	t.Helper()
	seedReviewOf(t, pool, "head", verdict)
}

// seedReviewOf seeds LEGION-208 in reviewing, its pull request's head at head with verdict
// settled on it, and its reviewer's round not yet completed.
func seedReviewOf(t *testing.T, pool *pgxpool.Pool, head, verdict string) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: head, Verdict: verdict})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
}

func issuePhase(t *testing.T, pool *pgxpool.Pool) phase.Phase {
	t.Helper()
	var got string
	if err := pool.QueryRow(context.Background(), "select phase from issues where key = $1", "LEGION-208").Scan(&got); err != nil {
		t.Fatal(err)
	}
	return phase.Phase(got)
}

// An approval is of the code the head it names carries. The reviewer approves the head it was
// shown - the tester's handoff head - and its own handoff push then replaces that head, while the
// approval event and the push arrive by different webhook paths in no promised order. So an
// approval stands for the current head when every push since the head it names changed only
// .legion/, whichever of the approval, the new heads and the pushes' paths is processed first. A
// push that changes code, whose changed paths are not known, or that rewrote history - or did not
// say whether it did - carries no approval across. Deciding reviews are ordered by when they were
// submitted, then by GitHub's review id, so the newest one submitted decides; a review without a
// submission time is ordered by id against any other. A pull request recorded before the daemon
// kept its pushes has none, so only an approval of its current head stands.
func TestAnApprovalStandsForEveryHeadThatChangesNothingButTheHandoff(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []string
		want  phase.Phase
		// unsettled seeds the approved head with its checks still running, rather than green.
		unsettled bool
	}{
		{name: "approval, then the reviewer's handoff push", steps: []string{"approve head", "sync", "push handoff", "green", "complete"}, want: phase.Retro},
		{name: "the reviewer's handoff push, then the approval", steps: []string{"sync", "push handoff", "green", "approve head", "complete"}, want: phase.Retro},
		{name: "the push's paths arrive after the new head and the approval", steps: []string{"sync", "approve head", "green", "complete", "push handoff"}, want: phase.Retro},
		{name: "the push's paths arrive before the new head", steps: []string{"push handoff", "approve head", "sync", "green", "complete"}, want: phase.Retro},
		{name: "a new head whose push never arrives", steps: []string{"sync", "approve head", "green", "complete"}, want: phase.Reviewing},
		{name: "a code push, then the approval of the head before it", steps: []string{"sync", "push code", "green", "approve head", "complete"}, want: phase.Reviewing},
		{name: "the approval, then a code push", steps: []string{"approve head", "sync", "push code", "green", "complete"}, want: phase.Reviewing},
		{name: "a push of truncated paths, then the approval of the head before it", steps: []string{"sync", "push truncated", "green", "approve head", "complete"}, want: phase.Reviewing},
		{name: "a push with no paths marker, then the approval of the head before it", steps: []string{"sync", "push paths unmarked", "green", "approve head", "complete"}, want: phase.Reviewing},
		{name: "a comment between a code push and its new head", steps: []string{"approve head", "push code", "comment", "sync", "green", "complete"}, want: phase.Reviewing},
		{name: "a comment on the handoff head after the approval", steps: []string{"approve head", "sync", "push handoff", "comment", "green", "complete"}, want: phase.Retro},
		{name: "a code push whose new head has not arrived", steps: []string{"approve head", "push code", "complete"}, want: phase.Reviewing},
		{name: "a code push whose new head has not arrived, then the approved head's checks", steps: []string{"approve head", "complete", "push code", "green head"}, want: phase.Reviewing, unsettled: true},
		{name: "a code push, then a handoff push on top of it, neither head arrived", steps: []string{"approve head", "push code", "push handoff head-3", "complete"}, want: phase.Reviewing},
		{name: "a code push that does not say which head it replaced", steps: []string{"approve head", "push code unplaced", "complete"}, want: phase.Reviewing},
		{name: "a code push that did not say which head it replaced, then the branch moves on without it", steps: []string{"approve head", "sync", "push handoff", "push code unplaced head-x", "sync head-3", "push handoff head-3", "green head-3", "complete"}, want: phase.Retro},
		{name: "a code head never synchronized, under a handoff head, pushes first", steps: []string{"push code", "push handoff head-3", "sync head-3", "green head-3", "approve head", "complete"}, want: phase.Reviewing},
		{name: "a code head never synchronized, under a handoff head, the head first", steps: []string{"sync head-3", "push code", "push handoff head-3", "green head-3", "approve head", "complete"}, want: phase.Reviewing},
		{name: "a late code push for a head already replaced", steps: []string{"approve head", "sync", "push handoff", "push code head-x", "green", "complete"}, want: phase.Retro},
		{name: "two handoff pushes, each before its head", steps: []string{"approve head", "push handoff", "sync", "push handoff head-3", "sync head-3", "green head-3", "complete"}, want: phase.Retro},
		{name: "two handoff pushes, each after its head", steps: []string{"approve head", "sync", "push handoff", "sync head-3", "push handoff head-3", "green head-3", "complete"}, want: phase.Retro},
		{name: "two handoff pushes, both classified after both heads, the second first", steps: []string{"approve head", "sync", "sync head-3", "push handoff head-3", "push handoff", "green head-3", "complete"}, want: phase.Retro},
		{name: "two handoff pushes, both classified after both heads, in order", steps: []string{"approve head", "sync", "sync head-3", "push handoff", "push handoff head-3", "green head-3", "complete"}, want: phase.Retro},
		{name: "two handoff pushes, the second classified between the heads", steps: []string{"approve head", "sync", "push handoff head-3", "sync head-3", "push handoff", "green head-3", "complete"}, want: phase.Retro},
		{name: "a force push to a handoff commit on an older base, its push first", steps: []string{"approve head", "push handoff forced head-l", "sync head-l", "green head-l", "complete"}, want: phase.Reviewing},
		{name: "a force push to a handoff commit on an older base, its head first", steps: []string{"approve head", "sync head-l", "push handoff forced head-l", "green head-l", "complete"}, want: phase.Reviewing},
		{name: "a handoff push that does not say whether it was forced", steps: []string{"approve head", "sync", "push handoff unmarked", "green", "complete"}, want: phase.Reviewing},
		{name: "a request for changes written after the approval, delivered first", steps: []string{"cr head id=12", "approve head id=11", "complete"}, want: phase.Implementing},
		{name: "a request for changes written after the approval, delivered after", steps: []string{"approve head id=11", "cr head id=12", "complete"}, want: phase.Implementing},
		{name: "a comment written after a request for changes, delivered first", steps: []string{"comment id=12", "cr head id=11", "complete"}, want: phase.Implementing},
		{name: "a comment written after an approval, delivered first", steps: []string{"comment id=12", "approve head id=11", "complete"}, want: phase.Retro},
		{name: "an approval written after the request for changes", steps: []string{"cr head id=11", "approve head id=12", "complete"}, want: phase.Retro},
		{name: "a draft's request for changes submitted after a one-step approval", steps: []string{"approve head id=101 at=2", "cr head id=100 at=3", "complete"}, want: phase.Implementing},
		{name: "a draft's request for changes submitted after a one-step approval, delivered first", steps: []string{"cr head id=100 at=3", "approve head id=101 at=2", "complete"}, want: phase.Implementing},
		{name: "a draft's approval submitted after a request for changes", steps: []string{"cr head id=101 at=2", "approve head id=100 at=3", "complete"}, want: phase.Retro},
		{name: "an untimed request for changes, then an older approval redelivered with a time", steps: []string{"cr head id=12", "approve head id=11 at=2", "complete"}, want: phase.Implementing},
		{name: "a timed approval, then a newer untimed request for changes", steps: []string{"approve head id=11 at=2", "cr head id=12", "complete"}, want: phase.Implementing},
		{name: "a code push delivered after the branch was reset to the approved head", steps: []string{"approve head", "sync", "sync head-3", "push code", "push handoff forced from=head-3 head", "sync head", "green head", "complete"}, want: phase.Reviewing},
		{name: "a code push delivered after the branch was reset to the approved head, then the reviewer's handoff push", steps: []string{"approve head", "sync", "sync head-3", "push code", "push handoff forced from=head-3 head", "sync head", "green head", "complete", "push handoff from=head head-4", "sync head-4", "green head-4"}, want: phase.Retro},
		// A handoff push can carry GitHub's skip-checks trailer and start no CI, so the code head's
		// settlement stands for the handoff head that replaced it, whenever it arrives.
		// An absence of information never displaces information: a settlement whose checks all
		// ended cancelled records no verdict, and the code head's green stands for the handoff head
		// whether the cancelled one is the handoff head's own run, a re-settlement of the code head,
		// or arrives first.
		{name: "the handoff head's own run cancelled after the code head's green", steps: []string{"green head", "approve head", "sync", "push handoff", "cancelled", "complete"}, want: phase.Retro, unsettled: true},
		{name: "a cancelled re-settlement of the code head after its green", steps: []string{"green head", "approve head", "sync", "push handoff", "cancelled head", "complete"}, want: phase.Retro, unsettled: true},
		{name: "the handoff head's own run cancelled before the code head's green", steps: []string{"approve head", "sync", "push handoff", "cancelled", "green head", "complete"}, want: phase.Retro, unsettled: true},
		{name: "the code head's checks settle after the reviewer's handoff head", steps: []string{"approve head", "sync", "push handoff", "green head", "complete"}, want: phase.Retro, unsettled: true},
		{name: "the code head's checks settle before the reviewer's handoff head, its push last", steps: []string{"green head", "approve head", "sync", "push handoff", "complete"}, want: phase.Retro, unsettled: true},
		{name: "the code head's checks settle before the reviewer's handoff head, its push first", steps: []string{"green head", "approve head", "push handoff", "sync", "complete"}, want: phase.Retro, unsettled: true},
		{name: "the code head's checks settle under two handoff heads", steps: []string{"approve head", "sync", "push handoff", "sync head-3", "push handoff head-3", "green head", "complete"}, want: phase.Retro, unsettled: true},
		{name: "the checks of a head a code push replaced", steps: []string{"approve head", "sync", "push code", "green head", "complete"}, want: phase.Reviewing, unsettled: true},
		{name: "a late settlement of a head a code push replaced leaves the head's own", steps: []string{"sync", "push code", "green", "red head", "approve", "complete"}, want: phase.Retro, unsettled: true},
		{name: "the checks of a head a force push replaced", steps: []string{"approve head", "sync head-l", "push handoff forced head-l", "green head", "complete"}, want: phase.Reviewing, unsettled: true},
		{name: "the handoff head's own red outranks the code head's late green", steps: []string{"approve head", "sync", "push handoff", "red", "green head", "complete"}, want: phase.Reviewing, unsettled: true},
		{name: "the handoff head's own green outranks the code head's late red", steps: []string{"approve head", "sync", "push handoff", "green", "red head", "complete"}, want: phase.Retro, unsettled: true},
		{name: "recorded before the chain: an approval of the current head", steps: []string{"approve head", "complete"}, want: phase.Retro},
		{name: "recorded before the chain: an approval of an earlier head", steps: []string{"approve older", "complete"}, want: phase.Reviewing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := context.Background()
			verdict := "green"
			if tc.unsettled {
				verdict = ""
			}
			seedReview(t, pool, verdict)
			engine := testEngine(config.DesignGateRootIssues, nil)
			// A head's push replaces the head before it; head-x is a late push's, whose head was
			// never the current one.
			before := map[string]string{"head-2": "head", "head-3": "head-2", "head-x": "older", "head-l": "head"}
			for i, step := range tc.steps {
				// A step is its name, then optional words: a head it names (head, head-2, ...), a
				// review id (id=N), a review's submission time (at=N, minutes past noon), the head a
				// push replaced when the map below does not say (from=H), "forced" for a push that
				// rewrote history, and "unmarked" for a push whose listener did not say.
				head, id, forced, from := "head-2", int64(0), "false", ""
				var submitted time.Time
				var name []string
				for _, word := range strings.Fields(step) {
					switch {
					case strings.HasPrefix(word, "head"):
						head = word
					case strings.HasPrefix(word, "id="):
						id, _ = strconv.ParseInt(strings.TrimPrefix(word, "id="), 10, 64)
					case strings.HasPrefix(word, "at="):
						minutes, _ := strconv.Atoi(strings.TrimPrefix(word, "at="))
						submitted = time.Date(2026, 9, 26, 12, minutes, 0, 0, time.UTC)
					case strings.HasPrefix(word, "from="):
						from = strings.TrimPrefix(word, "from=")
					case word == "forced":
						forced = "true"
					case word == "unmarked":
						forced = ""
					default:
						name = append(name, word)
					}
				}
				step := strings.Join(name, " ")
				var fact intake.Fact
				switch step {
				case "approve", "approve older", "cr":
					commit, state := head, "approved"
					if step == "approve older" {
						commit = "older"
					}
					if step == "cr" {
						state = "changes_requested"
					}
					fact = intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: id, SubmittedAt: submitted, State: state,
						CommitID: commit, Body: step}
				case "sync":
					fact = intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: head}
				case "comment":
					fact = intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: id, State: "commented", CommitID: "head-2", Body: "a comment"}
				case "push handoff", "push code", "push code unplaced", "push truncated", "push paths unmarked":
					paths, truncated := ".legion/review.json", "false"
					marker := &truncated
					switch step {
					case "push code", "push code unplaced":
						paths = ".legion/review.json\nsrc/widget.go"
					case "push truncated":
						truncated = "true"
					case "push paths unmarked":
						marker = nil
					}
					var forcedMarker *string
					if forced != "" {
						forcedMarker = &forced
					}
					replaced := before[head]
					if from != "" {
						replaced = from
					}
					if step == "push code unplaced" {
						replaced = ""
					}
					fact = intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: replaced, After: head,
						ChangedPaths: &paths, Truncated: marker, Forced: forcedMarker, Pusher: "legion-reviewer[bot]"}
				case "green":
					fact = intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: head,
						CheckRuns: []record.AttemptRun{{Name: "ci", ID: 2}}, Generation: 2, Snapshot: "green-" + head, Verdict: "green", Failing: []string{}}
				case "red":
					fact = intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: head,
						CheckRuns: []record.AttemptRun{{Name: "ci", ID: 3}}, Generation: 2, Snapshot: "red-" + head, Verdict: "red", Failing: []string{"ci"}}
				case "cancelled":
					// Every check ended cancelled and none failed: the listener's settlement records no verdict.
					fact = intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: head,
						CheckRuns: []record.AttemptRun{{Name: "ci", ID: 4}}, Generation: 3, Snapshot: "cancelled-" + head, Verdict: "", Failing: []string{}}
				case "complete":
					fact = intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim",
						Summary: "reviewed", Commit: "review-1"}
				}
				result, err := intake.ApplyFact(ctx, pool, "test", fmt.Sprintf("%d-%s", i, step), fact, engine)
				if err != nil || result.Refusal != nil {
					t.Fatalf("%s = %+v, %v", step, result.Refusal, err)
				}
			}
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("the issue is in %s, want %s", got, tc.want)
			}
		})
	}
}

// A review that carries no decision - a comment - leaves the round's decision and its reason as
// they are: the next implementer is told what the request for changes said, not the comment
// posted after it.
func TestACommentLeavesTheRoundsRequestForChanges(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedReview(t, pool, "")
	engine := testEngine(config.DesignGateRootIssues, nil)
	for i, fact := range []intake.Fact{
		intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "changes_requested", CommitID: "head", Body: "rename the widget"},
		intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "commented", CommitID: "head", Body: "one more thought"},
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"},
	} {
		if result, err := intake.ApplyFact(ctx, pool, "test", fmt.Sprintf("step-%d", i), fact, engine); err != nil || result.Refusal != nil {
			t.Fatalf("step %d = %+v, %v", i, result.Refusal, err)
		}
	}
	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("the issue is in %s, want implementing", got)
	}
	var task string
	if err := pool.QueryRow(ctx, "select payload->>'task' from outbox where kind = 'supervise' and payload->>'op' = 'start' order by id desc limit 1").Scan(&task); err != nil {
		t.Fatalf("read the implementer's start task: %v", err)
	}
	if !strings.Contains(task, "rename the widget") || strings.Contains(task, "one more thought") {
		t.Fatalf("the implementer's task = %q, want the request for changes' body and not the comment's", task)
	}
}

// Deciding reviews are ordered by when they were submitted, then by GitHub's review id, and the
// order holds across rounds: a review from an earlier round, delivered again while a later round is
// open, is no newer than one already processed and records nothing, so the reviewer's completion
// waits for the round's own review.
func TestAReviewFromAnEarlierRoundDeliveredAgainRecordsNothing(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedReview(t, pool, "green")
	engine := testEngine(config.DesignGateRootIssues, nil)
	requestChanges := intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 10, State: "changes_requested", CommitID: "head", Body: "round 1"}
	for i, fact := range []intake.Fact{
		requestChanges,
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"},
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "fixed", Commit: "round-2"},
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim", Summary: "tested", Verdict: "pass", Commit: "test-2"},
		requestChanges,
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-2"},
		intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 11, State: "approved", CommitID: "head", Body: "round 2"},
	} {
		if result, err := intake.ApplyFact(ctx, pool, "test", fmt.Sprintf("step-%d", i), fact, engine); err != nil || result.Refusal != nil {
			t.Fatalf("step %d = %+v, %v", i, result.Refusal, err)
		}
		switch got := issuePhase(t, pool); {
		case i == 1 && got != phase.Implementing:
			t.Fatalf("after round 1 the issue is in %s, want implementing", got)
		case i == 5 && got != phase.Reviewing:
			t.Fatalf("after round 1's review delivered again and round 2's completion the issue is in %s, want reviewing", got)
		}
	}
	if got := issuePhase(t, pool); got != phase.Retro {
		t.Fatalf("the issue is in %s, want retro: round 2's approval is the newest review", got)
	}
}

// A review that arrives once the round has ended - in retro, after the approval moved the issue on
// - decides nothing and counts no round: the rounds and the pull request's blocked notice belong
// to the round that was open.
func TestAReviewOutsideReviewingRecordsNoRound(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedReview(t, pool, "green")
	engine := testEngine(config.DesignGateRootIssues, nil)
	rounds := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, "select rounds from phases where issue = $1 and role = $2", "LEGION-208", "implementer").Scan(&n); err != nil {
			t.Fatalf("read the implementer's rounds: %v", err)
		}
		return n
	}
	for i, fact := range []intake.Fact{
		intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 11, State: "approved", CommitID: "head"},
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"},
	} {
		if result, err := intake.ApplyFact(ctx, pool, "test", fmt.Sprintf("step-%d", i), fact, engine); err != nil || result.Refusal != nil {
			t.Fatalf("step %d = %+v, %v", i, result.Refusal, err)
		}
	}
	if got := issuePhase(t, pool); got != phase.Retro {
		t.Fatalf("the issue is in %s, want retro", got)
	}
	before := rounds()
	if _, err := intake.ApplyFact(ctx, pool, "test", "late", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 12,
		State: "changes_requested", CommitID: "head", Body: "too late"}, engine); err != nil {
		t.Fatalf("apply the late review: %v", err)
	}
	if got := issuePhase(t, pool); got != phase.Retro {
		t.Fatalf("after the late review the issue is in %s, want retro", got)
	}
	if got := rounds(); got != before {
		t.Fatalf("the implementer's rounds went from %d to %d on a review outside reviewing", before, got)
	}
}

// A review can be processed while its issue is held from reviewing: the reviewer's launch budget
// ran out after it posted the review. That review still decides the round, so once the retry puts
// the issue back in reviewing, the reviewer's completion ends the round on it.
func TestAReviewWhileHeldFromReviewingDecidesTheRound(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  phase.Phase
	}{
		{state: "approved", want: phase.Retro},
		{state: "changes_requested", want: phase.Implementing},
	} {
		t.Run(tc.state, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := context.Background()
			seedReview(t, pool, "green")
			engine := testEngine(config.DesignGateRootIssues, nil)
			for i, fact := range []intake.Fact{
				intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleReviewer},
				intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 11, State: tc.state, CommitID: "head", Body: "the review"},
				intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision},
				intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"},
			} {
				if i == 1 {
					if got := issuePhase(t, pool); got != phase.Held {
						t.Fatalf("before the review the issue is in %s, want held", got)
					}
				}
				result, err := intake.ApplyFact(ctx, pool, "test", fmt.Sprintf("held-%d", i), fact, engine)
				if err != nil || result.Refusal != nil {
					t.Fatalf("fact %d (%T) = %+v, %v", i, fact, result.Refusal, err)
				}
			}
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("the issue is in %s, want %s", got, tc.want)
			}
		})
	}
}

// Reviews are ordered across the pull request's whole life: reopening it keeps the newest deciding
// review it has had, so an earlier review delivered again after the reopen records nothing.
func TestAReopenedPullRequestKeepsItsNewestReview(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedReview(t, pool, "green")
	engine := testEngine(config.DesignGateRootIssues, nil)
	for i, fact := range []intake.Fact{
		intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 12, State: "approved", CommitID: "head", Body: "approved"},
		intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"},
		intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 3}},
			Generation: 3, Snapshot: "green-again", Verdict: "green", Failing: []string{}},
		intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 11, State: "changes_requested", CommitID: "head", Body: "redelivered"},
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"},
	} {
		result, err := intake.ApplyFact(ctx, pool, "test", fmt.Sprintf("reopen-%d", i), fact, engine)
		if err != nil || result.Refusal != nil {
			t.Fatalf("fact %d (%T) = %+v, %v", i, fact, result.Refusal, err)
		}
	}
	if got := issuePhase(t, pool); got != phase.Retro {
		t.Fatalf("the issue is in %s, want retro: the redelivered request for changes is older than the approval", got)
	}
}

// The reviewer can complete before its review arrives and before the issue is held from reviewing:
// the review then lands while held, and the retry that restores reviewing finds both halves of the
// round already in. It ends the round there rather than starting the reviewer again, whose second
// completion of the same commit would be refused as not new and leave the issue in reviewing.
func TestARetryEndsARoundWhoseReviewerCompletedBeforeTheHold(t *testing.T) {
	for _, tc := range []struct {
		state  string
		want   phase.Phase
		rounds int
	}{
		{state: "approved", want: phase.Retro},
		{state: "changes_requested", want: phase.Implementing, rounds: 1},
	} {
		t.Run(tc.state, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := context.Background()
			seedReview(t, pool, "green")
			engine := testEngine(config.DesignGateRootIssues, nil)
			for i, fact := range []intake.Fact{
				intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"},
				intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleReviewer},
				intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, ID: 11, State: tc.state, CommitID: "head", Body: "the review"},
				intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision},
			} {
				result, err := intake.ApplyFact(ctx, pool, "test", fmt.Sprintf("early-%d", i), fact, engine)
				if err != nil || result.Refusal != nil {
					t.Fatalf("fact %d (%T) = %+v, %v", i, fact, result.Refusal, err)
				}
			}
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("the issue is in %s, want %s", got, tc.want)
			}
			var starts int
			if err := pool.QueryRow(ctx, `select count(*) from outbox where kind = 'supervise' and payload->>'op' = 'start'
				and payload->>'role' = 'reviewer' and payload->>'phase' = 'reviewing'`).Scan(&starts); err != nil {
				t.Fatal(err)
			}
			if starts != 0 {
				t.Fatalf("the retry started the reviewer %d times, want none: its round had already ended", starts)
			}
			var rounds int
			if err := pool.QueryRow(ctx, "select rounds from phases where issue = $1 and role = $2", "LEGION-208", "implementer").Scan(&rounds); err != nil {
				t.Fatal(err)
			}
			if rounds != tc.rounds {
				t.Fatalf("the implementer's rounds = %d, want %d: the round ended once", rounds, tc.rounds)
			}
		})
	}
}

// A round the retry restores to reviewing that its reviewer completed before the hold, and that no
// review decides, is stuck as it was: the restarted reviewer's task says why, so it submits the
// decision rather than reviewing afresh. The architect ordered the retry, so it is told nothing new:
// the one review-stuck notice is the completion's, and a second would read as the reviewer's answer.
func TestARetryOfAStuckRoundTellsTheReviewerWhy(t *testing.T) {
	pool := migratedPool(t)
	seedReviewOf(t, pool, "c0ffee", "green")
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
	for i, fact := range []intake.Fact{
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"},
		intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleReviewer},
		intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision},
	} {
		if result := apply(fmt.Sprintf("stuck-%d", i), fact); result.Refusal != nil {
			t.Fatalf("fact %d (%T) was refused: %+v", i, fact, result.Refusal)
		}
	}
	if got := issuePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("the issue is in %s, want reviewing", got)
	}
	told := reviewStuckNotices(t, pool)
	if len(told) != 1 {
		t.Fatalf("the architect was told %d times (%+v), want once: at the completion, not at the retry", len(told), told)
	}
	var task string
	if err := pool.QueryRow(t.Context(), `select payload->>'task' from outbox where kind = 'supervise' and payload->>'op' = 'start'
		and payload->>'role' = 'reviewer' order by id desc limit 1`).Scan(&task); err != nil {
		t.Fatalf("read the reviewer's retry task: %v", err)
	}
	if !strings.Contains(task, told[0].Reason) {
		t.Fatalf("the reviewer's retry task = %q, want it to carry the reason the architect was told, %q", task, told[0].Reason)
	}
}

// A red standing at a head that a push which may change code made sends the work back to
// implementing, whatever the round holds, as the red's own settlement does in reviewing
// (classify.RedSendsBack): the round never decides it. It meets a completed round only through a
// hold - it settled while the issue was held from reviewing, where nothing moves - and the retry
// that restores reviewing sends the work back rather than reporting the round stuck.
func TestARetryOfARoundWithARedCodeHeadSendsTheWorkBack(t *testing.T) {
	pool := migratedPool(t)
	seedReviewOf(t, pool, "c0ffee", "")
	apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
	for i, fact := range []intake.Fact{
		intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"},
		intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleReviewer},
		intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "c0ffee", CheckRuns: []record.AttemptRun{{Name: "lint", ID: 1}},
			Generation: 1, Snapshot: "red-c0ffee", Verdict: "red", Failing: []string{"lint"}},
		intake.RetryOrEscalate{Issue: "LEGION-208", Decision: intake.RetryDecision},
	} {
		if result := apply(fmt.Sprintf("red-%d", i), fact); result.Refusal != nil {
			t.Fatalf("fact %d (%T) was refused: %+v", i, fact, result.Refusal)
		}
	}
	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("the issue is in %s, want implementing: the code head is red", got)
	}
	if got := noticeKinds(t, pool, "LEGION-208"); !containsNotice(got, "checks-red") {
		t.Fatalf("notices %v, want checks-red", got)
	}
	if got := reviewStuckNotices(t, pool); len(got) != 1 {
		t.Fatalf("the architect was told the round was stuck %d times (%+v), want once: at the completion, before the red", len(got), got)
	}
	var starts int
	if err := pool.QueryRow(t.Context(), `select count(*) from outbox where kind = 'supervise' and payload->>'op' = 'start'
		and payload->>'role' = 'implementer'`).Scan(&starts); err != nil || starts != 1 {
		t.Fatalf("implementer starts = %d, %v; want one", starts, err)
	}
}

// The reviewer's approval submitted before its completion but delivered after it ends the round as
// any approval does, and tells nothing beyond the completion's notice.
func TestALateDeliveredApprovalEndsAStuckRound(t *testing.T) {
	pool := migratedPool(t)
	seedReviewOf(t, pool, "c0ffee", "green")
	engine := testEngine(config.DesignGateRootIssues, nil)
	engine.cfg.ReviewAppLogin = "legion-reviewer[bot]"
	apply := applyFacts(t, pool, engine)
	apply("complete", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"})
	apply("approve", intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "c0ffee", Author: "legion-reviewer[bot]",
		Body: "ship it", SubmittedAt: time.Date(2026, 9, 22, 23, 59, 0, 0, time.UTC)})
	if got := issuePhase(t, pool); got != phase.Retro {
		t.Fatalf("the issue is in %s, want retro", got)
	}
	if got := reviewStuckNotices(t, pool); len(got) != 1 {
		t.Fatalf("the architect was told %d times (%+v), want once: at the completion", len(got), got)
	}
}

// A round ends only on a review that decides it: an APPROVE of the head or a REQUEST_CHANGES. A
// reviewer that completes its round with none - it submitted a COMMENT, a review in any other
// state, or no review at all - leaves the issue in reviewing with nothing on its way to end it, so
// its architect is told, naming the head, and told again each time the reviewer (the review App)
// submits another review that decides nothing. A reply on a review thread is a review GitHub gives
// no body, not a submitted one, and it tells nothing; nor does anyone else's comment, which changes
// nothing about the round. A review that arrives before the reviewer completes tells nobody: the
// completion is still to come. A deciding review from the same round still ends it, as always.
func TestARoundNoReviewDecidesTellsTheArchitect(t *testing.T) {
	const reviewApp = "legion-reviewer[bot]"
	// testEngine's clock applies the completion at midnight; GitHub stamps each review's submission,
	// and answerSkew bounds how far the two clocks can disagree.
	early, late := time.Date(2026, 9, 22, 23, 59, 0, 0, time.UTC), time.Date(2026, 9, 23, 0, 0, 30, 0, time.UTC)
	withinSkew := time.Date(2026, 9, 23, 0, 0, 5, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		before   []string
		after    []intake.PullRequestReview
		told     int
		decision string
		want     phase.Phase
	}{
		{name: "a COMMENT, then the reviewer completes", before: []string{"commented"}, told: 1, decision: "approved", want: phase.Retro},
		{name: "no review at all", told: 1, decision: "approved", want: phase.Retro},
		{name: "the reviewer completes, then its COMMENT submitted after the completion", after: []intake.PullRequestReview{{State: "commented", Author: reviewApp, Body: "one more thought", SubmittedAt: late}},
			told: 2, decision: "changes_requested", want: phase.Implementing},
		{name: "the reviewer completes, then its COMMENT submitted before the completion, delivered after", after: []intake.PullRequestReview{{State: "commented", Author: reviewApp, Body: "one more thought", SubmittedAt: early}},
			told: 1, decision: "changes_requested", want: phase.Implementing},
		{name: "the reviewer completes, then its COMMENT with no submission time", after: []intake.PullRequestReview{{State: "commented", Author: reviewApp, Body: "one more thought"}},
			told: 1, decision: "approved", want: phase.Retro},
		{name: "the reviewer completes, then its COMMENT submitted within the clocks' skew of the completion", after: []intake.PullRequestReview{{State: "commented", Author: reviewApp, Body: "one more thought", SubmittedAt: withinSkew}},
			told: 1, decision: "approved", want: phase.Retro},
		{name: "the reviewer completes, then its reply on a review thread", after: []intake.PullRequestReview{{State: "commented", Author: reviewApp, SubmittedAt: late}},
			told: 1, decision: "approved", want: phase.Retro},
		{name: "the reviewer completes, then another account's COMMENT", after: []intake.PullRequestReview{{State: "commented", Author: "a-human", Body: "a thought", SubmittedAt: late}},
			told: 1, decision: "approved", want: phase.Retro},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedReviewOf(t, pool, "c0ffee", "green")
			engine := testEngine(config.DesignGateRootIssues, nil)
			engine.cfg.ReviewAppLogin = reviewApp
			apply := applyFacts(t, pool, engine)
			review := func(id string, review intake.PullRequestReview) {
				t.Helper()
				review.Repo, review.Number, review.CommitID = "sjawhar/legion", 42, "c0ffee"
				apply(id, review)
			}
			for i, state := range tc.before {
				review(fmt.Sprintf("before-%d", i), intake.PullRequestReview{State: state, Author: reviewApp, Body: "the " + state + " review"})
				if got := reviewStuckNotices(t, pool); len(got) != 0 {
					t.Fatalf("a %s review before the reviewer completed told the architect %+v, want nothing", state, got)
				}
			}
			if result := apply("complete", intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer,
				Claim: "review-claim", Summary: "reviewed", Commit: "review-1"}); result.Refusal != nil {
				t.Fatalf("the reviewer's completion was refused: %+v", result.Refusal)
			}
			for i, after := range tc.after {
				review(fmt.Sprintf("after-%d", i), after)
			}
			if got := issuePhase(t, pool); got != phase.Reviewing {
				t.Fatalf("after the reviews that followed the completion the issue is in %s, want reviewing", got)
			}
			told := reviewStuckNotices(t, pool)
			if len(told) != tc.told {
				t.Fatalf("the architect was told %d times (%+v), want %d", len(told), told, tc.told)
			}
			for _, notice := range told {
				if notice.Role != claim.RoleReviewer || notice.Phase != phase.Reviewing || !strings.Contains(notice.Reason, "c0ffee") ||
					notice.Topic != "notifications.role.legion-legion-legion-208-reviewer" {
					t.Fatalf("notice %+v, want the reviewer's reviewing round, naming head c0ffee, at the reviewer's role topic", notice)
				}
			}
			if told[0].Summary != "the reviewer's completion" || len(told) == 2 && told[1].Summary != "the reviewer's review" {
				t.Fatalf("notices %+v, want the first written by the reviewer's completion and any second by its review", told)
			}
			review("decides", intake.PullRequestReview{State: tc.decision, Author: reviewApp, Body: "the decision", SubmittedAt: late})
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("after the %s review the issue is in %s, want %s", tc.decision, got, tc.want)
			}
			if got := reviewStuckNotices(t, pool); len(got) != tc.told {
				t.Fatalf("the deciding review told the architect again: %+v", got)
			}
		})
	}
}

// An approval ends the round only while it approves the head's code and the head's checks are
// green. One that cannot - CI settled red at the head, which in reviewing the reviewer's round
// decides, or a push since the approved head may have changed code - leaves the round as stuck as
// no review at all, so the architect is told, once, when the round becomes stuck, by whichever fact
// made it so: the completion, a CI result, or a push classified late. A later CI result that fails
// other checks at the same head leaves it stuck the same way and tells nothing. A round whose checks
// are still running waits for them and tells nobody.
func TestARoundItsApprovalCannotEndTellsTheArchitect(t *testing.T) {
	approve := func(head string) intake.Fact {
		return intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: head, Body: "approved"}
	}
	requestChanges := intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "changes_requested", CommitID: "head-2", Body: "fix the checks"}
	complete := intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Summary: "reviewed", Commit: "review-1"}
	sync := func(head string) intake.Fact {
		return intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: head}
	}
	push := func(from, to, paths string) intake.Fact {
		unforced, whole := "false", "false"
		return intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: from, After: to,
			ChangedPaths: &paths, Truncated: &whole, Forced: &unforced, Pusher: "legion-reviewer[bot]"}
	}
	settle := func(head, verdict string, generation int64, failing ...string) intake.Fact {
		return intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: head, CheckRuns: []record.AttemptRun{{Name: "lint", ID: generation}},
			Generation: generation, Snapshot: fmt.Sprintf("%s-%s-%d", verdict, head, generation), Verdict: verdict, Failing: append([]string{}, failing...)}
	}
	const handoff, code = ".legion/review.json", ".legion/review.json\nsrc/widget.go"
	for _, tc := range []struct {
		name    string
		verdict string
		steps   []intake.Fact
		// told is what the one notice's reason names, and by the fact that wrote it; empty, the
		// architect is told nothing.
		told []string
		by   string
		then intake.Fact
		want phase.Phase
	}{
		{name: "CI settles red on the reviewer's handoff head after it completes, then fails other checks",
			steps: []intake.Fact{approve("head"), sync("head-2"), push("head", "head-2", handoff), complete,
				settle("head-2", "red", 2, "lint"), settle("head-2", "red", 3, "lint", "unit"), settle("head-2", "red", 4, "unit")},
			told: []string{"head-2", "lint"}, by: "a CI result", then: requestChanges, want: phase.Implementing},
		{name: "a push since the approved head changed code", verdict: "green",
			steps: []intake.Fact{approve("head"), sync("head-2"), push("head", "head-2", code), settle("head-2", "green", 2), complete},
			told:  []string{"head-2"}, by: "the reviewer's completion", then: approve("head-2"), want: phase.Retro},
		{name: "the handoff push over a code push is classified after the completion", verdict: "green",
			steps: []intake.Fact{approve("head"), sync("head-2"), push("head", "head-2", code), settle("head-2", "green", 2), sync("head-3"), complete,
				push("head-2", "head-3", handoff)},
			told: []string{"head-3"}, by: "a push", then: approve("head-3"), want: phase.Retro},
		{name: "the approved head's checks are still running",
			steps: []intake.Fact{approve("head"), complete},
			then:  settle("head", "green", 2), want: phase.Retro},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedReviewOf(t, pool, "head", tc.verdict)
			apply := applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))
			for i, fact := range tc.steps {
				if result := apply(fmt.Sprintf("step-%d", i), fact); result.Refusal != nil {
					t.Fatalf("step %d (%T) was refused: %+v", i, fact, result.Refusal)
				}
			}
			if got := issuePhase(t, pool); got != phase.Reviewing {
				t.Fatalf("the issue is in %s, want reviewing", got)
			}
			told := reviewStuckNotices(t, pool)
			if len(tc.told) == 0 && len(told) != 0 {
				t.Fatalf("the architect was told %+v, want nothing while the checks run", told)
			}
			if len(tc.told) != 0 {
				if len(told) != 1 {
					t.Fatalf("the architect was told %d times (%+v), want once", len(told), told)
				}
				for _, want := range tc.told {
					if !strings.Contains(told[0].Reason, want) {
						t.Fatalf("the notice's reason %q does not name %q", told[0].Reason, want)
					}
				}
				if told[0].Summary != tc.by {
					t.Fatalf("the notice was written by %q, want %q", told[0].Summary, tc.by)
				}
			}
			apply("then", tc.then)
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("the issue is in %s, want %s", got, tc.want)
			}
			if got := reviewStuckNotices(t, pool); len(got) != len(told) {
				t.Fatalf("the round's end told the architect again: %+v", got)
			}
		})
	}
}

// reviewStuckNotices is every review-stuck notice the outbox holds, in order.
func reviewStuckNotices(t *testing.T, pool *pgxpool.Pool) []record.Notice {
	t.Helper()
	rows, err := pool.Query(t.Context(), `select coalesce(payload->>'role', ''), coalesce(payload->>'phase', ''), coalesce(payload->>'summary', ''),
		coalesce(payload->>'reason', ''), coalesce(payload->>'topic', '')
		from outbox where kind = 'notice' and payload->>'kind' = 'review-stuck' order by id`)
	if err != nil {
		t.Fatalf("read review-stuck notices: %v", err)
	}
	defer rows.Close()
	notices := []record.Notice{}
	for rows.Next() {
		notice := record.Notice{Kind: "review-stuck"}
		if err := rows.Scan(&notice.Role, &notice.Phase, &notice.Summary, &notice.Reason, &notice.Topic); err != nil {
			t.Fatalf("scan review-stuck notice: %v", err)
		}
		notices = append(notices, notice)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate review-stuck notices: %v", err)
	}
	return notices
}
