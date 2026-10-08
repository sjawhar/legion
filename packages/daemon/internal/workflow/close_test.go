package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A child leaving the workflow ends only the child: the shipped daemon lingers the closed issue's
// own tree and wakes the tree's architect for a child (reducers.ts reduceIssueClosed). Here the
// root is mid-implementation when its child is signed off, or a human moves the child out of the
// workflow. The child leaves the table: its phase is parked in done, so no transition or status
// write follows. The root keeps its phase, its linger stays unarmed, none of its claims is stopped,
// and the architect is told which child left and how. What the child's claims get is the
// status's: a child closed as done has every claim closed (issue_close) and its Sandbox released
// with its volume (a releasing IssueSuspend), while one parked in backlog, icebox or triage has
// every claim suspended and its Sandbox kept (a keeping IssueSuspend).
func TestAChildLeavingTheWorkflowNeverClosesItsTree(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fact    intake.Fact
		notice  record.NoticeKind
		op      string
		release bool
	}{
		{name: "signed off", fact: intake.SignOff{Issue: "LEGION-209"}, notice: "child-closed", op: "issue_close", release: true},
		{name: "closed by a human", fact: intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.closed", Status: "done", Title: "child", Parent: "LEGION-208", Rank: "V"}, notice: "child-closed", op: "issue_close", release: true},
		{name: "moved to backlog", fact: intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.updated", Status: "backlog", Title: "child", Parent: "LEGION-208", Rank: "V"}, notice: "child-status", op: "suspend"},
		{name: "moved to icebox", fact: intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.updated", Status: "icebox", Title: "child", Parent: "LEGION-208", Rank: "V"}, notice: "child-status", op: "suspend"},
		{name: "moved to triage", fact: intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.updated", Status: "triage", Title: "child", Parent: "LEGION-208", Rank: "V"}, notice: "child-status", op: "suspend"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			now := time.Date(2026, 9, 23, 4, 0, 0, 0, time.UTC)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
			parent := "LEGION-208"
			seedIssue(t, pool, record.Issue{Key: "LEGION-209", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.ProductionCheck, Generation: 7, Status: "retro", Rank: "V", LastDispatchSeq: 1})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implementer"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleImplementer, Claim: "child-implementer", HandoffCommit: "production-check", LastHandoff: "production-check"})
			engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, Clock: func() time.Time { return now }}, nil)

			result, err := intake.ApplyFact(context.Background(), pool, "api", "child-leaves", tc.fact, engine, admissionStub{})
			if err != nil || result.Refusal != nil {
				t.Fatalf("ApplyFact = %#v, %v", result, err)
			}
			var rootPhase phase.Phase
			var lingering bool
			if err := pool.QueryRow(context.Background(), "select phase, linger_until is not null from issues where key = 'LEGION-208'").Scan(&rootPhase, &lingering); err != nil {
				t.Fatalf("read root: %v", err)
			}
			if rootPhase != phase.Implementing || lingering {
				t.Fatalf("root phase=%s lingering=%t, want implementing and no linger", rootPhase, lingering)
			}
			var childPhase phase.Phase
			if err := pool.QueryRow(context.Background(), "select phase from issues where key = 'LEGION-209'").Scan(&childPhase); err != nil {
				t.Fatalf("read child: %v", err)
			}
			if childPhase != phase.Done {
				t.Fatalf("child phase = %s, want parked in done", childPhase)
			}
			assertOutboxCount(t, pool, "linger_close", 0)
			stopped := superviseRequests(t, pool, tc.op)
			for _, role := range claim.Roles {
				if stopped["LEGION-209/"+string(role)] != 1 || stopped["LEGION-208/"+string(role)] != 0 {
					t.Fatalf("%s rows %v, want one for every one of the child's claims and none of the root's", tc.op, stopped)
				}
			}
			for _, other := range []string{"suspend", "issue_close", "tree_close"} {
				if other != tc.op && len(superviseRequests(t, pool, other)) != 0 {
					t.Fatalf("the child's leave as %s queued %s rows %v, want %s rows alone", tc.notice, other, superviseRequests(t, pool, other), tc.op)
				}
			}
			if got := issueSuspensions(t, pool, "LEGION-209"); len(got) != 1 || got[0].Release != tc.release || got[0].Tree != "LEGION-208" || got[0].Generation != 7 || got[0].TreeGeneration != 7 {
				t.Fatalf("issue suspensions of the child = %+v, want one of its tree at both generations 7 with release %t", got, tc.release)
			}
			if got := issueSuspensions(t, pool, "LEGION-208"); len(got) != 0 {
				t.Fatalf("the child's leave suspended the root's issue: %+v", got)
			}
			if got := noticeKinds(t, pool, "LEGION-209"); !containsNotice(got, tc.notice) {
				t.Fatalf("child notices = %v, want %s for the tree's architect", got, tc.notice)
			}
		})
	}
}

// Admission frees the slot of a root a human moves to triage, so the engine lingers its tree as it
// does for backlog, icebox, and done: a tree without a slot must not keep running.
func TestARootMovedToTriageLingersItsTree(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 9, 23, 4, 0, 0, 0, time.UTC)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, Clock: func() time.Time { return now }}, nil)
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "root-triage", intake.DispatchIssue{Key: "LEGION-208", Seq: 2, Type: "issue.updated", Status: "triage", Title: "root", Rank: "U"}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact: %v", err)
	}
	var rootPhase phase.Phase
	var lingering bool
	if err := pool.QueryRow(context.Background(), "select phase, linger_until is not null from issues where key = 'LEGION-208'").Scan(&rootPhase, &lingering); err != nil {
		t.Fatalf("read root: %v", err)
	}
	if rootPhase != phase.Done || !lingering {
		t.Fatalf("root in triage: phase=%s lingering=%t, want its tree lingering", rootPhase, lingering)
	}
	assertOutboxCount(t, pool, "linger_close", 1)
}

func noticeKinds(t *testing.T, pool *pgxpool.Pool, issue string) []record.NoticeKind {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload->>'kind' from outbox where kind = 'notice' and issue = $1 order by id", issue)
	if err != nil {
		t.Fatalf("read notices: %v", err)
	}
	defer rows.Close()
	var kinds []record.NoticeKind
	for rows.Next() {
		var kind record.NoticeKind
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scan notice: %v", err)
		}
		kinds = append(kinds, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate notices: %v", err)
	}
	return kinds
}

func containsNotice(kinds []record.NoticeKind, want record.NoticeKind) bool {
	for _, kind := range kinds {
		if kind == want {
			return true
		}
	}
	return false
}

// issueSuspensions decodes the queued issue_suspend rows of issue, oldest first.
func issueSuspensions(t *testing.T, pool *pgxpool.Pool, issue string) []record.IssueSuspend {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload from outbox where kind = 'issue_suspend' and issue = $1 order by id", issue)
	if err != nil {
		t.Fatalf("read issue suspensions: %v", err)
	}
	defer rows.Close()
	var suspensions []record.IssueSuspend
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("scan issue suspension: %v", err)
		}
		var suspension record.IssueSuspend
		if err := json.Unmarshal(payload, &suspension); err != nil {
			t.Fatalf("decode issue suspension %s: %v", payload, err)
		}
		suspensions = append(suspensions, suspension)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate issue suspensions: %v", err)
	}
	return suspensions
}

// An issue close is fenced as a suspend is: it acts unless a newer start has run against the claim
// — a person set the done child todo again, whose run it must not end — and a row without an id is
// never dropped. It names no linger, so no root is read for it; a tree close acts on its linger
// alone.
func TestStopActsFencesAnIssueCloseLikeASuspend(t *testing.T) {
	until := time.Date(2026, 9, 23, 5, 0, 0, 0, time.UTC)
	lingering := &record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Generation: 7, LingerUntil: &until}
	for _, tc := range []struct {
		name      string
		id        int64
		stop      record.SuperviseRequest
		lastStart int64
		root      *record.Issue
		want      bool
	}{
		{name: "an issue close no start followed", id: 10, stop: record.SuperviseRequest{Op: "issue_close"}, lastStart: 4, want: true},
		{name: "an issue close a newer start superseded", id: 10, stop: record.SuperviseRequest{Op: "issue_close"}, lastStart: 11, want: false},
		{name: "an issue close with no id", id: 0, stop: record.SuperviseRequest{Op: "issue_close"}, lastStart: 11, want: true},
		{name: "an issue close of a lingering tree reads no root", id: 10, stop: record.SuperviseRequest{Op: "issue_close"}, lastStart: 11, root: lingering, want: false},
		{name: "a suspend a newer start superseded", id: 10, stop: record.SuperviseRequest{Op: "suspend"}, lastStart: 11, want: false},
		{name: "a tree close of its linger", id: 10, stop: record.SuperviseRequest{Op: "tree_close", Linger: 7}, lastStart: 11, root: lingering, want: true},
		{name: "a tree close with no root", id: 10, stop: record.SuperviseRequest{Op: "tree_close", Linger: 7}, lastStart: 4, want: false},
		{name: "a start is not a stop", id: 10, stop: record.SuperviseRequest{Op: "start"}, lastStart: 4, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StopActs(tc.id, tc.stop, tc.lastStart, tc.root); got != tc.want {
				t.Fatalf("StopActs(%d, %s, %d, %v) = %t, want %t", tc.id, tc.stop.Op, tc.lastStart, tc.root != nil, got, tc.want)
			}
		})
	}
}

// The record keeps whether its pull request merged or closed, whatever phase the issue is in, so a
// re-admitted generation drops a finished pull request and keeps an open one.
func TestTheRecordKeepsWhetherItsPullRequestMergedOrClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		at    phase.Phase
		fact  intake.Fact
		want  record.PullRequestState
		phase phase.Phase
	}{
		{name: "merged when awaited", at: phase.AwaitingMerge, fact: intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"}, want: record.PullRequestMerged, phase: phase.ProductionCheck},
		{name: "merged early", at: phase.Reviewing, fact: intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 42, MergeSHA: "merge"}, want: record.PullRequestMerged, phase: phase.Reviewing},
		{name: "closed unmerged", at: phase.Reviewing, fact: intake.PullRequestClosed{Repo: "sjawhar/legion", Number: 42}, want: record.PullRequestClosed, phase: phase.Reviewing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: tc.at, Generation: 1, Status: "needs_review", Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head",
				Failing: []string{}, CheckRuns: []record.AttemptRun{}})
			if _, err := intake.ApplyFact(context.Background(), pool, "github", "pr-finished", tc.fact, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
				t.Fatalf("ApplyFact: %v", err)
			}
			var state record.PullRequestState
			var current phase.Phase
			if err := pool.QueryRow(context.Background(), "select pr.state, i.phase from pull_requests pr join issues i on i.key = pr.issue where pr.issue = 'LEGION-208'").Scan(&state, &current); err != nil {
				t.Fatalf("read pull request: %v", err)
			}
			if state != tc.want || current != tc.phase {
				t.Fatalf("pull request %s with the issue in %s, want %s in %s", state, current, tc.want, tc.phase)
			}
		})
	}
}
