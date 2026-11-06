package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The other side of record.Issue.AcceptedAtClose, which no test holds: a root closed while it
// was not accepted hands nobody over for that reason. The mark is what the expiry reads, so a
// beginLinger that wrote true for every close would pass every test of the accepted side and
// change this one — a member standing at admitted under a root nobody accepted is a child that
// never started and waits on nothing, which the linger leaves where it is.
//
// The fixture is the accepted test's own, with one fact different: the root is closed by hand
// at admitted, before any phase of it ran, rather than in retro.
func TestARootClosedWhileItWasNotAcceptedHandsNobodyOver(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 4, 4, 0, 0, 0, time.UTC)
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: 48 * time.Hour,
		Clock: func() time.Time { return now }}, nil)
	root, member := "LEGION-208", "LEGION-210"
	rootKey := root
	seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 7,
		Status: "in_progress", DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1})
	seedIssue(t, pool, record.Issue{Key: member, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Parent: &rootKey, Project: "LEGION", Title: "member", Phase: phase.Admitted, Generation: 7,
		Status: "todo", Rank: "V2", LastDispatchSeq: 1})

	apply := applyFacts(t, pool, engine)
	apply("closed-by-hand", intake.DispatchIssue{Key: root, ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.closed",
		Status: "done", Title: "root", Rank: "U"})
	var accepted bool
	if err := pool.QueryRow(ctx, "select accepted_at_close from issues where key = $1", root).Scan(&accepted); err != nil {
		t.Fatalf("read the mark of %s: %v", root, err)
	}
	if accepted {
		t.Fatalf("%s was marked accepted at its close though no phase of it ever ran: the mark is what the expiry reads", root)
	}
	if _, err := pool.Exec(ctx, "update issues set status = 'done', dispatch_status = 'done' where key = $1", root); err != nil {
		t.Fatalf("record the status the person's close shows: %v", err)
	}

	apply("linger", intake.LingerExpired{Issue: root, Generation: 7})

	if handover := handoverOf(t, pool, member); handover != nil && !handover.Cancelled {
		t.Fatalf("the linger of a root nobody accepted handed %s over: %+v. Only a member the daemon held at admitted beneath an accepted ancestor is handed over", member, handover)
	}
	var writes int
	if err := pool.QueryRow(ctx, `select count(*) from outbox where kind = 'dispatch_status' and issue = $1
		and payload->>'handOver' = 'true'`, member).Scan(&writes); err != nil {
		t.Fatalf("count the hand-over writes of %s: %v", member, err)
	}
	if writes != 0 {
		t.Fatalf("the linger wrote %d hand-overs for %s, want none", writes, member)
	}
}
