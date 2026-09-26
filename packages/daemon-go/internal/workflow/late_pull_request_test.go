package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// GitHub redelivers a failed delivery on request, hours late if need be, and nothing orders a
// webhook against those that followed it. Every pull request lifecycle event carries the pull
// request's updated_at, so one older than the newest applied is a late redelivery and changes
// nothing: it neither moves the head back and clears what was decided at the newer head, nor
// reopens or closes the pull request against a newer close or reopen. An event at the same clock
// applies (GitHub's clock is to the second, so two real events can share one), and so does one
// that carries no clock.
func TestALatePullRequestEventChangesNothing(t *testing.T) {
	applied := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	earlier, later := applied.Add(-time.Hour), applied.Add(time.Minute)
	open := func(state record.PullRequestState) record.PullRequest {
		return record.PullRequest{State: state, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208",
			HeadSHA: "head-c", HeadUpdatedAt: applied, HeadUpdatedAtSource: "webhook", Verdict: "green", ReviewDecision: "approved",
			Failing: []string{}, FailingStatuses: []string{}, CheckRuns: []record.AttemptRun{}}
	}
	type want struct {
		head, verdict, decision string
		state                   record.PullRequestState
	}
	kept := want{head: "head-c", verdict: "green", decision: "approved", state: record.PullRequestOpen}
	for _, tc := range []struct {
		name string
		seed record.PullRequestState
		fact intake.Fact
		want want
	}{
		{"a synchronize older than the head", record.PullRequestOpen,
			intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-b", UpdatedAt: earlier}, kept},
		{"an opened older than the head", record.PullRequestOpen,
			intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-a", UpdatedAt: earlier}, kept},
		{"a close older than the reopen", record.PullRequestOpen,
			intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 42, UpdatedAt: earlier}, kept},
		{"a reopen older than the close", record.PullRequestClosed,
			intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-c", UpdatedAt: earlier},
			want{head: "head-c", verdict: "green", decision: "approved", state: record.PullRequestClosed}},

		{"a newer synchronize", record.PullRequestOpen,
			intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-d", UpdatedAt: later},
			want{head: "head-d", state: record.PullRequestOpen}},
		{"a synchronize at the same clock", record.PullRequestOpen,
			intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-d", UpdatedAt: applied},
			want{head: "head-d", state: record.PullRequestOpen}},
		{"a synchronize with no clock", record.PullRequestOpen,
			intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-d"},
			want{head: "head-d", state: record.PullRequestOpen}},
		{"a newer close", record.PullRequestOpen,
			intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 42, UpdatedAt: later},
			want{head: "head-c", verdict: "green", decision: "approved", state: record.PullRequestClosed}},
		{"a close with no clock", record.PullRequestOpen,
			intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 42},
			want{head: "head-c", verdict: "green", decision: "approved", state: record.PullRequestClosed}},
		{"a newer reopen", record.PullRequestClosed,
			intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head-c", UpdatedAt: later},
			want{head: "head-c", state: record.PullRequestOpen}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
			seedPR(t, pool, open(tc.seed))
			if _, err := intake.ApplyFact(context.Background(), pool, "github", "late", tc.fact, testEngine(), admissionStub{}); err != nil {
				t.Fatalf("ApplyFact: %v", err)
			}
			var got want
			if err := pool.QueryRow(context.Background(), "select head_sha, verdict, review_decision, state from pull_requests where issue = 'LEGION-208'").
				Scan(&got.head, &got.verdict, &got.decision, &got.state); err != nil {
				t.Fatalf("read pull request: %v", err)
			}
			if got != tc.want {
				t.Fatalf("pull request %s, want %s", describe(got.head, got.verdict, got.decision, got.state), describe(tc.want.head, tc.want.verdict, tc.want.decision, tc.want.state))
			}
		})
	}
}

func describe(head, verdict, decision string, state record.PullRequestState) string {
	return fmt.Sprintf("{head %s, verdict %q, decision %q, %s}", head, verdict, decision, state)
}
