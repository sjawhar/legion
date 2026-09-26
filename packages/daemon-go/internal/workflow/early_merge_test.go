package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A pull request that finishes before the issue awaits its merge — merged early, or closed without
// merging — is the architect's to act on, as the shipped daemon routes it: the architect is told
// once, naming the pull request and the phase the issue was in, and the issue's phase is left for
// the architect to decide. A redelivered observation tells it nothing new.
func TestAPullRequestFinishingOutsideAwaitingMergeTellsTheArchitectOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fact   intake.Fact
		kind   record.NoticeKind
		reason string
	}{
		{name: "merged early", fact: intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"}, kind: "pr-merged",
			reason: "pull request #42 merged while LEGION-208 was in reviewing"},
		{name: "closed without merging", fact: intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 42}, kind: "pr-closed-unmerged",
			reason: "pull request #42 closed without merging while LEGION-208 was in reviewing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head",
				Failing: []string{}, FailingStatuses: []string{}, CheckRuns: []record.AttemptRun{}})
			for _, id := range []string{"finished", "redelivered"} {
				if _, err := intake.ApplyFact(context.Background(), pool, "github", id, tc.fact, testEngine(), admissionStub{}); err != nil {
					t.Fatalf("ApplyFact %s: %v", id, err)
				}
			}
			assertPhase(t, pool, phase.Reviewing)
			got := architectNotices(t, pool)
			if len(got) != 1 || got[0].Kind != tc.kind || got[0].Role != claim.RoleArchitect || !strings.HasPrefix(got[0].Reason, tc.reason) {
				t.Fatalf("notices = %+v, want one %s to the architect beginning %q", got, tc.kind, tc.reason)
			}
		})
	}
}

// A close older than the pull request's newest lifecycle event is a late redelivery
// (classify.LateLifecycle): it neither closes the pull request nor tells the architect. The newer
// close closes it at the head it carries and tells the architect once, and that close redelivered at
// its own clock, which is not late, tells it nothing more, since the pull request is already closed.
func TestALateCloseTellsTheArchitectNothingAndARedeliveredCloseNothingMore(t *testing.T) {
	earlier, later := lateApplied.Add(-time.Hour), lateApplied.Add(time.Minute)
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-c",
		HeadUpdatedAt: lateApplied, HeadUpdatedAtSource: "webhook", Failing: []string{}, FailingStatuses: []string{}, CheckRuns: []record.AttemptRun{}})
	for _, step := range []struct {
		id      string
		fact    intake.Fact
		head    string
		state   record.PullRequestState
		notices int
	}{
		{"late-close", lateClosed("head-b", earlier), "head-c", record.PullRequestOpen, 0},
		{"close", lateClosed("head-d", later), "head-d", record.PullRequestClosed, 1},
		{"redelivered-close", lateClosed("head-d", later), "head-d", record.PullRequestClosed, 1},
	} {
		if _, err := intake.ApplyFact(context.Background(), pool, "github", step.id, step.fact, testEngine(), admissionStub{}); err != nil {
			t.Fatalf("ApplyFact %s: %v", step.id, err)
		}
		var head string
		var state record.PullRequestState
		if err := pool.QueryRow(t.Context(), "select head_sha, state from pull_requests where issue = 'LEGION-208'").Scan(&head, &state); err != nil {
			t.Fatalf("read the pull request after %s: %v", step.id, err)
		}
		got := architectNotices(t, pool)
		if head != step.head || state != step.state || len(got) != step.notices {
			t.Fatalf("after %s the pull request is at %s, %s, with architect notices %+v; want %s, %s, and %d", step.id, head, state, got, step.head, step.state, step.notices)
		}
	}
	assertPhase(t, pool, phase.Reviewing)
}

// An issue that reaches awaiting_merge with its pull request already merged has nothing left to
// merge: it moves on to the production check at once, and no READY asks a human to merge it.
func TestAnIssueReachingAwaitingMergeWithItsPullRequestMergedMovesToProductionCheck(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "U"})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4, ApprovedVersion: new(4)})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestMerged, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head",
		Failing: []string{}, FailingStatuses: []string{}, CheckRuns: []record.AttemptRun{}})

	result, err := intake.ApplyFact(context.Background(), pool, "api", "ready", intake.HandoffComplete{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merger-claim", Generation: 1, Ready: true, Summary: readyPacket, Commit: "head"}, readyEngine("merge-queue"), admissionStub{})
	if err != nil || result.Refusal != nil {
		t.Fatalf("ApplyFact READY = %#v, %v", result, err)
	}
	assertPhase(t, pool, phase.ProductionCheck)
	assertStartTasksNamePhase(t, pool, phase.ProductionCheck)
	if got := messageBodies(t, pool); len(got) != 0 {
		t.Fatalf("Dispatch messages = %q, want no READY for a merged pull request", got)
	}
	if got := mergeQueuePublishes(t, pool); len(got) != 0 {
		t.Fatalf("merge queue publishes = %v, want none for a merged pull request", got)
	}
}

// architectNotices is every notice the outbox holds for the architect, in order.
func architectNotices(t *testing.T, pool *pgxpool.Pool) []record.Notice {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload->>'kind', coalesce(payload->>'role', ''), coalesce(payload->>'reason', '') from outbox where kind = 'notice' and payload->>'role' = 'architect' order by id")
	if err != nil {
		t.Fatalf("read notices: %v", err)
	}
	defer rows.Close()
	notices := []record.Notice{}
	for rows.Next() {
		var notice record.Notice
		if err := rows.Scan(&notice.Kind, &notice.Role, &notice.Reason); err != nil {
			t.Fatalf("scan notice: %v", err)
		}
		notices = append(notices, notice)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate notices: %v", err)
	}
	return notices
}
