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
// implementing, the implementer's task names the failing checks, and the architect is told with
// the same checks. Its next push is then a counted fix attempt, as a red verdict makes any new head.
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
				var rounds int
				if err := pool.QueryRow(context.Background(), "select rounds from phases where issue = 'LEGION-208' and role = 'implementer'").Scan(&rounds); err != nil {
					t.Fatalf("read the implementer's rounds: %v", err)
				}
				if got := noticeKinds(t, pool, "LEGION-208"); containsNotice(got, "checks-red") || !strings.Contains(task, "rename the widget") || strings.Contains(task, want) || rounds != 1 {
					t.Fatalf("task %q, notices %v, rounds %d; want the reviewer's body, no checks-red and the round counted", task, got, rounds)
				}
				return
			}
			var reason string
			if err := pool.QueryRow(context.Background(), "select payload->>'reason' from outbox where kind = 'notice' and payload->>'kind' = 'checks-red'").Scan(&reason); err != nil {
				t.Fatalf("read the architect's checks-red notice: %v", err)
			}
			if !strings.Contains(task, want) || !strings.Contains(reason, want) {
				t.Fatalf("task %q and notice %q; want both to say %q", task, reason, want)
			}
		})
	}
}

// An issue in awaiting_merge has every worker suspended and a READY a human was told to merge. A
// red that then stands for its head by the head's own CI - a required check failing on a rerun,
// or a newly required one - keeps GitHub from merging it, and no worker would hear of it: the
// issue goes back to implementing exactly as a red in testing does, the implementer's task and the
// architect's checks-red notice naming the red checks, the project's merge queue role told the
// READY is withdrawn, and the round returns through testing, review and READY. A head a push that
// changed only .legion/ reached is no exception, unlike in testing or reviewing, where such a
// head's red is a round's to decide: no round is open in awaiting_merge. A red carried to the READY
// head from the code head before it moves nothing: READY found the head's own CI green on GitHub,
// which is what GitHub merges by. Nor does a red only on a check the base branch does not require.
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
// must merge forward. GitHub reports mergeability UNKNOWN until it has computed it - that is "not
// yet known", never a conflict - so an unknown or a mergeable read moves nothing. Outside
// awaiting_merge a conflict moves nothing either: in testing the tester is already at work and
// will see the conflict on GitHub when its pass runs, and in reviewing no round is open to decide
// a conflict the way a red CI verdict is (RedSendsBack decides a red only where a round is open).
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
