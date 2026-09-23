package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A child leaving the workflow ends only the child: the shipped daemon lingers the closed issue's
// own tree and wakes the tree's architect for a child (reducers.ts reduceIssueClosed). Here the
// root is mid-implementation when its child is signed off, or a human moves the child out of the
// workflow; the root keeps its phase, its linger stays unarmed, none of its claims is suspended,
// and the architect is told which child left and how.
func TestAChildLeavingTheWorkflowNeverClosesItsTree(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fact     intake.Fact
		notice   record.NoticeKind
		child    phase.Phase
		suspends int
	}{
		{name: "signed off", fact: intake.SignOff{Issue: "LEGION-209"}, notice: "child-closed", child: phase.Done, suspends: 1},
		{name: "closed by a human", fact: intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.closed", Status: "done", Title: "child", Parent: "LEGION-208", Rank: "V"}, notice: "child-closed", child: phase.ProductionCheck},
		{name: "moved to backlog", fact: intake.DispatchIssue{Key: "LEGION-209", Seq: 2, Type: "issue.updated", Status: "backlog", Title: "child", Parent: "LEGION-208", Rank: "V"}, notice: "child-status", child: phase.ProductionCheck},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			now := time.Date(2026, 9, 23, 4, 0, 0, 0, time.UTC)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress", Rank: "U", LastDispatchSeq: 1})
			parent := "LEGION-208"
			seedIssue(t, pool, record.Issue{Key: "LEGION-209", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent, Phase: phase.ProductionCheck, Generation: 7, Status: "retro", Rank: "V", LastDispatchSeq: 1})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implementer"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleImplementer, Claim: "child-implementer", HandoffCommit: "production-check", LastHandoff: "production-check"})
			engine := New(record.NewStore(), Config{Project: "LEGION", LingerHours: time.Hour, Clock: func() time.Time { return now }}, nil)

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
			if childPhase != tc.child {
				t.Fatalf("child phase = %s, want %s", childPhase, tc.child)
			}
			assertOutboxCount(t, pool, "linger_close", 0)
			if suspended := superviseRequests(t, pool, "suspend"); len(suspended) != tc.suspends || suspended["LEGION-208/"+string(claim.RoleImplementer)] != 0 {
				t.Fatalf("suspended %v, want only the child's own finished role (%d)", suspended, tc.suspends)
			}
			if got := noticeKinds(t, pool, "LEGION-209"); !containsNotice(got, tc.notice) {
				t.Fatalf("child notices = %v, want %s for the tree's architect", got, tc.notice)
			}
		})
	}
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
