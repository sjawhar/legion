package workflow

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A red CI verdict that stands for the head is a regression the implementer has to fix, whatever
// phase before READY the issue is in: in testing, reviewing, retro and merging the tester's pass,
// the reviewer's decision and the merger's READY are all of code CI has now found broken. The issue
// goes back to implementing, the implementer's task names the failing checks, the architect is
// told with the same checks in a checks-red notice naming the phase the red stopped, and the round
// is counted, as every return to implementing counts one: the implementer's completion of this pass
// is told from its last by it. The push that answers it is counted nowhere: no head is tallied as
// a fix attempt any more, and a red that stands sends the work back however many came before it
// (contract 56). The implementer's start waits for nobody: the role CI leaves with no phase - the
// tester, the reviewer whether or not its round is completed, the merger, and the implementer
// itself in retro - keeps its turn, its jobs, its process and its session, and is told its phase
// ended by a message on its own topic. Leaving merging also voids a READY the design
// gate refused there (ReadyPending).
//
// The CI rule reads nothing of the review round: a round whose request for changes is already in
// is sent back by the red all the same, with the red named rather than the reviewer's body. A
// settlement stands only for the head it names: a red of the code head a push replaced sends
// nothing back. A red the review App planned (its failing tests) sends nothing back, and
// neither does a red in implementing, where the implementer is already at work. Production clears
// the planned mark at the next code-changing push by anyone but the review App
// (classify.AdvancePullRequestHead), which retro's own push is, so the planned rows in retro and
// merging guard the predicate rather than a live path.
func TestARedVerdictSendsTheTreeBackToImplementing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		from    phase.Phase
		status  string
		planned bool
		// decided seeds the reviewer's row with a request for changes: both halves of the round in.
		decided bool
		want    phase.Phase
		// settles is the commit the red settlement names: the head, or the code head a push
		// replaced.
		settles string
		// uncompleted seeds the reviewer's row without its completion: its round still at work.
		uncompleted bool
		// readyPending seeds a READY the design gate refused in merging, which leaving merging voids.
		readyPending bool
	}{
		{name: "in testing", from: phase.Testing, status: "testing", want: phase.Implementing},
		{name: "in reviewing", from: phase.Reviewing, status: "needs_review", want: phase.Implementing},
		{name: "in reviewing, the reviewer's round not completed", from: phase.Reviewing, status: "needs_review", want: phase.Implementing, uncompleted: true},
		{name: "a planned red in testing", from: phase.Testing, status: "testing", planned: true, want: phase.Testing},
		{name: "in implementing", from: phase.Implementing, status: "in_progress", want: phase.Implementing},
		{name: "on a code head whose round has a request for changes in", from: phase.Reviewing, status: "needs_review", decided: true, want: phase.Implementing},
		{name: "on the code head a push replaced, in testing", from: phase.Testing, status: "testing", want: phase.Testing, settles: "code"},
		{name: "on the code head a push replaced, in reviewing", from: phase.Reviewing, status: "needs_review", want: phase.Reviewing, settles: "code"},
		{name: "in retro", from: phase.Retro, status: "retro", want: phase.Implementing},
		{name: "a planned red in retro", from: phase.Retro, status: "retro", planned: true, want: phase.Retro},
		{name: "in merging", from: phase.Merging, status: "retro", want: phase.Implementing, readyPending: true},
		{name: "a planned red in merging", from: phase.Merging, status: "retro", planned: true, want: phase.Merging, readyPending: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			issue := record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"}
			if tc.readyPending {
				issue.ReadyPending = &record.ReadyPending{Step: record.DesignStepSpec, Version: 4}
			}
			seedIssue(t, pool, issue)
			pr := record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", PlannedRed: tc.planned, Required: []string{"python-cli-tests / test (pytest)"}}
			seedPR(t, pool, pr)
			// The implementer's row is as it stands in each phase: its handoff cleared when it
			// entered retro, so no completion is recorded there either.
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			if tc.from == phase.Reviewing {
				reviewer := record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", HandoffRecord: "review-1", HandoffVersion: 1, Summary: "reviewed"}
				if tc.uncompleted {
					reviewer.HandoffRecord, reviewer.HandoffVersion, reviewer.Summary = "", 0, ""
				}
				if tc.decided {
					reviewer.Decision = &record.ReviewDecision{State: "changes_requested", Body: "rename the widget", Head: "head"}
				}
				seedPhase(t, pool, reviewer)
			}
			if tc.from == phase.Merging {
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})
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
			var pending *int
			if err := pool.QueryRow(context.Background(), "select ready_pending_version from issues where key = 'LEGION-208'").Scan(&pending); err != nil {
				t.Fatalf("read the issue's pending READY version: %v", err)
			}
			if tc.want == tc.from {
				if got := noticeKinds(t, pool, "LEGION-208"); containsNotice(got, "checks-red") {
					t.Fatalf("notices %v, want no checks-red", got)
				}
				assertOutboxCount(t, pool, "supervise", 0)
				if rounds := implementerRounds(t, pool); rounds != 0 {
					t.Fatalf("rounds = %d, want none counted: nothing went back", rounds)
				}
				if (pending != nil) != tc.readyPending {
					t.Fatalf("the issue's pending READY version is %v, want it kept as seeded: nothing left merging", pending)
				}
				return
			}
			if pending != nil {
				t.Fatalf("the issue's pending READY version is %d, want none: a READY the gate refused belongs to the merging phase the red ended", *pending)
			}
			task := implementerStart(t, pool)
			reason, noticePhase := checksRedNotice(t, pool)
			want := "CI is red at head: python-cli-tests / test (pytest)"
			rounds := implementerRounds(t, pool)
			if !strings.Contains(task, want) || strings.Contains(task, "rename the widget") || !strings.Contains(reason, want) || noticePhase != tc.from || rounds != 1 {
				t.Fatalf("task %q, notice %q in %q and rounds %d; want both to say %q and nothing of the reviewer's body, the notice in %s, and the round counted", task, reason, noticePhase, rounds, want, tc.from)
			}
		})
	}
}

// A red that sends the work back tells the role whose phase it ended, on that role's own Envoy
// topic, that the phase is over and which phase the issue is in now (record.PhaseEnded): the tester
// in testing, the reviewer in reviewing whether or not it completed its round, the merger in
// merging, and the implementer in retro, which is told besides that its implementing task follows,
// since the phase the red opens is its own. Nothing else tells any of them: no transition stops a
// worker, and a task the daemon has for a role is delivered only once that role's turn ends. Two
// phases tell nobody: accepting, whose role is the tree's architect, which hears the red in its
// checks-red notice instead, and awaiting_merge, which no role works. The row is the transition's,
// written beside the architect's notice and the implementer's start, and carries the reason that
// notice carries.
func TestARedTellsTheRoleWhosePhaseItEnded(t *testing.T) {
	const reason = "CI is red at head: python-cli-tests / test (pytest)"
	for _, tc := range []struct {
		name   string
		from   phase.Phase
		status string
		// uncompleted seeds the reviewer's row without its completion: its round still at work.
		uncompleted bool
		// want is the message queued for the role whose phase ended; the zero value is none.
		want record.PhaseEnded
	}{
		{name: "the tester in testing", from: phase.Testing, status: "testing",
			want: record.PhaseEnded{Role: claim.RoleTester, From: phase.Testing, To: phase.Implementing, Reason: reason}},
		{name: "the reviewer in a round it completed", from: phase.Reviewing, status: "needs_review",
			want: record.PhaseEnded{Role: claim.RoleReviewer, From: phase.Reviewing, To: phase.Implementing, Reason: reason}},
		{name: "the reviewer in a round it did not complete", from: phase.Reviewing, status: "needs_review", uncompleted: true,
			want: record.PhaseEnded{Role: claim.RoleReviewer, From: phase.Reviewing, To: phase.Implementing, Reason: reason}},
		{name: "the merger in merging", from: phase.Merging, status: "retro",
			want: record.PhaseEnded{Role: claim.RoleMerger, From: phase.Merging, To: phase.Implementing, Reason: reason}},
		{name: "the implementer in retro", from: phase.Retro, status: "retro",
			want: record.PhaseEnded{Role: claim.RoleImplementer, From: phase.Retro, To: phase.Implementing, Reason: reason, TaskFollows: true}},
		{name: "the architect in accepting, which its notice tells", from: phase.Accepting, status: "needs_review"},
		{name: "nobody in awaiting_merge, a phase no role works", from: phase.AwaitingMerge, status: "retro"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Required: []string{"python-cli-tests / test (pytest)"}})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			if tc.from == phase.Reviewing {
				reviewer := record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleReviewer, Claim: "review-claim", HandoffCommit: "head", Summary: "reviewed"}
				if tc.uncompleted {
					reviewer.HandoffCommit, reviewer.Summary = "", ""
				}
				seedPhase(t, pool, reviewer)
			}
			if tc.from == phase.Merging || tc.from == phase.AwaitingMerge {
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", HandoffCommit: "head"})
			}
			if result, err := intake.ApplyFact(context.Background(), pool, "github", "red", intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42,
				HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: "pytest", ID: 7}}, Generation: 1, Snapshot: "red-head",
				Failing: []string{"python-cli-tests / test (pytest)"}}, testEngine(config.DesignGateRootIssues, nil)); err != nil || result.Refusal != nil {
				t.Fatalf("apply the red verdict = %+v, %v", result.Refusal, err)
			}
			if got := issuePhase(t, pool); got != phase.Implementing {
				t.Fatalf("after the red verdict the issue is in %s, want implementing", got)
			}
			// The implementer's start is written whatever the red ended, and waits for nothing.
			if task := implementerStart(t, pool); !strings.Contains(task, reason) {
				t.Fatalf("the implementer's task is %q, want the red named in it", task)
			}
			got := phaseEndedMessages(t, pool, "LEGION-208")
			if tc.want == (record.PhaseEnded{}) {
				if len(got) != 0 {
					t.Fatalf("the red out of %s told %+v, want nobody: its role is %q", tc.from, got, RoleFor(tc.from))
				}
				return
			}
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("the red out of %s told %+v, want the one message %+v", tc.from, got, tc.want)
			}
		})
	}
}

// phaseEndedMessages is every phase-ended message the outbox holds for issue, oldest first, decoded
// as the runner decodes it.
func phaseEndedMessages(t *testing.T, pool *pgxpool.Pool, issue string) []record.PhaseEnded {
	t.Helper()
	rows, err := pool.Query(context.Background(), "select id, payload from outbox where kind = 'phase_ended' and issue = $1 order by id", issue)
	if err != nil {
		t.Fatalf("read the phase-ended messages of %s: %v", issue, err)
	}
	defer rows.Close()
	var messages []record.PhaseEnded
	for rows.Next() {
		row := record.OutboxRow{Kind: record.OutboxKindPhaseEnded}
		if err := rows.Scan(&row.ID, &row.Payload); err != nil {
			t.Fatalf("read the phase-ended messages of %s: %v", issue, err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode phase-ended row %d: %v", row.ID, err)
		}
		messages = append(messages, payload.(record.PhaseEnded))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the phase-ended messages of %s: %v", issue, err)
	}
	return messages
}

// An issue in awaiting_merge has no worker with an active phase task and a READY a human was told
// to merge. A red that then stands for its head by the head's own CI — a required check failing on
// a rerun, or a newly required one — keeps GitHub from merging it, so the issue goes back to
// implementing exactly as a red in testing does: the implementer's task and the architect's
// checks-red notice name the red checks, the project's merge queue role is told the READY is
// withdrawn, the return is counted as a round, and the round returns through testing, review and
// READY. A red of the code head before the READY head moves nothing: a settlement stands only for
// the head it names. Nor does a red only on a check the base branch does not require.
func TestARedVerdictInAwaitingMergeSendsTheTreeBackToImplementing(t *testing.T) {
	const required = "pr-checks-result"
	for _, tc := range []struct {
		name string
		// settles is the commit the red settlement names: the head, or the code head a push
		// replaced.
		settles string
		failing string
		want    phase.Phase
	}{
		{"on the READY head", "head", required, phase.Implementing},
		{"on the code head before the READY head", "code", required, phase.AwaitingMerge},
		{"only on a check the base branch does not require", "head", "review", phase.AwaitingMerge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "U"})
			pr := record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Required: []string{required}}
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
			reason, from := checksRedNotice(t, pool)
			var status string
			if err := pool.QueryRow(context.Background(), "select payload->>'status' from outbox where kind = 'dispatch_status'").Scan(&status); err != nil {
				t.Fatalf("read the status write: %v", err)
			}
			if task := implementerTask(t, pool); !strings.Contains(task, want) || !strings.Contains(reason, want) || from != phase.AwaitingMerge || status != "in_progress" {
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

// In awaiting_merge the READY head's push can arrive after the head and after a red settlement of
// it or of the code head it replaced, which stands for nothing at the READY head. The push decides
// the checks again when its replay of the head run changes the judgement (push), so the READY ends
// as it does when the push arrives first, whichever the three facts' order: withdrawn or standing,
// the same merge queue packets and the same phase.
func TestARedInAwaitingMergeEndsTheSameWhicheverOrderThePushArrivesIn(t *testing.T) {
	const required = "pr-checks-result"
	sync := syncedTo("head")
	push := pushedFromTo("code", "head", "legion-merger[bot]")
	for _, settles := range []string{"code", "head"} {
		red := intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: settles, CheckRuns: []record.AttemptRun{{Name: required, ID: 1}},
			Generation: 1, Snapshot: "red", Failing: []string{required}}
		type outcome struct {
			phase     phase.Phase
			published int
		}
		var results []outcome
		for _, order := range [][]intake.Fact{{push, sync, red}, {sync, red, push}, {sync, push, red}} {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "code", Required: []string{required}})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "head", Summary: "READY #42 at head"})
			engine := readyEngine("merge-queue")
			for _, fact := range order {
				applyRefusingNothing(t, pool, engine, fact)
			}
			results = append(results, outcome{issuePhase(t, pool), len(mergeQueuePublishes(t, pool))})
		}
		for i, got := range results[1:] {
			if got != results[0] {
				t.Errorf("red at %s, order %d: phase %s and %d merge queue packets; push first: %s and %d",
					settles, i+1, got.phase, got.published, results[0].phase, results[0].published)
			}
		}
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
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
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
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
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
// read moves nothing. Outside awaiting_merge a conflict moves nothing either: a conflict is no red
// for the CI rule to act on, since GitHub runs no checks on it for a verdict to stand, and the
// tester or reviewer already at work will see it on GitHub when its pass runs.
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
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
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
			reason, from := checksRedNotice(t, pool)
			var status string
			if err := pool.QueryRow(context.Background(), "select payload->>'status' from outbox where kind = 'dispatch_status'").Scan(&status); err != nil {
				t.Fatalf("read the status write: %v", err)
			}
			if task := implementerTask(t, pool); !strings.Contains(task, want) || !strings.Contains(reason, want) || from != phase.AwaitingMerge || status != "in_progress" {
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
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
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
	var notices int
	if err := pool.QueryRow(context.Background(), "select count(*) from outbox where kind = 'notice' and payload->>'kind' = 'checks-red'").Scan(&notices); err != nil {
		t.Fatalf("count the architect's checks-red notices: %v", err)
	}
	reason, _ := checksRedNotice(t, pool)
	if notices != 1 || !strings.Contains(reason, want) {
		t.Fatalf("checks-red notices = %d, reason %q; want exactly one saying %q", notices, reason, want)
	}
}

// A head the daemon reached past one it never recorded - h2 to h4, h3 between - is judged by the
// chain of pushes h2..h3..h4 whichever of the pushes and the head arrives first. Two pushes of the
// review App's plan h4's red, so the work stays in testing in every order; the review App's red
// tests then the implementer's fix leave h4's red unplanned, which sends the work back.
func TestASkippedHeadIsJudgedByItsChainOfPushesInEitherOrder(t *testing.T) {
	const required, implementer = "pr-checks-result", "legion-implementer[bot]"
	for _, tc := range []struct {
		name          string
		first, second intake.Fact
		planned       bool
	}{
		{"two pushes of the review App's", pushedFromTo("h2", "h3", testReviewApp), pushedFromTo("h3", "h4", testReviewApp), true},
		{"the review App's red tests, then the implementer's fix", pushedFromTo("h2", "h3", testReviewApp), pushedFromTo("h3", "h4", implementer), false},
	} {
		for _, order := range []struct {
			name  string
			facts []intake.Fact
		}{
			{"pushes first", []intake.Fact{tc.first, tc.second, syncedTo("h4"), settledAt("h4", required, 2, required)}},
			{"head first, pushes late in order", []intake.Fact{syncedTo("h4"), settledAt("h4", required, 2, required), tc.first, tc.second}},
			{"head first, the earlier head's push last", []intake.Fact{syncedTo("h4"), settledAt("h4", required, 2, required), tc.second, tc.first}},
		} {
			t.Run(tc.name+", "+order.name, func(t *testing.T) {
				pool := migratedPool(t)
				seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
					Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
				seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
					Number: 42, Branch: "legion/LEGION-208", HeadSHA: "h2", CheckedHead: "h2", Failing: []string{required},
					CheckRuns: []record.AttemptRun{{Name: required, ID: 1}}, Required: []string{required}})
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
				engine := readyEngine("merge-queue")
				for _, fact := range order.facts {
					applyRefusingNothing(t, pool, engine, fact)
				}
				want := phase.Implementing
				if tc.planned {
					want = phase.Testing
				}
				if got := issuePhase(t, pool); got != want {
					t.Fatalf("after every fact the issue is in %s, want %s", got, want)
				}
				var planned bool
				if err := pool.QueryRow(context.Background(), "select planned_red from pull_requests where issue = 'LEGION-208'").
					Scan(&planned); err != nil {
					t.Fatalf("read the pull request: %v", err)
				}
				if planned != tc.planned {
					t.Fatalf("planned red %v; want %v", planned, tc.planned)
				}
			})
		}
	}
}

// A reopened pull request whose head moved while it was closed - commits pushed then reach no
// synchronize - is judged as a synchronize to that head would be. Its push never arrives here, so
// the reopened head's red is held (holdsRed) until the hold lapses and the required-checks pass's
// next read decides it, which sends the work back; a reopen at the recorded head holds nothing and
// sends it back at once.
func TestAReopenedPullRequestJudgesItsNewHeadAsASynchronizeWould(t *testing.T) {
	const required = "pr-checks-result"
	for _, tc := range []struct {
		name string
		// failing is the recorded head's settlement, reopened is the head the reopen names.
		failing  []string
		reopened string
		want     phase.Phase
		// checksRed counts the architect's notices.
		checksRed int
	}{
		{"past a red", []string{required}, "h4", phase.Implementing, 1},
		{"past a green", []string{}, "h4", phase.Implementing, 1},
		{"at the recorded head", []string{required}, "h3", phase.Implementing, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestClosed, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "h3", CheckedHead: "h3", Failing: tc.failing,
				CheckRuns: []record.AttemptRun{{Name: required, ID: 1}}, Required: []string{required}})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			engine := readyEngine("merge-queue")
			applyRefusingNothing(t, pool, engine, intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208",
				HeadSHA: tc.reopened, Reopened: true})
			applyRefusingNothing(t, pool, engine, settledAt(tc.reopened, required, 2, required))
			if tc.reopened != "h3" {
				if got, kinds := issuePhase(t, pool), noticeKinds(t, pool, "LEGION-208"); got != phase.Testing || len(kinds) != 0 {
					t.Fatalf("while the reopened head's push is awaited the issue is in %s and the architect was told %v, want testing and nothing", got, kinds)
				}
				applyRefusingNothing(t, pool, readyEngineAt("merge-queue", heldRedLapses), intake.RequiredChecks{Repo: "sjawhar/legion", Number: 42, Names: []string{required}})
			}
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
			if checksRed != tc.checksRed {
				t.Fatalf("checks-red %d, pr-blocked %d; want %d and none", checksRed, blocked, tc.checksRed)
			}
		})
	}
}

// Each head is held from its own arrival, not from when the head run opened: h3's push is lost for
// good, so the run h3 opened stays open, and h4 arrives on top of it an hour later. h4's red is held
// for its own ten minutes, nothing told, and the pass's unchanged read decides it once that lapses.
func TestAHeadThatJoinsAnOldRunIsHeldFromItsOwnArrival(t *testing.T) {
	const required = "pr-checks-result"
	start := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "h2", CheckedHead: "h2", Failing: []string{},
		CheckRuns: []record.AttemptRun{{Name: required, ID: 1}}, Required: []string{required}})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	applyRefusingNothing(t, pool, readyEngineAt("merge-queue", start), syncedTo("h3"))
	later := readyEngineAt("merge-queue", start.Add(time.Hour))
	applyRefusingNothing(t, pool, later, syncedTo("h4"))
	applyRefusingNothing(t, pool, later, pushedFromTo("h3", "h4", "legion-implementer[bot]"))
	applyRefusingNothing(t, pool, later, settledAt("h4", required, 2, required))
	if got, kinds := issuePhase(t, pool), noticeKinds(t, pool, "LEGION-208"); got != phase.Testing || len(kinds) != 0 {
		t.Fatalf("an hour after the run opened, h4's red leaves the issue in %s and the architect told %v, want testing and nothing: h4 arrived just now", got, kinds)
	}
	applyRefusingNothing(t, pool, readyEngineAt("merge-queue", start.Add(time.Hour+redHold+time.Second)),
		intake.RequiredChecks{Repo: "sjawhar/legion", Number: 42, Names: []string{required}})
	if got := issuePhase(t, pool); got != phase.Implementing {
		t.Fatalf("once h4's own hold lapses the issue is in %s, want implementing", got)
	}
}

// A branch that returns, inside one open head run, to a head whose lapse was already decided is
// held from its new arrival, and the pass's unchanged read decides its red once that hold lapses:
// the arrival clears the mark of the earlier lapse (stampRun). h3's push is lost for good, so the
// run h3 opened stays open; h3 settles green and its hold lapses, decided green; the branch moves
// to h4 and back to h3, by a synchronize alone or by a push back to it, and h3 settles red.
func TestAHeadTheBranchReturnsToIsHeldAndDecidedAgain(t *testing.T) {
	const required = "pr-checks-result"
	type timed struct {
		at   time.Duration
		fact intake.Fact
	}
	unchanged := intake.RequiredChecks{Repo: "sjawhar/legion", Number: 42, Names: []string{required}}
	for _, tc := range []struct {
		name string
		// lapse is when the pass decides h3's first, green, hold; returned is when the branch is
		// back at h3.
		lapse, returned time.Duration
		back            []timed
	}{
		{name: "by a synchronize", lapse: 11 * time.Minute, returned: 13 * time.Minute, back: []timed{
			{12 * time.Minute, syncedTo("h4")},
			{12 * time.Minute, pushedFromTo("h3", "h4", "legion-implementer[bot]")},
			{13 * time.Minute, syncedTo("h3")},
			{14 * time.Minute, settledAt("h3", required, 5, required)},
		}},
		{name: "by a push back", lapse: time.Hour, returned: time.Hour + 5*time.Minute, back: []timed{
			{time.Hour, pushedFromTo("h3", "h4", "legion-implementer[bot]")},
			{time.Hour, syncedTo("h4")},
			{time.Hour, settledAt("h4", required, 3)},
			{time.Hour + 5*time.Minute, pushedFromTo("h4", "h3", "legion-implementer[bot]")},
			{time.Hour + 5*time.Minute, syncedTo("h3")},
			{time.Hour + 6*time.Minute, settledAt("h3", required, 4, required)},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "h2", CheckedHead: "h2", Failing: []string{},
				CheckRuns: []record.AttemptRun{{Name: required, ID: 1}}, Required: []string{required}})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			var logged bytes.Buffer
			at := func(d time.Duration) *Engine {
				return New(record.NewStore(), Config{
					Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), MergeQueueRole: "merge-queue", Linger: time.Hour, ProductionCheckRole: claim.RoleTester, ReviewAppLogin: testReviewApp, Repositories: testRepositories,
					Clock: func() time.Time { return start.Add(d) },
				}, slog.New(slog.NewTextHandler(&logged, nil)))
			}
			applyRefusingNothing(t, pool, at(0), syncedTo("h3"))
			applyRefusingNothing(t, pool, at(time.Minute), settledAt("h3", required, 2))
			applyRefusingNothing(t, pool, at(tc.lapse), unchanged)
			if pr := runOf(t, pool); pr.Run == nil || pr.Run.Decided != "h3" || !strings.Contains(logged.String(), "lapsed; the green is decided as it stands") {
				t.Fatalf("h3's green hold lapsed with run %+v and log %q, want the run open, its lapse decided at h3 and logged as a green", pr.Run, logged.String())
			}
			for _, step := range tc.back {
				applyRefusingNothing(t, pool, at(step.at), step.fact)
			}
			held := at(tc.returned + redHold - time.Minute)
			applyRefusingNothing(t, pool, held, unchanged)
			if got, kinds := issuePhase(t, pool), noticeKinds(t, pool, "LEGION-208"); got != phase.Testing || len(kinds) != 0 {
				t.Fatalf("h3's red within ten minutes of the return leaves the issue in %s and the architect told %v, want testing and nothing", got, kinds)
			}
			lapsed := at(tc.returned + redHold + time.Second)
			if pr := runOf(t, pool); !lapsed.HoldLapsed(pr) {
				t.Fatalf("ten minutes after the return the hold on h3's red has not lapsed (run %+v), want it lapsed: the return starts h3's hold afresh", pr.Run)
			}
			applyRefusingNothing(t, pool, lapsed, unchanged)
			if got := issuePhase(t, pool); got != phase.Implementing {
				t.Fatalf("once the return's hold lapses the issue is in %s, want implementing", got)
			}
			if !strings.Contains(logged.String(), "lapsed; the red is decided as it stands") {
				t.Fatalf("the log %q does not say the red was decided", logged.String())
			}
		})
	}
}

// runOf is LEGION-208's pull request as recorded.
func runOf(t *testing.T, pool *pgxpool.Pool) record.PullRequest {
	t.Helper()
	var pr *record.PullRequest
	seedRecord(t, pool, func(tx pgx.Tx) (err error) {
		pr, err = record.NewStore().PullRequest(t.Context(), tx, "LEGION-208")
		return err
	})
	return *pr
}

// A red that settles while its head waits for its push is held, and acted on once the push closes
// the head run, as the push arriving first acts on it: h3 arrives before its push; its red settles
// and is held, nothing told; h3's push - the implementer's code - closes the run, and the red then
// sends the work back with a checks-red.
func TestARedHeldWhileItsHeadWaitsIsReportedWhenThePushArrives(t *testing.T) {
	const required, implementer = "pr-checks-result", "legion-implementer[bot]"
	sync, red, push := syncedTo("h3"), settledAt("h3", required, 2, required), pushedFromTo("h2", "h3", implementer)
	type outcome struct {
		phase   phase.Phase
		notices string
	}
	run := func(facts []intake.Fact, held func(pool *pgxpool.Pool)) outcome {
		pool := migratedPool(t)
		seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
			Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
		seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
			Number: 42, Branch: "legion/LEGION-208", HeadSHA: "h2", CheckedHead: "h2", Failing: []string{required},
			CheckRuns: []record.AttemptRun{{Name: required, ID: 1}}, Required: []string{required}})
		seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
		engine := readyEngine("merge-queue")
		for i, fact := range facts {
			applyRefusingNothing(t, pool, engine, fact)
			if held != nil && i == 1 {
				held(pool)
			}
		}
		return outcome{issuePhase(t, pool), fmt.Sprint(noticeKinds(t, pool, "LEGION-208"))}
	}
	first := run([]intake.Fact{push, sync, red}, nil)
	if first != (outcome{phase.Implementing, "[checks-red]"}) {
		t.Fatalf("push first: %+v, want implementing and one checks-red", first)
	}
	late := run([]intake.Fact{sync, red, push}, func(pool *pgxpool.Pool) {
		if kinds := noticeKinds(t, pool, "LEGION-208"); len(kinds) != 0 || issuePhase(t, pool) != phase.Testing {
			t.Fatalf("while h3's push is on its way the architect was told %v and the issue is in %s, want nothing told and testing: the red is held",
				kinds, issuePhase(t, pool))
		}
	})
	if late != first {
		t.Fatalf("push last: %+v; push first: %+v", late, first)
	}
}

// Two heads that each arrive before their pushes - h3, then h4 - are judged as the pushes arriving
// first judge them, whatever order the pushes arrive in, and the work goes where the pushes first
// send it: the head run the first opens replays once they do, and h4's red is held while it waits
// rather than sending the work back on a judgement the pushes can still change. The review App's
// red tests then the implementer's code end with the count unchanged and the work sent back; two
// pushes of the review App's plan h4's red, which keeps the work in testing - also below the cap,
// where a red sent back before the pushes arrived could not be taken back.
func TestTwoHeadsAheadOfTheirPushesAreJudgedAsThePushesFirstJudgeThem(t *testing.T) {
	const required, implementer = "pr-checks-result", "legion-implementer[bot]"
	for _, tc := range []struct {
		name          string
		first, second intake.Fact
		want          phase.Phase
		planned       bool
	}{
		{"the review App's red tests, then the implementer's code", pushedFromTo("h2", "h3", testReviewApp), pushedFromTo("h3", "h4", implementer),
			phase.Implementing, false},
		{"two pushes of the review App's", pushedFromTo("h2", "h3", testReviewApp), pushedFromTo("h3", "h4", testReviewApp), phase.Testing, true},
	} {
		red := settledAt("h4", required, 2, required)
		{
			for _, order := range []struct {
				name  string
				facts []intake.Fact
			}{
				{"pushes first", []intake.Fact{tc.first, tc.second, syncedTo("h3"), syncedTo("h4"), red}},
				{"both heads first, pushes in order", []intake.Fact{syncedTo("h3"), syncedTo("h4"), red, tc.first, tc.second}},
				{"both heads first, pushes reversed", []intake.Fact{syncedTo("h3"), syncedTo("h4"), red, tc.second, tc.first}},
				{"each head before its push", []intake.Fact{syncedTo("h3"), tc.first, syncedTo("h4"), red, tc.second}},
			} {
				t.Run(fmt.Sprintf("%s, %s", tc.name, order.name), func(t *testing.T) {
					pool := migratedPool(t)
					seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
						Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
					seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
						Number: 42, Branch: "legion/LEGION-208", HeadSHA: "h2", CheckedHead: "h2", Failing: []string{required},
						CheckRuns: []record.AttemptRun{{Name: required, ID: 1}}, Required: []string{required}})
					seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
					engine := readyEngine("merge-queue")
					for _, fact := range order.facts {
						applyRefusingNothing(t, pool, engine, fact)
					}
					if got := issuePhase(t, pool); got != tc.want {
						t.Fatalf("after every fact the issue is in %s, want %s", got, tc.want)
					}
					var planned, open bool
					if err := pool.QueryRow(context.Background(), "select planned_red, head_run is not null from pull_requests where issue = 'LEGION-208'").
						Scan(&planned, &open); err != nil {
						t.Fatalf("read the pull request: %v", err)
					}
					if planned != tc.planned || open {
						t.Fatalf("planned red %v, head run open %v; want %v and closed", planned, open, tc.planned)
					}
				})
			}
		}
	}
}

// A push for a head the branch has since left - delivered after the head the record holds - moves
// no head, and decides the checks when its replay of the head run changes the judgement or closes
// the run (push), since no other fact reads what that replay changed. The chain it completes can
// clear the planned mark the heads above it carried from a head whose push was unknown, with the
// newest head's push lost for good, so the run stays open; or it can close the run with the
// judgement exactly as the heads left it. Each way the red at the recorded head, held while the
// run waited and now past its hold, comes to stand and sends the work back, and each way one
// clause of the re-check is what reads it.
func TestAPushForAHeadTheBranchLeftDecidesTheChecks(t *testing.T) {
	const required, implementer = "pr-checks-result", "legion-implementer[bot]"
	start := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		// seeded is the pull request before the heads arrive, facts what arrives while the run is
		// open, and late the push, for a head the branch has left, delivered an hour later. closes
		// says the late push closes the head run, which is the re-check's other clause. Every row
		// ends with the planned mark clear.
		seeded record.PullRequest
		facts  []intake.Fact
		late   intake.Fact
		closes bool
	}{
		{
			name: "the late push clears the planned mark h4 and h5 carried",
			seeded: record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "h3", Required: []string{required}, PlannedRed: true},
			facts: []intake.Fact{syncedTo("h4"), syncedTo("h5"), settledAt("h5", required, 2, required)},
			late:  pushedFromTo("h3", "h4", implementer),
			// The count is untouched: no verdict stands at h3 or h4 for a move to count.
		},
		{
			name: "the late push closes the run with the judgement unchanged",
			seeded: record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "h3", Required: []string{required}},
			facts: []intake.Fact{syncedTo("h4"), syncedTo("h5"), pushedFromTo("h4", "h5", implementer),
				settledAt("h5", required, 2, required)},
			late: pushedFromTo("h3", "h4", implementer),
			// h4's move is the run's one waiting head, and the push that reached it changes no
			// judgement: no verdict stands at h3 for its move to count, and an implementer's push
			// leaves the planned mark clear, as the unchained move already had it. Closing the run
			// is what lets h5's held red be judged.
			closes: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
			seedPR(t, pool, tc.seeded)
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			for _, fact := range tc.facts {
				applyRefusingNothing(t, pool, readyEngineAt("merge-queue", start), fact)
			}
			if got := issuePhase(t, pool); got != phase.Testing {
				t.Fatalf("before the late push the issue is in %s, want testing: the red is planned or held at the exhausted count", got)
			}

			applyRefusingNothing(t, pool, readyEngineAt("merge-queue", start.Add(time.Hour)), tc.late)

			if got := issuePhase(t, pool); got != phase.Implementing {
				t.Fatalf("after the late push the issue is in %s, want implementing: the push's replay is what makes the red stand", got)
			}
			pr := runOf(t, pool)
			if pr.PlannedRed {
				t.Fatalf("planned red %v, want false", pr.PlannedRed)
			}
			if (pr.Run == nil) != tc.closes {
				t.Fatalf("the head run is open %v after the late push, want open %v: which clause of the re-check reads the push depends on it", pr.Run != nil, !tc.closes)
			}
		})
	}
}

// A reopen past a head the daemon never recorded keeps the pushes recorded while the pull request
// was closed, so a push that arrives after the reopen completes the chain the reopen's move took:
// the review App's pushes h2..h3, pushed while closed, and h3..h4 after the reopen at h4 take back
// the count the reopen made and plan h4's red.
func TestAReopenKeepsThePushesItsMoveIsJudgedBy(t *testing.T) {
	const required = "pr-checks-result"
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
		Phase: phase.Testing, Generation: 1, Status: "testing", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestClosed, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "h2", CheckedHead: "h2", Failing: []string{required},
		CheckRuns: []record.AttemptRun{{Name: required, ID: 1}}, Required: []string{required}})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
	engine := readyEngine("merge-queue")
	applyRefusingNothing(t, pool, engine, pushedFromTo("h2", "h3", testReviewApp))
	applyRefusingNothing(t, pool, engine, intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208",
		HeadSHA: "h4", Reopened: true})
	applyRefusingNothing(t, pool, engine, pushedFromTo("h3", "h4", testReviewApp))
	var planned bool
	if err := pool.QueryRow(context.Background(), "select planned_red from pull_requests where issue = 'LEGION-208'").
		Scan(&planned); err != nil {
		t.Fatalf("read the pull request: %v", err)
	}
	if !planned {
		t.Fatalf("planned red %v; want true", planned)
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

// pushedFromTo is the branch push that replaced from with to, by pusher.
func pushedFromTo(from, to, pusher string) intake.Fact {
	return intake.Push{Repo: "sjawhar/legion", Branch: "legion/LEGION-208", Before: from, After: to, Pusher: pusher}
}

// phaseStartKeys is every key a phase worker's start payload carries
// (record.SuperviseRequest): the operation, the issue's tree, the role, its task, the issue
// generation, the phase the start serves and the assignment it hands that role. The list is
// asserted whole rather than key by key, so a start that grows a field nobody meant it to carry —
// a role to wait for before it acts, say — fails here.
const phaseStartKeys = "assignment,generation,needsSlot,op,phase,role,task,tree"

// implementerStart is the task of the implementer's start the outbox holds, the row that carries
// the task: a start is two rows (workflow.StartOrOwe), and the relaunch row, which carries no
// task, is the other. The task row carries phaseStartKeys and nothing else; needsSlot is among
// them, because the delivery of a worker's task is the one part of a transition that waits, for
// the issue's admission slot.
func implementerStart(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var task, keys string
	if err := pool.QueryRow(context.Background(), `select payload->>'task',
		(select string_agg(key, ',' order by key) from jsonb_object_keys(payload) as key)
		from outbox where kind = 'supervise' and payload->>'op' = 'start' and coalesce(payload->>'task', '') <> '' and payload->>'role' = 'implementer'`).Scan(&task, &keys); err != nil {
		t.Fatalf("read the implementer's start: %v", err)
	}
	if keys != phaseStartKeys {
		t.Fatalf("the implementer's start carries %s, want %s", keys, phaseStartKeys)
	}
	return task
}

// checksRedNotice is the architect's checks-red notice the outbox holds: its reason, and the phase
// the red stopped.
func checksRedNotice(t *testing.T, pool *pgxpool.Pool) (reason string, from phase.Phase) {
	t.Helper()
	var stopped string
	if err := pool.QueryRow(context.Background(), "select payload->>'reason', coalesce(payload->>'phase', '') from outbox where kind = 'notice' and payload->>'kind' = 'checks-red'").Scan(&reason, &stopped); err != nil {
		t.Fatalf("read the architect's checks-red notice: %v", err)
	}
	return reason, phase.Phase(stopped)
}

// The implementer pushes and completes without waiting for CI, so a required check can settle red
// on its head while the issue is still in implementing, where a red sends nothing back, and the
// settlement does not come again. The completion is then the fact that lets the red stand outside
// implementing (implementationReady): the work goes back to implementing at once, the
// implementer's task and the architect's checks-red naming the red as it would from testing, the
// return counted as a round, and no tester round runs on code CI already found broken. A head CI
// found green, or has not judged yet, goes on to testing, where a red that settles later sends the
// work back as any red in testing does.
func TestACompletionOnARedThatSettledWhileImplementingSendsTheWorkBack(t *testing.T) {
	const required = "pr-checks-result"
	for _, tc := range []struct {
		name string
		// seeded is the pull request's head before the facts; before is what reaches the issue
		// before the implementer's completion at head.
		seeded string
		before []intake.Fact
		want   phase.Phase
	}{
		{"red", "head", []intake.Fact{settledAt("head", required, 1, required)}, phase.Implementing},
		{"green", "head", []intake.Fact{settledAt("head", required, 1)}, phase.Testing},
		{"not settled yet", "head", nil, phase.Testing},
		// The round's fix pushed, the completion arriving before GitHub's synchronize: the record
		// still holds the red head the round fixed, and that red sends nothing back.
		{"red at the head the round fixed, the completion before its synchronize", "red-head", []intake.Fact{settledAt("red-head", required, 1, required)}, phase.Testing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: tc.seeded, Required: []string{required}})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			engine := testEngine(config.DesignGateRootIssues, nil)
			for i, fact := range tc.before {
				applyRefusingNothing(t, pool, engine, fact)
				if got := issuePhase(t, pool); got != phase.Implementing {
					t.Fatalf("after fact %d (%T) the issue is in %s, want implementing: no verdict moves it there", i+1, fact, got)
				}
			}
			assertOutboxCount(t, pool, "supervise", 0)
			applyRefusingNothing(t, pool, engine, intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Commit: "head", HandoffRecord: "implement-1", HandoffVersion: 1, Summary: "implemented"})
			if got := issuePhase(t, pool); got != tc.want {
				t.Fatalf("after the implementer's completion the issue is in %s, want %s", got, tc.want)
			}
			if tc.want == phase.Testing {
				if got := noticeKinds(t, pool, "LEGION-208"); containsNotice(got, "checks-red") {
					t.Fatalf("notices %v, want no checks-red", got)
				}
				if rounds := implementerRounds(t, pool); rounds != 0 {
					t.Fatalf("rounds = %d, want none: nothing went back", rounds)
				}
				return
			}
			task := implementerStart(t, pool)
			reason, from := checksRedNotice(t, pool)
			want := "CI is red at head: " + required
			if !strings.Contains(task, want) || !strings.Contains(task, "Phase: implementing.") || !strings.Contains(reason, want) || from != phase.Testing {
				t.Fatalf("implementer task %q, notice %q in %q; want both to say %q, the notice in testing", task, reason, from, want)
			}
			if rounds := implementerRounds(t, pool); rounds != 1 {
				t.Fatalf("rounds = %d, want the return counted as one", rounds)
			}
		})
	}
}

// A round that fixed a red at "red" with one code push completes at once, before either webhook of
// that push reaches the daemon. Whichever order the push and its synchronize then arrive in, the
// only red is the one the round fixed, and the issue stays in testing: neither the completion nor
// the push judges the head the push is replacing, and the fixed head's arrival finds no red.
func TestACompletionBeforeItsPushesWebhooksIsNotSentBackOnTheRedItFixed(t *testing.T) {
	const required = "pr-checks-result"
	push := pushedFromTo("red", "fixed", "legion-implementer[bot]")
	for _, tc := range []struct {
		name  string
		after []intake.Fact
	}{
		{"push then synchronize", []intake.Fact{push, syncedTo("fixed")}},
		{"synchronize then push", []intake.Fact{syncedTo("fixed"), push}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "red", Required: []string{required}})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			engine := testEngine(config.DesignGateRootIssues, nil)
			applyRefusingNothing(t, pool, engine, settledAt("red", required, 1, required),
				intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Commit: "fixed", HandoffRecord: "implement-1", HandoffVersion: 1, Summary: "fixed the red"})
			for i, fact := range tc.after {
				applyRefusingNothing(t, pool, engine, fact)
				if got := issuePhase(t, pool); got != phase.Testing {
					t.Fatalf("after fact %d (%T) the issue is in %s, want testing: the only red is the head the round fixed", i+1, fact, got)
				}
			}
			if got := noticeKinds(t, pool, "LEGION-208"); containsNotice(got, "checks-red") {
				t.Fatalf("notices %v, want no checks-red", got)
			}
			if rounds := implementerRounds(t, pool); rounds != 0 {
				t.Fatalf("rounds = %d, want none", rounds)
			}
		})
	}
}

// A push webhook naming the head the record already holds can arrive after that head's own red
// settled while implementing and after the completion handed the head on without judging it. The
// push is then the first fact outside implementing to see the red, and sends the work back: a code
// commit pushed above the commit the completion reports, and the implementer's push whose late
// arrival clears the planned mark its head carried from the review App's red tests below it, so the
// red at the head is no longer planned.
func TestALatePushOfTheRecordedHeadActsOnItsRed(t *testing.T) {
	const required = "pr-checks-result"
	for _, tc := range []struct {
		name     string
		seeded   record.PullRequest
		before   []intake.Fact
		commit   string
		latePush intake.Fact
	}{
		{
			name: "a code commit above the commit the completion reports",
			seeded: record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "h0", Required: []string{required}},
			before: []intake.Fact{pushedFromTo("h0", "reported", "legion-implementer[bot]"), syncedTo("reported"),
				syncedTo("c2"), settledAt("c2", required, 1, required)},
			commit:   "reported",
			latePush: pushedFromTo("reported", "c2", "legion-implementer[bot]"),
		},
		{
			name: "the implementer's push whose late arrival clears the planned mark its head carried",
			seeded: record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "red-tests", Required: []string{required}, PlannedRed: true},
			before:   []intake.Fact{settledAt("red-tests", required, 1, required), syncedTo("fix"), settledAt("fix", required, 2, required)},
			commit:   "fix",
			latePush: pushedFromTo("red-tests", "fix", "legion-implementer[bot]"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
			seedPR(t, pool, tc.seeded)
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			engine := testEngine(config.DesignGateRootIssues, nil)
			applyRefusingNothing(t, pool, engine, tc.before...)
			applyRefusingNothing(t, pool, engine, intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Commit: tc.commit, HandoffRecord: "implement-1", HandoffVersion: 1, Summary: "implemented"})
			if got := issuePhase(t, pool); got != phase.Testing {
				t.Fatalf("after the completion the issue is in %s, want testing: the completion judges no head it did not report, nor a red the cap holds", got)
			}
			applyRefusingNothing(t, pool, engine, tc.latePush)
			if got := issuePhase(t, pool); got != phase.Implementing {
				t.Fatalf("after the late push the issue is in %s, want implementing: the red stands for the head and nothing else acts on it", got)
			}
			if rounds := implementerRounds(t, pool); rounds != 1 {
				t.Fatalf("rounds = %d, want the return counted as one", rounds)
			}
		})
	}
}

// A red only a required workflow makes, which the completion leaves to the pass's next read of the
// runs (its result may be two minutes old), still sends the work back: that read, returning the
// same runs, is a change once the completion cleared the head they were read at, and acts on it.
func TestAWorkflowOnlyRedTheCompletionDefersIsSentBackByTheNextRead(t *testing.T) {
	const required, workflow = "pr-checks-result", ".github/workflows/review.yml"
	redRun := []record.RequiredWorkflow{{Path: workflow, Result: "failure"}}
	for _, tc := range []struct {
		name   string
		seeded record.PullRequest
		before []intake.Fact
		commit string
		// after is applied after the completion; the issue is in testing until then.
		after []intake.Fact
	}{
		{
			name: "a workflow-only red, the next read unchanged",
			seeded: record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Required: []string{required}, Workflows: redRun, WorkflowsHead: "head"},
			before: []intake.Fact{settledAt("head", required, 1)},
			commit: "head",
			after: []intake.Fact{intake.RequiredChecks{Repo: "sjawhar/legion", Number: 42, Names: []string{required},
				Workflows: redRun, WorkflowsHead: "head"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
			seedPR(t, pool, tc.seeded)
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			engine := testEngine(config.DesignGateRootIssues, nil)
			applyRefusingNothing(t, pool, engine, tc.before...)
			applyRefusingNothing(t, pool, engine, intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", Commit: tc.commit, HandoffRecord: "implement-1", HandoffVersion: 1, Summary: "implemented"})
			if len(tc.after) > 0 {
				if got := issuePhase(t, pool); got != phase.Testing {
					t.Fatalf("after the completion the issue is in %s, want testing: the completion leaves a workflow-only red to the next read", got)
				}
				applyRefusingNothing(t, pool, engine, tc.after...)
			}
			if got := issuePhase(t, pool); got != phase.Implementing {
				t.Fatalf("the issue is in %s, want implementing: a red standing for the head handed on was acted on by nothing", got)
			}
			if rounds := implementerRounds(t, pool); rounds != 1 {
				t.Fatalf("rounds = %d, want the return counted as one", rounds)
			}
		})
	}
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
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Required: []string{required}})
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
