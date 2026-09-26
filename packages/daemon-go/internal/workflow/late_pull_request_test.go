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

// lateApplied is the clock of the newest lifecycle event the seeded pull request has applied.
var lateApplied = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

// pullRequestView is what a late event could overwrite.
type pullRequestView struct {
	head, verdict, decision string
	state                   record.PullRequestState
}

// afterEvents seeds a reviewing issue whose pull request is at head-c, green and approved, with
// lateApplied as its clock, applies facts in order, and reads the pull request back.
func afterEvents(t *testing.T, state record.PullRequestState, facts ...intake.Fact) pullRequestView {
	t.Helper()
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "U"})
	seedPR(t, pool, record.PullRequest{State: state, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208",
		HeadSHA: "head-c", HeadUpdatedAt: lateApplied, HeadUpdatedAtSource: "webhook", Verdict: "green", ReviewDecision: "approved",
		Failing: []string{}, FailingStatuses: []string{}, CheckRuns: []record.AttemptRun{}})
	for i, fact := range facts {
		if _, err := intake.ApplyFact(context.Background(), pool, "github", fmt.Sprintf("step-%d", i), fact, testEngine(), admissionStub{}); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	var got pullRequestView
	if err := pool.QueryRow(context.Background(), "select head_sha, verdict, review_decision, state from pull_requests where issue = 'LEGION-208'").
		Scan(&got.head, &got.verdict, &got.decision, &got.state); err != nil {
		t.Fatalf("read pull request: %v", err)
	}
	return got
}

func lateSync(head string, at time.Time) intake.Fact {
	return intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: head, UpdatedAt: at}
}

func lateOpened(head string, at time.Time) intake.Fact {
	return intake.PullRequestOpened{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: head, UpdatedAt: at}
}

func lateClosed(at time.Time) intake.Fact {
	return intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 42, UpdatedAt: at}
}

// GitHub redelivers a failed delivery on request, hours late if need be, and nothing orders a
// webhook against those that followed it. Every pull request lifecycle event carries the pull
// request's updated_at, so one older than the newest applied is a late redelivery and changes
// nothing: it neither moves the head back and clears what was decided at the newer head, nor
// reopens or closes the pull request against a newer close or reopen. An event at the same clock
// applies (GitHub's clock is to the second, so two real events can share one), and so does one
// that carries no clock.
func TestALatePullRequestEventChangesNothing(t *testing.T) {
	earlier, later := lateApplied.Add(-time.Hour), lateApplied.Add(time.Minute)
	kept := pullRequestView{head: "head-c", verdict: "green", decision: "approved", state: record.PullRequestOpen}
	closed := pullRequestView{head: "head-c", verdict: "green", decision: "approved", state: record.PullRequestClosed}
	for _, tc := range []struct {
		name string
		seed record.PullRequestState
		fact intake.Fact
		want pullRequestView
	}{
		{"a synchronize older than the head", record.PullRequestOpen, lateSync("head-b", earlier), kept},
		{"an opened older than the head", record.PullRequestOpen, lateOpened("head-a", earlier), kept},
		{"a close older than the reopen", record.PullRequestOpen, lateClosed(earlier), kept},
		{"a reopen older than the close", record.PullRequestClosed, lateOpened("head-c", earlier), closed},

		{"a newer synchronize", record.PullRequestOpen, lateSync("head-d", later), pullRequestView{head: "head-d", state: record.PullRequestOpen}},
		{"a synchronize at the same clock", record.PullRequestOpen, lateSync("head-d", lateApplied), pullRequestView{head: "head-d", state: record.PullRequestOpen}},
		{"a synchronize with no clock", record.PullRequestOpen, lateSync("head-d", time.Time{}), pullRequestView{head: "head-d", state: record.PullRequestOpen}},
		{"a newer close", record.PullRequestOpen, lateClosed(later), closed},
		{"a close with no clock", record.PullRequestOpen, lateClosed(time.Time{}), closed},
		{"a newer reopen", record.PullRequestClosed, lateOpened("head-c", later), pullRequestView{head: "head-c", state: record.PullRequestOpen}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := afterEvents(t, tc.seed, tc.fact); got != tc.want {
				t.Fatalf("pull request %+v, want %+v", got, tc.want)
			}
		})
	}
}

// An event with no clock (from a listener that did not carry updated_at) is applied, but it never
// lowers the pull request's clock: the stored clock stays the latest one known, so an older
// timestamped event redelivered afterwards is still late and changes nothing.
func TestAnEventWithNoClockKeepsTheLatestClock(t *testing.T) {
	earlier, later := lateApplied.Add(-time.Hour), lateApplied.Add(time.Minute)
	for _, tc := range []struct {
		name  string
		facts []intake.Fact
		head  string
		state record.PullRequestState
	}{
		{"a synchronize with no clock, then an older one",
			[]intake.Fact{lateSync("head-d", later), lateSync("head-e", time.Time{}), lateSync("head-b", lateApplied)}, "head-e", record.PullRequestOpen},
		{"a reopen with no clock, then an older synchronize",
			[]intake.Fact{lateOpened("head-c", time.Time{}), lateSync("head-b", earlier)}, "head-c", record.PullRequestOpen},
		{"a close with no clock, then an older reopen",
			[]intake.Fact{lateClosed(time.Time{}), lateOpened("head-c", earlier)}, "head-c", record.PullRequestClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := afterEvents(t, record.PullRequestOpen, tc.facts...); got.head != tc.head || got.state != tc.state {
				t.Fatalf("pull request at %s, %s; want %s, %s", got.head, got.state, tc.head, tc.state)
			}
		})
	}
}
