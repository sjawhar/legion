package admit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// heldChildStatements counts the statements a pool's connections send.
type heldChildStatements struct{ sent atomic.Int64 }

func (c *heldChildStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.sent.Add(1)
	return ctx
}

func (c *heldChildStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// countingPool is a pool on pool's database whose statements counter counts.
func countingPool(t *testing.T, pool *pgxpool.Pool, counter *heldChildStatements) *pgxpool.Pool {
	t.Helper()
	config := pool.Config().Copy()
	config.ConnConfig.Tracer = counter
	counted, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open the counting pool: %v", err)
	}
	t.Cleanup(counted.Close)
	return counted
}

// Every fact looks at every child a blocker holds: admission's promotion hands the workflow the
// project's records and the mirror rows, and the workflow judges the tree of each child nothing
// holds any longer (StartUnblockedChildren). That judgement reads what every one of those trees
// is: a project whose trees each hold such a child paid a root, a slot list, a record list and a
// design gate per tree, on every fact of every consumer, the API and the boot's replay, under the
// one fact lock. It now reads the slots once and the gates of those trees once, whatever their
// number.
//
// Each tree here is a root of the waiting line that waits on an unfinished issue, so promotion
// starts nothing and writes nothing, and no tree holds an open gate, so no child starts: the
// statements counted are the judgement itself.
func TestAFactJudgesHeldChildrenWithAsManyStatementsWhateverNumberOfTreesHoldOne(t *testing.T) {
	run := func(t *testing.T, trees int) int64 {
		t.Helper()
		pool := migratedPool(t)
		for i := range trees {
			root := fmt.Sprintf("LEGION-%d", 200+2*i)
			child := fmt.Sprintf("LEGION-%d", 201+2*i)
			seedWaiting(t, pool, root, fmt.Sprintf("A%04d", i))
			seedMirror(t, pool, root, "todo", "LEGION-UNFINISHED")
			putIssue(t, pool, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: testProject, Title: child, Tree: root, Parent: &root, Phase: phase.Admitted, Generation: 1,
				Status: "todo", Rank: fmt.Sprintf("B%04d", i), LastDispatchSeq: 1})
			// The child waits on nothing any longer, so each fact judges whether its tree starts it.
			seedMirror(t, pool, child, "todo")
		}
		counter := &heldChildStatements{}
		counted := countingPool(t, pool, counter)
		admission := newAdmission(t, 2*trees+2, slog.New(slog.NewTextHandler(io.Discard, nil)))
		handlers := []intake.Handler{blockers.NewMirror(record.NewStore(), testProject), admission.engine, admission}
		fact := intake.DispatchArtifact{Key: fmt.Sprintf("LEGION-%d", 200), ArtifactID: "spec", Kind: intake.DispatchArtifactVersion, Version: 2}
		if _, err := intake.ApplyFact(context.Background(), counted, "dispatch", "a document version", fact, handlers...); err != nil {
			t.Fatalf("apply a fact with %d trees holding a child: %v", trees, err)
		}
		sent := counter.sent.Load()
		if sent == 0 {
			t.Fatalf("apply a fact with %d trees holding a child: the counting pool saw no statement", trees)
		}
		// Nothing starts: no tree's design gate is open, so each child stays where it was, and
		// each root waits on its unfinished issue rather than taking a slot.
		if child := issue(t, pool, "LEGION-201"); child.Phase != phase.Admitted {
			t.Fatalf("the held child = %s, want it still admitted: its tree has no open gate", child.Phase)
		}
		assertSlots(t, pool, nil)
		return sent
	}

	const many = 20
	few, lots := run(t, 1), run(t, many)
	if lots != few {
		t.Errorf("a fact sent %d statements with %d trees holding a child, %d with 1; want as many", lots, many, few)
	}
}

// One judgement covers several trees, and each tree's own state decides its own children: the
// child of a live tree whose design gate is open starts, and the child of a live tree whose gate
// is closed waits for that gate, in the very same fact. The closed tree sorts first and holds the
// first slot, so a judgement that stops at the first tree or child it does not start leaves the
// open tree's child behind and fails here.
func TestOneFactStartsTheChildOfTheTreeWhoseGateIsOpenAndLeavesTheOtherHeld(t *testing.T) {
	pool := migratedPool(t)
	closed, open := "LEGION-300", "LEGION-400"
	for _, root := range []string{closed, open} {
		seedSlotted(t, pool, root, root)
		child := root + "-child"
		putIssue(t, pool, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: testProject, Title: child, Tree: root, Parent: &root, Phase: phase.Admitted, Generation: 1,
			Status: "todo", Rank: root + "-B", LastDispatchSeq: 1})
		seedMirror(t, pool, child, "todo")
	}
	approved := 2
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		if err := records.PutGate(context.Background(), tx, record.DesignGate{Issue: open, ArtifactID: "spec-open", LatestVersion: 2, ApprovedVersion: &approved}); err != nil {
			t.Fatalf("put the open gate: %v", err)
		}
		if err := records.PutGate(context.Background(), tx, record.DesignGate{Issue: closed, ArtifactID: "spec-closed", LatestVersion: 3, ApprovedVersion: &approved}); err != nil {
			t.Fatalf("put the closed gate: %v", err)
		}
	})

	admission := newAdmission(t, 4, slog.New(slog.NewTextHandler(io.Discard, nil)))
	fact := intake.DispatchArtifact{Key: open, ArtifactID: "spec-open", Kind: intake.DispatchArtifactVersion, Version: 2}
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "a document version", fact,
		blockers.NewMirror(record.NewStore(), testProject), admission.engine, admission); err != nil {
		t.Fatalf("apply a fact with one open and one closed tree: %v", err)
	}

	if started := issue(t, pool, open+"-child"); started.Phase != phase.Planning {
		t.Errorf("the child of the tree whose gate is open = %s, want it planning", started.Phase)
	}
	if held := issue(t, pool, closed+"-child"); held.Phase != phase.Admitted {
		t.Errorf("the child of the tree whose gate is closed = %s, want it still admitted", held.Phase)
	}
}
