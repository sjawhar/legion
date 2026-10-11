package workflow

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A red CI verdict settled on the head while the issue is in testing or reviewing is a regression
// the implementer has to fix: nothing else moves the tree, since the tester's pass and the
// reviewer's decision are both of code CI has now found broken. The issue goes back to
// implementing, the implementer's task names the failing checks, the architect is told with the
// same checks, and the round is counted, as every return to implementing counts one: the
// implementer's completion of this pass is told from its last by it. Its next push is then a
// counted fix attempt, as a red verdict makes any new head.
// A red the review App planned (its failing tests) sends nothing back, and neither does a red in
// implementing, where the implementer is already at work. Nor does a red on a head a handoff-only
// push reached, whose code is the head it replaced: in reviewing that is the reviewer's own
// handoff head, whose settled verdict the reviewer waits for before it decides, so the round is
// left open, its reviewer running, and the red is the round's to decide. And a round that has
// already decided is ended by its decision before the red rule is asked: a request for changes
// sends the implementer the reviewer's body and counts the round, and no checks-red is told. The
// same holds for the code head's own red carried to the handoff head that replaced it
// (classify.SettlementFor): the round decides it, and nothing is sent back.
func TestARedVerdictInTestingOrReviewingSendsTheTreeBackToImplementing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		from        phase.Phase
		status      string
		planned     bool
		handoffHead bool
		// decided seeds the reviewer's row with a request for changes: both halves of the round in.
		decided bool
		want    phase.Phase
		// settles is the commit the red settlement names: the head, or the code head a handoff-only
		// push replaced.
		settles string
		// uncompleted seeds the reviewer's row without its completion: its round still at work.
		uncompleted bool
		// quiesce is the role the implementer's start waits for: the one CI took the phase from
		// before it completed, none when it had completed.
		quiesce claim.Role
	}{
		{"in testing", phase.Testing, "testing", false, false, false, phase.Implementing, "", false, claim.RoleTester},
		{"in reviewing", phase.Reviewing, "needs_review", false, false, false, phase.Implementing, "", false, ""},
		{"in reviewing, the reviewer's round not completed", phase.Reviewing, "needs_review", false, false, false, phase.Implementing, "", true, claim.RoleReviewer},
		{"a planned red in testing", phase.Testing, "testing", true, false, false, phase.Testing, "", false, ""},
		{"in implementing", phase.Implementing, "in_progress", false, false, false, phase.Implementing, "", false, ""},
		{"on the tester's handoff-only head", phase.Testing, "testing", false, true, false, phase.Testing, "", false, ""},
		{"on the reviewer's handoff-only head, its round half in", phase.Reviewing, "needs_review", false, true, false, phase.Reviewing, "", false, ""},
		{"on a code head whose round has decided", phase.Reviewing, "needs_review", false, false, true, phase.Implementing, "", false, ""},
		{"on the code head, carried to the tester's handoff-only head", phase.Testing, "testing", false, true, false, phase.Testing, "code", false, ""},
		{"on the code head, carried to the reviewer's handoff-only head", phase.Reviewing, "needs_review", false, true, false, phase.Reviewing, "code", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			pr := record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", PlannedRed: tc.planned, Required: []string{"python-cli-tests / test (pytest)"}}
			if tc.handoffHead {
				pr.Pushes = []record.ClassifiedPush{{SHA: "head", Before: "code", HandoffOnly: true}}
			}
			seedPR(t, pool, pr)
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			if tc.from == phase.Reviewing {
				reviewer := record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", HandoffCommit: "head", Summary: "reviewed"}
				if tc.uncompleted {
					reviewer.HandoffCommit, reviewer.Summary = "", ""
				}
				if tc.decided {
					reviewer.Decision = &record.ReviewDecision{State: "changes_requested", Body: "rename the widget", Head: "head"}
				}
				seedPhase(t, pool, reviewer)
			}
			engine := testEngine(config.DesignGateRootIssues, nil)
			settles := tc.settles
			if settles == "" {
				settles = "head"
			}
			if result, err := intake.ApplyFact(context.Background(), pool, "github", "red", intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42,
				HeadSHA: settles, CheckRuns: []record.AttemptRun{{Name: "pytest", ID: 7}}, Generation: 1, Snapshot: "red-head",
				Failing: []string{"python-cli-tests / test (pytest)"}}, engine); err != nil || result.Refusal != nil {
				t.Fatalf("apply the red verdict = %+v, %v", result.Refusal, err)
			}
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("after the red verdict the issue is in %s, want %s", got, tc.want)
			}
			if tc.want == tc.from {
				if got := noticeKinds(t, pool, "LEGION-208"); containsNotice(got, "checks-red") {
					t.Fatalf("notices %v, want no checks-red", got)
				}
				assertOutboxCount(t, pool, "supervise", 0)
				if rounds := implementerRounds(t, pool); rounds != 0 {
					t.Fatalf("rounds = %d, want none counted: nothing went back", rounds)
				}
				if tc.settles != "" {
					var verdict string
					seedRecord(t, pool, func(tx pgx.Tx) error {
						pr, err := record.NewStore().PullRequest(t.Context(), tx, "LEGION-208")
						if err == nil {
							verdict = classify.HeadVerdict(*pr) + " of " + pr.CheckedHead
						}
						return err
					})
					if want := "red of " + tc.settles; verdict != want {
						t.Fatalf("the head's verdict is %q, want the carried %q", verdict, want)
					}
				}
				return
			}
			var task, quiesce string
			if err := pool.QueryRow(context.Background(), "select payload->>'task', coalesce(payload->>'quiesce', '') from outbox where kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = 'implementer'").Scan(&task, &quiesce); err != nil {
				t.Fatalf("read the implementer's start: %v", err)
			}
			if claim.Role(quiesce) != tc.quiesce {
				t.Fatalf("the implementer's start waits for %q, want %q", quiesce, tc.quiesce)
			}
			want := "CI is red at head: python-cli-tests / test (pytest)"
			if tc.decided {
				rounds := implementerRounds(t, pool)
				if got := noticeKinds(t, pool, "LEGION-208"); containsNotice(got, "checks-red") || !strings.Contains(task, "rename the widget") || strings.Contains(task, want) || rounds != 1 {
					t.Fatalf("task %q, notices %v, rounds %d; want the reviewer's body, no checks-red and the round counted", task, got, rounds)
				}
				return
			}
			var reason string
			if err := pool.QueryRow(context.Background(), "select payload->>'reason' from outbox where kind = 'notice' and payload->>'kind' = 'checks-red'").Scan(&reason); err != nil {
				t.Fatalf("read the architect's checks-red notice: %v", err)
			}
			if rounds := implementerRounds(t, pool); !strings.Contains(task, want) || !strings.Contains(reason, want) || rounds != 1 {
				t.Fatalf("task %q, notice %q and rounds %d; want both to say %q and the round counted", task, reason, rounds, want)
			}
		})
	}
}

// An issue in awaiting_merge has every worker suspended and a READY a human was told to merge. A
// red that then stands for its head by the head's own CI - a required check failing on a rerun,
// or a newly required one - keeps GitHub from merging it, and no worker would hear of it: the
// issue goes back to implementing exactly as a red in testing does, the implementer's task and the
// architect's checks-red notice naming the red checks, the project's merge queue role told the
// READY is withdrawn, the return counted as a round, and the round returns through testing, review
// and READY. A head a push that changed only .legion/ reached is no exception, unlike in testing
// or reviewing, where such a head's red is a round's to decide: no round is open in
// awaiting_merge. A red carried to the READY head from the code head before it moves nothing:
// READY found the head's own CI green on GitHub, which is what GitHub merges by. Nor does a red
// only on a check the base branch does not require.
func TestARedVerdictInAwaitingMergeSendsTheTreeBackToImplementing(t *testing.T) {
	const required = "pr-checks-result"
	for _, tc := range []struct {
		name        string
		handoffHead bool
		// settles is the commit the red settlement names: the head, or the code head a handoff-only
		// push replaced.
		settles string
		failing string
		want    phase.Phase
	}{
		{"on the READY head", false, "head", required, phase.Implementing},
		{"on a READY head a handoff-only push reached", true, "head", required, phase.Implementing},
		{"on the code head, carried to the READY head", true, "code", required, phase.AwaitingMerge},
		{"only on a check the base branch does not require", false, "head", "review", phase.AwaitingMerge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "U"})
			pr := record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Required: []string{required}}
			if tc.handoffHead {
				pr.Pushes = []record.ClassifiedPush{{SHA: "head", Before: "code", HandoffOnly: true}}
			}
			seedPR(t, pool, pr)
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "head", Summary: "READY #42 at head"})
			applyRefusingNothing(t, pool, readyEngine("merge-queue"), intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42,
				HeadSHA: tc.settles, CheckRuns: []record.AttemptRun{{Name: required, ID: 1}, {Name: "review", ID: 2}}, Generation: 1, Snapshot: "red",
				Failing: []string{tc.failing}})
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("after the red verdict the issue is in %s, want %s", got, tc.want)
			}
			published := mergeQueuePublishes(t, pool)
			if tc.want == phase.AwaitingMerge {
				if got := noticeKinds(t, pool, "LEGION-208"); len(got) != 0 {
					t.Fatalf("notices %v, want none", got)
				}
				assertOutboxCount(t, pool, "supervise", 0)
				assertOutboxCount(t, pool, "dispatch_status", 0)
				if len(published) != 0 {
					t.Fatalf("merge queue publishes %v, want none: the READY stands", published)
				}
				if rounds := implementerRounds(t, pool); rounds != 0 {
					t.Fatalf("rounds = %d, want none counted: nothing went back", rounds)
				}
				return
			}
			want := "CI is red at head: " + required
			var reason, from, status string
			if err := pool.QueryRow(context.Background(), "select payload->>'reason', payload->>'phase' from outbox where kind = 'notice' and payload->>'kind' = 'checks-red'").Scan(&reason, &from); err != nil {
				t.Fatalf("read the architect's checks-red notice: %v", err)
			}
			if err := pool.QueryRow(context.Background(), "select payload->>'status' from outbox where kind = 'dispatch_status'").Scan(&status); err != nil {
				t.Fatalf("read the status write: %v", err)
			}
			if task := implementerTask(t, pool); !strings.Contains(task, want) || !strings.Contains(reason, want) || from != string(phase.AwaitingMerge) || status != "in_progress" {
				t.Fatalf("task %q, notice %q from %q, status %q; want both to say %q, from awaiting_merge, and the issue in_progress", task, reason, from, status, want)
			}
			if rounds := implementerRounds(t, pool); rounds != 1 {
				t.Fatalf("rounds = %d, want the return to implementing counted", rounds)
			}
			if len(published) != 1 || published[0].Role != "merge-queue" || !strings.Contains(published[0].Packet, "READY withdrawn") || !strings.Contains(published[0].Packet, want) {
				t.Fatalf("merge queue publishes %v, want one to merge-queue withdrawing the READY and saying %q", published, want)
			}
		})
	}
}

// A READY is posted on the Dispatch issue whether or not the project names a merge queue role, so
// its withdrawal is too: with no role, the issue's own record would otherwise keep telling a human
// to merge a head GitHub will not merge. With a role, the role is told as well.
func TestAREADYWithdrawalIsPostedOnTheIssueWithOrWithoutAMergeQueueRole(t *testing.T) {
	const required = "pr-checks-result"
	for _, role := range []string{"", "merge-queue"} {
		t.Run("merge queue role "+strconv.Quote(role), func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Required: []string{required}})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			applyRefusingNothing(t, pool, readyEngine(role), intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42,
				HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: required, ID: 1}}, Generation: 1, Snapshot: "red", Failing: []string{required}})
			want := "READY withdrawn for LEGION-208, pull request #42: CI is red at head: " + required
			if posted := messageBodies(t, pool); len(posted) != 1 || !strings.Contains(posted[0], want) {
				t.Fatalf("Dispatch messages %q, want one saying %q", posted, want)
			}
			if published := mergeQueuePublishes(t, pool); (role == "") != (len(published) == 0) {
				t.Fatalf("merge queue publishes %v with role %q, want one exactly when a role is named", published, role)
			}
		})
	}
}

// A settlement reads no workflow run, and the run result it would judge a READY head by is as old
// as the pass's last read of the runs: a short re-run can turn the run green on GitHub, and READY
// pass on it, before the next read. So a settlement that finds the READY head red only by a
// required workflow withdraws nothing, and the pass's next read of the runs decides: a run still
// red withdraws the READY then, and one that passed leaves it standing.
func TestASettlementLeavesARedOnlyRequiredWorkflowsMakeToTheNextRead(t *testing.T) {
	const required, workflow = "pr-checks-result", ".github/workflows/claude-pr-review.yml"
	for _, tc := range []struct {
		name string
		read string
		want phase.Phase
	}{
		{"the next read finds the re-run passed", classify.Success, phase.AwaitingMerge},
		{"the next read finds the run still red", "failure", phase.Implementing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			engine := readyEngine("merge-queue")
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "U"})
			// The pass last read the run failed at the READY head; it has been re-run since.
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Required: []string{required},
				Workflows: []record.RequiredWorkflow{{Path: workflow, Result: "failure"}}, WorkflowsHead: "head"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			applyRefusingNothing(t, pool, engine, intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head",
				CheckRuns: []record.AttemptRun{{Name: required, ID: 1}}, Generation: 1, Snapshot: "re-run", Failing: []string{}})
			if got := issuePhase(t, pool); got != phase.AwaitingMerge {
				t.Fatalf("after the settlement the issue is in %s, want awaiting_merge", got)
			}
			if published, posted := mergeQueuePublishes(t, pool), messageBodies(t, pool); len(published) != 0 || len(posted) != 0 {
				t.Fatalf("after the settlement: publishes %v, messages %q; want the READY standing", published, posted)
			}
			applyRefusingNothing(t, pool, engine, intake.RequiredChecks{Repo: "sjawhar/legion", Number: 42, Names: []string{required},
				Workflows: []record.RequiredWorkflow{{Path: workflow, Result: tc.read}}, WorkflowsHead: "head"})
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("after the next read the issue is in %s, want %s", got, tc.want)
			}
			if published := mergeQueuePublishes(t, pool); (tc.want == phase.Implementing) != (len(published) == 1) {
				t.Fatalf("merge queue publishes %v, want a withdrawal exactly when the read found the run red", published)
			}
		})
	}
}

// A READY pull request that starts conflicting with its base while awaiting_merge is in the same
// bind as one whose own CI turned red: GitHub computes no merge ref for a conflicting head, so it
// runs no checks on it at all, and nothing - not a CI settlement, not a required-checks read -
// would ever tell the daemon the READY cannot be merged. The daemon's own read of GitHub's
// mergeability (workflow's mergeability) is the only path to it, and a conflicting read sends the
// tree back to implementing exactly as RedWithdrawsReady does, naming the base the implementer
// must merge forward, and counts the return as a round, so the implementer's completion of the
// merge is told from its completion of the round before. GitHub reports mergeability UNKNOWN until
// it has computed it - that is "not yet known", never a conflict - so an unknown or a mergeable
// read moves nothing. Outside awaiting_merge a conflict moves nothing either: in testing the
// tester is already at work and will see the conflict on GitHub when its pass runs, and in
// reviewing no round is open to decide a conflict the way a red CI verdict is (RedSendsBack
// decides a red only where a round is open).
func TestAConflictingMergeabilityInAwaitingMergeSendsTheTreeBackToImplementing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		from      phase.Phase
		status    string
		mergeable record.Mergeability
		want      phase.Phase
	}{
		{"conflicting in awaiting_merge", phase.AwaitingMerge, "retro", record.MergeabilityConflicting, phase.Implementing},
		{"unknown in awaiting_merge: GitHub has not computed it yet", phase.AwaitingMerge, "retro", record.MergeabilityUnknown, phase.AwaitingMerge},
		{"mergeable in awaiting_merge", phase.AwaitingMerge, "retro", record.MergeabilityMergeable, phase.AwaitingMerge},
		{"conflicting in testing: the tester is already at work", phase.Testing, "testing", record.MergeabilityConflicting, phase.Testing},
		{"conflicting in reviewing: no round is open to decide it", phase.Reviewing, "needs_review", record.MergeabilityConflicting, phase.Reviewing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			if tc.from == phase.Reviewing {
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", HandoffCommit: "head", Summary: "reviewed"})
			}
			if tc.from == phase.AwaitingMerge {
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "head", Summary: "READY #42 at head"})
			}
			applyRefusingNothing(t, pool, readyEngine("merge-queue"),
				intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", Mergeable: tc.mergeable})
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("after the mergeability read the issue is in %s, want %s", got, tc.want)
			}
			published := mergeQueuePublishes(t, pool)
			if tc.want != phase.Implementing {
				if len(published) != 0 {
					t.Fatalf("merge queue publishes %v, want none: nothing moved", published)
				}
				assertOutboxCount(t, pool, "supervise", 0)
				if rounds := implementerRounds(t, pool); rounds != 0 {
					t.Fatalf("rounds = %d, want none counted: nothing went back", rounds)
				}
				return
			}
			want := "the head conflicts with main: GitHub runs no checks on it; merge main forward"
			var reason, from, status string
			if err := pool.QueryRow(context.Background(), "select payload->>'reason', payload->>'phase' from outbox where kind = 'notice' and payload->>'kind' = 'checks-red'").Scan(&reason, &from); err != nil {
				t.Fatalf("read the architect's checks-red notice: %v", err)
			}
			if err := pool.QueryRow(context.Background(), "select payload->>'status' from outbox where kind = 'dispatch_status'").Scan(&status); err != nil {
				t.Fatalf("read the status write: %v", err)
			}
			if task := implementerTask(t, pool); !strings.Contains(task, want) || !strings.Contains(reason, want) || from != string(phase.AwaitingMerge) || status != "in_progress" {
				t.Fatalf("task %q, notice %q from %q, status %q; want both to say %q, from awaiting_merge, and the issue in_progress", task, reason, from, status, want)
			}
			if rounds := implementerRounds(t, pool); rounds != 1 {
				t.Fatalf("rounds = %d, want the return to implementing counted", rounds)
			}
			if len(published) != 1 || published[0].Role != "merge-queue" || !strings.Contains(published[0].Packet, "READY withdrawn") || !strings.Contains(published[0].Packet, want) {
				t.Fatalf("merge queue publishes %v, want one to merge-queue withdrawing the READY and saying %q", published, want)
			}
		})
	}
}

// A conflict the required-checks poll records before the issue reaches awaiting_merge - in
// merging, where the merger pushes nothing on its way to READY - is stored but decides nothing
// there: no worker is suspended yet, and no round is open to act on it. The merger's READY still
// posts, since GitHub runs no checks at all on a conflicting head for its own required-check read
// to find red, so the issue enters awaiting_merge with CONFLICTING already on the record. Deciding
// only on a read that changes the stored value would leave that conflict permanently unacted on -
// every later read of the same CONFLICTING answer "changes" nothing - so the tree would still wait
// forever, reached in a different order than a conflict that starts after READY. The handler must
// decide on every CONFLICTING read, whether or not the record already said so: the first read
// after the issue reaches awaiting_merge withdraws the READY, and the transition itself, which
// takes the issue out of awaiting_merge, is what stops a further repeat of the same read from
// doing it again.
func TestAConflictRecordedBeforeAwaitingMergeIsWithdrawnOnTheFirstReadAfterReady(t *testing.T) {
	pool := migratedPool(t)
	engine := testEngine(config.DesignGateRootIssues, nil)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 1, ApprovedVersion: new(1)})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})

	conflict := intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", Mergeable: record.MergeabilityConflicting}
	applyRefusingNothing(t, pool, engine, conflict)
	if got := issuePhase(t, pool); got != phase.Merging {
		t.Fatalf("after the first conflicting read the issue is in %s, want merging: nothing decides it before awaiting_merge", got)
	}

	applyRefusingNothing(t, pool, engine, intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Ready: true, Summary: readyPacket, Commit: "head"})
	if got := issuePhase(t, pool); got != phase.AwaitingMerge {
		t.Fatalf("after READY the issue is in %s, want awaiting_merge: a conflict already on the record does not refuse it", got)
	}

	applyRefusingNothing(t, pool, engine, conflict)
	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("after the same conflicting read again the issue is in %s, want implementing: the record did not change, but the decision must still run on this first read in awaiting_merge", got)
	}
	want := "the head conflicts with main: GitHub runs no checks on it; merge main forward"
	var reason string
	var notices int
	if err := pool.QueryRow(context.Background(), "select count(*) from outbox where kind = 'notice' and payload->>'kind' = 'checks-red'").Scan(&notices); err != nil {
		t.Fatalf("count the architect's checks-red notices: %v", err)
	}
	if err := pool.QueryRow(context.Background(), "select payload->>'reason' from outbox where kind = 'notice' and payload->>'kind' = 'checks-red'").Scan(&reason); err != nil {
		t.Fatalf("read the architect's checks-red notice: %v", err)
	}
	if notices != 1 || !strings.Contains(reason, want) {
		t.Fatalf("checks-red notices = %d, reason %q; want exactly one saying %q", notices, reason, want)
	}
}

// At max_fix_attempts the daemon stops sending the work back from testing or reviewing on CI red
// within the attempt the count last counted, and the architect decides what comes next
// (skill://legion-architect's pr-blocked row): the red verdict that finds the count exhausted is
// posted on the issue and told to the architect once, and the issue stays in its phase. Every later
// red at that head stays there too, and tells nothing new: a re-run that fails again, a red that
// returns after the head read green (a flaky check flapping), and, in reviewing, the reviewer's
// completion of its round, which asks the round what the red comes to. A cap that held the first
// red and let the next one through would bound nothing. A fix pushed onto the red raises the count,
// and its red is told again, and held the same way.
func TestAnExhaustedFixAttemptCountHoldsEveryRedAtThatCount(t *testing.T) {
	const required = "pr-checks-result"
	settlement := func(attempt int, failing ...string) intake.Fact {
		return intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head",
			CheckRuns: []record.AttemptRun{{Name: required, ID: int64(attempt)}}, Generation: int64(attempt),
			Snapshot: "settled-" + strconv.Itoa(attempt), Failing: append([]string{}, failing...)}
	}
	completion := intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Commit: "review-1", Summary: "reviewed"}
	for _, tc := range []struct {
		name   string
		from   phase.Phase
		status string
		// completed seeds the reviewer's row with its completion: its round half in.
		completed bool
		// attempts and reported seed the pull request's fix attempts and the count last told.
		attempts, reported int
		facts              []intake.Fact
	}{
		{"in testing, red twice", phase.Testing, "testing", false, 3, 0, []intake.Fact{settlement(1, required), settlement(2, required)}},
		{"in testing, red, green, red", phase.Testing, "testing", false, 3, 0, []intake.Fact{settlement(1, required), settlement(2), settlement(3, required)}},
		{"in testing, red twice at a count past the one told", phase.Testing, "testing", false, 4, 3, []intake.Fact{settlement(1, required), settlement(2, required)}},
		{"in reviewing, red twice", phase.Reviewing, "needs_review", true, 3, 0, []intake.Fact{settlement(1, required), settlement(2, required)}},
		{"in reviewing, red, green, red", phase.Reviewing, "needs_review", true, 3, 0, []intake.Fact{settlement(1, required), settlement(2), settlement(3, required)}},
		{"in reviewing, red, then the reviewer completes", phase.Reviewing, "needs_review", false, 3, 0, []intake.Fact{settlement(1, required), completion}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Required: []string{required},
				FixAttempts: tc.attempts, BlockedAttempts: tc.reported, HeadCounted: "head"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			if tc.from == phase.Reviewing {
				reviewer := record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"}
				if tc.completed {
					reviewer.HandoffCommit, reviewer.Summary = "head", "reviewed"
				}
				seedPhase(t, pool, reviewer)
			}
			engine := readyEngine("merge-queue")
			for i, fact := range tc.facts {
				applyRefusingNothing(t, pool, engine, fact)
				if got := issuePhase(t, pool); got != tc.from {
					t.Fatalf("after fact %d (%T) the issue is in %s, want it held in %s at the exhausted count", i+1, fact, got, tc.from)
				}
			}
			blocked := 0
			for _, kind := range noticeKinds(t, pool, "LEGION-208") {
				switch kind {
				case "pr-blocked":
					blocked++
				case "checks-red":
					t.Fatalf("the architect was told checks-red, want nothing sent back at the exhausted count")
				}
			}
			if blocked != 1 {
				t.Fatalf("pr-blocked notices = %d, want one for the exhausted count", blocked)
			}
			if posted := messageBodies(t, pool); len(posted) != 1 || !strings.Contains(posted[0], "reached max_fix_attempts=3") {
				t.Fatalf("Dispatch messages %q, want the one max_fix_attempts message", posted)
			}
			assertOutboxCount(t, pool, "supervise", 0)
			if rounds := implementerRounds(t, pool); rounds != 0 {
				t.Fatalf("rounds = %d, want none: nothing went back", rounds)
			}
		})
	}
}

// The cap withholds the daemon's own move on a red, not a decision: at an exhausted count a
// reviewer's request for changes still ends its round and sends the work back to implementing,
// whichever of its two halves - the review on GitHub and the reviewer's completion - arrives
// second. The return is the review's, so the implementer is told the reviewer's body and the round
// is counted against review_round_cap, and the architect hears no checks-red.
func TestAnExhaustedFixAttemptCountStillTakesTheReviewersRequestForChanges(t *testing.T) {
	const required = "pr-checks-result"
	red := intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head",
		CheckRuns: []record.AttemptRun{{Name: required, ID: 1}}, Generation: 1, Snapshot: "settled-1", Failing: []string{required}}
	review := intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "changes_requested", CommitID: "head", HeadSHA: "head",
		Author: testReviewApp, Body: "rename the widget"}
	completion := intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Commit: "review-1", Summary: "reviewed"}
	for _, tc := range []struct {
		name  string
		facts []intake.Fact
	}{
		{"the review, then the completion", []intake.Fact{red, review, completion}},
		{"the completion, then the review", []intake.Fact{red, completion, review}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Required: []string{required}, FixAttempts: 3, HeadCounted: "head"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim"})
			engine := readyEngine("merge-queue")
			for i, fact := range tc.facts[:2] {
				applyRefusingNothing(t, pool, engine, fact)
				if got := issuePhase(t, pool); got != phase.Reviewing {
					t.Fatalf("after fact %d (%T) the issue is in %s, want reviewing: the round is not decided yet", i+1, fact, got)
				}
			}
			applyRefusingNothing(t, pool, engine, tc.facts[2])
			if got := issuePhase(t, pool); got != phase.Implementing {
				t.Fatalf("after the round's second half the issue is in %s, want implementing: a request for changes is a decision the cap does not withhold", got)
			}
			if task, rounds := implementerTask(t, pool), implementerRounds(t, pool); !strings.Contains(task, "rename the widget") || rounds != 1 {
				t.Fatalf("implementer task %q, rounds %d; want the reviewer's body and the round counted", task, rounds)
			}
			if got := noticeKinds(t, pool, "LEGION-208"); containsNotice(got, "checks-red") || !containsNotice(got, "pr-blocked") {
				t.Fatalf("notices %v, want the count's pr-blocked and no checks-red", got)
			}
		})
	}
}

// The cap bounds the loop it counts: pushes onto a red verdict, and every head pushed on top of the
// counted one before a push lands on a green verdict. A code head pushed onto a green one - the
// implementer's answer to a request for changes after a re-run turned the counted head green -
// raises no count and leaves that attempt, so a red there in testing is new and not the loop's: it
// sends the work back with a checks-red, as below the cap, whether or not the count was already
// reported. The next push onto that red raises the count and is reported then. Every other push
// stays in the attempt: a code head pushed while the counted head had no verdict yet, directly or
// over a handoff-only head, and an uncounted push onto the held red - the tester's red tests, its
// handoff-only test.json, the implementer's fix onto the planned red - so the attempt's red is
// held and reports the count once, and no push sequence slips a round past the cap. A red reaching
// a handoff-only head from the counted code head it replaced is the loop's too: it sends nothing
// back (a handoff head never does), and it reports the count once.
//
// GitHub delivers a head's push webhook and its synchronize in either order, and a head whose push
// has not arrived is counted until the push proves handoff-only or the review App's. So every case
// runs in both orders, and both must leave the same counted attempt, the same hold or send-back,
// and the same notices.
func TestAnExhaustedFixAttemptCountHoldsTheCountedAttemptUntilAHeadIsPushedOntoGreen(t *testing.T) {
	const required, implementer = "pr-checks-result", "legion-implementer[bot]"
	// step is a settlement, or a head's arrival: its push webhook and its synchronize, in the order
	// the subtest delivers them.
	type step struct {
		settle                  intake.Fact
		from, to, paths, pusher string
	}
	settled := func(head string, attempt int, failing ...string) step {
		return step{settle: settledAt(head, required, attempt, failing...)}
	}
	arrives := func(from, to, paths, pusher string) step {
		return step{from: from, to: to, paths: paths, pusher: pusher}
	}
	const code, redTests, handoff = "src/widget.go", "src/widget_test.go", ".legion/LEGION-208/test.json"
	for _, tc := range []struct {
		name     string
		reported int
		steps    []step
		// want is the phase the last step leaves the issue in; checksRed and blocked count the
		// architect's notices of each kind; attempts and mark are the pull request's fix attempts and
		// the counted head it is left with.
		want               phase.Phase
		checksRed, blocked int
		attempts           int
		mark               string
	}{
		{"a head pushed onto green after the count was reported", 3,
			[]step{settled("h3", 1, required), settled("h3", 2), arrives("h3", "h4", code, implementer), settled("h4", 1, required)},
			phase.Implementing, 1, 0, 3, ""},
		{"a head pushed onto green after the count went green unreported", 0,
			[]step{settled("h3", 1), arrives("h3", "h4", code, implementer), settled("h4", 1, required)},
			phase.Implementing, 1, 0, 3, ""},
		{"a handoff head over the counted head, the counted head's red carried to it", 0,
			[]step{arrives("h3", "h3t", handoff, testReviewApp), settled("h3", 1, required)},
			phase.Testing, 0, 1, 3, "h3"},
		{"a code head pushed before the counted head was judged", 0,
			[]step{arrives("h3", "h4", code, implementer), settled("h4", 1, required)},
			phase.Testing, 0, 1, 3, "h3"},
		{"a code head pushed over a handoff head before the counted head was judged", 0,
			[]step{arrives("h3", "h3t", ".legion/LEGION-208/implement.json", implementer), arrives("h3t", "h4", code, implementer), settled("h4", 1, required)},
			phase.Testing, 0, 1, 3, "h3"},
		{"the tester's red tests pushed onto the held red, then the implementer's fix", 0,
			[]step{settled("h3", 1, required), arrives("h3", "h4", redTests, testReviewApp), settled("h4", 1, required),
				arrives("h4", "h5", code, implementer), settled("h5", 1, required)},
			phase.Testing, 0, 1, 3, "h3"},
		{"the implementer's fix pushed before CI judged the tester's red tests on the held red", 0,
			[]step{settled("h3", 1, required), arrives("h3", "h4", redTests, testReviewApp), arrives("h4", "h5", code, implementer), settled("h5", 1, required)},
			phase.Testing, 0, 1, 3, "h3"},
		{"the tester's handoff pushed onto the held red, the red carried to it", 0,
			[]step{settled("h3", 1, required), arrives("h3", "h3t", handoff, testReviewApp), settled("h3", 2, required)},
			phase.Testing, 0, 1, 3, "h3"},
	} {
		for _, pushFirst := range []bool{false, true} {
			order := "synchronize first"
			if pushFirst {
				order = "push first"
			}
			t.Run(tc.name+", "+order, func(t *testing.T) {
				var facts []intake.Fact
				for _, s := range tc.steps {
					if s.to == "" {
						facts = append(facts, s.settle)
						continue
					}
					arrival := []intake.Fact{syncedTo(s.to), pushedFromTo(s.from, s.to, s.paths, s.pusher)}
					if pushFirst {
						arrival[0], arrival[1] = arrival[1], arrival[0]
					}
					facts = append(facts, arrival...)
				}
				pool := migratedPool(t)
				seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
					Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
				seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
					Number: 42, Branch: "legion/LEGION-208", HeadSHA: "h3", Required: []string{required},
					FixAttempts: 3, BlockedAttempts: tc.reported, HeadCounted: "h3"})
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
				engine := readyEngine("merge-queue")
				last := len(facts) - 1
				for i, fact := range facts[:last] {
					applyRefusingNothing(t, pool, engine, fact)
					if got := issuePhase(t, pool); got != phase.Testing {
						t.Fatalf("after fact %d (%T) the issue is in %s, want testing", i+1, fact, got)
					}
				}
				applyRefusingNothing(t, pool, engine, facts[last])
				if got := issuePhase(t, pool); got != tc.want {
					t.Fatalf("after the last red the issue is in %s, want %s", got, tc.want)
				}
				checksRed, blocked := 0, 0
				for _, kind := range noticeKinds(t, pool, "LEGION-208") {
					switch kind {
					case "checks-red":
						checksRed++
					case "pr-blocked":
						blocked++
					}
				}
				if checksRed != tc.checksRed || blocked != tc.blocked {
					t.Fatalf("checks-red notices %d and pr-blocked notices %d, want %d and %d", checksRed, blocked, tc.checksRed, tc.blocked)
				}
				var attempts int
				var mark string
				if err := pool.QueryRow(context.Background(), "select fix_attempts, head_counted from pull_requests where issue = 'LEGION-208'").Scan(&attempts, &mark); err != nil {
					t.Fatalf("read the fix attempts: %v", err)
				}
				if attempts != tc.attempts || mark != tc.mark {
					t.Fatalf("fix attempts %d and counted head %q, want %d and %q", attempts, mark, tc.attempts, tc.mark)
				}
			})
		}
	}
}

// A reopened pull request whose head moved while it was closed - commits pushed then reach no
// synchronize - is judged as a synchronize to that head would be. At an exhausted count already
// reported, a reopen past a red counts the push and reports the new count when the reopened head's
// red is held; a reopen past a green leaves the counted attempt, so the reopened head's red is new
// and sends the work back. A reopen at the recorded head carries the counted attempt as it stood.
func TestAReopenedPullRequestJudgesItsNewHeadAsASynchronizeWould(t *testing.T) {
	const required = "pr-checks-result"
	for _, tc := range []struct {
		name string
		// failing is the recorded head's settlement, reopened is the head the reopen names.
		failing  []string
		reopened string
		want     phase.Phase
		// checksRed and blocked count the architect's notices; attempts and mark the pull request's.
		checksRed, blocked int
		attempts           int
		mark               string
	}{
		{"past a red", []string{required}, "h4", phase.Testing, 0, 1, 4, "h4"},
		{"past a green", []string{}, "h4", phase.Implementing, 1, 0, 3, ""},
		{"at the recorded head", []string{required}, "h3", phase.Testing, 0, 0, 3, "h3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestClosed, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "h3", CheckedHead: "h3", Failing: tc.failing,
				CheckRuns: []record.AttemptRun{{Name: required, ID: 1}}, Required: []string{required},
				FixAttempts: 3, BlockedAttempts: 3, HeadCounted: "h3"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			engine := readyEngine("merge-queue")
			applyRefusingNothing(t, pool, engine, intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208",
				HeadSHA: tc.reopened, Reopened: true})
			applyRefusingNothing(t, pool, engine, settledAt(tc.reopened, required, 2, required))
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("after the reopened head's red the issue is in %s, want %s", got, tc.want)
			}
			checksRed, blocked := 0, 0
			for _, kind := range noticeKinds(t, pool, "LEGION-208") {
				switch kind {
				case "checks-red":
					checksRed++
				case "pr-blocked":
					blocked++
				}
			}
			var attempts int
			var mark string
			if err := pool.QueryRow(context.Background(), "select fix_attempts, head_counted from pull_requests where issue = 'LEGION-208'").Scan(&attempts, &mark); err != nil {
				t.Fatalf("read the fix attempts: %v", err)
			}
			if checksRed != tc.checksRed || blocked != tc.blocked || attempts != tc.attempts || mark != tc.mark {
				t.Fatalf("checks-red %d, pr-blocked %d, fix attempts %d, counted head %q; want %d, %d, %d and %q",
					checksRed, blocked, attempts, mark, tc.checksRed, tc.blocked, tc.attempts, tc.mark)
			}
		})
	}
}

// In awaiting_merge no implementer loops for the fix-attempt cap to bound, and GitHub will not merge
// a red head, so a red there withdraws the READY and sends the work back whatever the count: never
// held, never left standing. That holds for a count told in testing before the work went green and
// moved on through review, retro and READY, and for a READY head that is itself the head the count
// last counted. The withdrawal is told by its checks-red; a count already reported is not reported
// again.
func TestAnExhaustedFixAttemptCountNeverHoldsARedInAwaitingMerge(t *testing.T) {
	const required, implementer = "pr-checks-result", "legion-implementer[bot]"
	tester := intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim", Commit: "test-1", Summary: "passed", Verdict: "pass"}
	approval := intake.PullRequestReview{Repo: "sjawhar/legion", Number: 42, State: "approved", CommitID: "h3", HeadSHA: "h3", Author: testReviewApp, Body: "approved"}
	reviewer := intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", Commit: "review-1", Summary: "reviewed"}
	retro := intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Commit: "retro-1", Summary: "retro"}
	ready := intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", Ready: true, Summary: readyPacket, Commit: "h4"}
	for _, tc := range []struct {
		name string
		from phase.Phase
		// steps carry the issue to awaiting_merge, each leaving it in the phase named beside it.
		steps []struct {
			fact  intake.Fact
			phase phase.Phase
		}
		red intake.Fact
	}{
		{"a count told in testing, then green, review, retro and READY", phase.Testing, []struct {
			fact  intake.Fact
			phase phase.Phase
		}{
			{settledAt("h3", required, 1, required), phase.Testing},
			{settledAt("h3", required, 2), phase.Testing},
			{tester, phase.Reviewing},
			{approval, phase.Reviewing},
			{reviewer, phase.Retro},
			{syncedTo("h4"), phase.Retro},
			{pushedFromTo("h3", "h4", "docs/solutions/legion/widget.md", implementer), phase.Retro},
			{settledAt("h4", required, 1), phase.Retro},
			{retro, phase.Merging},
			{ready, phase.AwaitingMerge},
		}, settledAt("h4", required, 2, required)},
		{"a READY head the count last counted", phase.AwaitingMerge, nil, settledAt("h3", required, 1, required)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			status := map[phase.Phase]string{phase.Testing: "testing", phase.AwaitingMerge: "retro"}[tc.from]
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: status, Rank: "U"})
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 1, ApprovedVersion: new(1)})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "h3", Required: []string{required}, FixAttempts: 3, HeadCounted: "h3"})
			for role, token := range map[claim.Role]claim.Token{claim.RoleImplementer: "implement-claim", claim.RoleTester: "test-claim", claim.RoleReviewer: "review-claim", claim.RoleMerger: "merge-claim"} {
				row := record.PhaseRow{Issue: "LEGION-208", Role: role, Claim: token}
				if tc.from == phase.AwaitingMerge && role == claim.RoleMerger {
					row.HandoffCommit, row.Summary = "h3", "READY #42 at h3"
				}
				seedPhase(t, pool, row)
			}
			engine := readyEngine("merge-queue")
			for i, step := range tc.steps {
				applyRefusingNothing(t, pool, engine, step.fact)
				if got := issuePhase(t, pool); got != step.phase {
					t.Fatalf("after step %d (%T) the issue is in %s, want %s", i+1, step.fact, got, step.phase)
				}
			}
			applyRefusingNothing(t, pool, engine, tc.red)
			if got := issuePhase(t, pool); got != phase.Implementing {
				t.Fatalf("after the red at the READY head the issue is in %s, want implementing: a READY never stands on red", got)
			}
			checksRed, blocked := 0, 0
			for _, kind := range noticeKinds(t, pool, "LEGION-208") {
				switch kind {
				case "checks-red":
					checksRed++
				case "pr-blocked":
					blocked++
				}
			}
			wantBlocked := 0
			if tc.from == phase.Testing {
				wantBlocked = 1
			}
			if checksRed != 1 || blocked != wantBlocked {
				t.Fatalf("checks-red notices %d and pr-blocked notices %d, want 1 and %d", checksRed, blocked, wantBlocked)
			}
			withdrawn := 0
			for _, published := range mergeQueuePublishes(t, pool) {
				if strings.Contains(published.Packet, "READY withdrawn") {
					withdrawn++
				}
			}
			if withdrawn != 1 {
				t.Fatalf("READY withdrawals published %d, want 1", withdrawn)
			}
		})
	}
}

// settledAt is a CI settlement of head on the one check required, attempt numbering its run and its
// fence, failing the checks named.
func settledAt(head, required string, attempt int, failing ...string) intake.Fact {
	return intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: head,
		CheckRuns: []record.AttemptRun{{Name: required, ID: int64(attempt)}}, Generation: int64(attempt),
		Snapshot: head + "-" + strconv.Itoa(attempt), Failing: append([]string{}, failing...)}
}

// syncedTo is GitHub's synchronize of the pull request to head.
func syncedTo(head string) intake.Fact {
	return intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: head}
}

// pushedFromTo is the branch push that replaced from with to, changing paths (newline-separated),
// by pusher.
func pushedFromTo(from, to, paths, pusher string) intake.Fact {
	unforced, whole := "false", "false"
	return intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: from, After: to,
		ChangedPaths: &paths, Truncated: &whole, Forced: &unforced, Pusher: pusher}
}

// Checks can flap: red, then green on a re-run, then red again. Below the cap the first red sends
// the work back, counting one round and starting the implementer once; the issue is then in
// implementing, where no CI verdict moves it, so the green and the second red open no second
// round, deliver no second task and withdraw no READY twice.
func TestAFlappingVerdictBelowTheCapSendsTheWorkBackOnce(t *testing.T) {
	const required = "pr-checks-result"
	for _, tc := range []struct {
		from   phase.Phase
		status string
	}{
		{phase.Testing, "testing"},
		{phase.AwaitingMerge, "retro"},
	} {
		t.Run("from "+string(tc.from), func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Required: []string{required}, FixAttempts: 1, HeadCounted: "head"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			if tc.from == phase.AwaitingMerge {
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "head", Summary: "READY #42 at head"})
			}
			engine := readyEngine("merge-queue")
			for attempt, failing := range [][]string{{required}, {}, {required}} {
				applyRefusingNothing(t, pool, engine, intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head",
					CheckRuns: []record.AttemptRun{{Name: required, ID: int64(attempt + 1)}}, Generation: int64(attempt + 1),
					Snapshot: "settled-" + strconv.Itoa(attempt+1), Failing: failing})
				if got := issuePhase(t, pool); got != phase.Implementing {
					t.Fatalf("after verdict %d the issue is in %s, want implementing", attempt+1, got)
				}
			}
			if rounds := implementerRounds(t, pool); rounds != 1 {
				t.Fatalf("rounds = %d, want the one return counted", rounds)
			}
			assertOutboxCount(t, pool, "supervise", 1)
			checksRed := 0
			for _, kind := range noticeKinds(t, pool, "LEGION-208") {
				if kind == "checks-red" {
					checksRed++
				}
			}
			if checksRed != 1 {
				t.Fatalf("checks-red notices = %d, want one", checksRed)
			}
			want := 0
			if tc.from == phase.AwaitingMerge {
				want = 1
			}
			if published := mergeQueuePublishes(t, pool); len(published) != want {
				t.Fatalf("merge queue publishes %v, want %d withdrawal", published, want)
			}
		})
	}
}
