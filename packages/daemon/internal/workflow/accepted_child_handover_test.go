package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The child the accepted-parent hold stopped runs once the ancestor that held it has closed,
// which is what the architect's notice promises it ("it starts once <root> has closed").
//
// A person moves a new issue under a tree whose root is already accepted. The daemon records it
// at admitted and holds it there, and that same hold is what lets the root close at all:
// record.OpenBeneath leaves it out of the wait a merged root keeps for the issues beneath it.
// Without this the root merged, the tree lingered, and the linger's own hand-over passed the
// child over as an admitted member waiting on nothing — so it stood todo at admitted in a closed
// tree for ever, with nobody told.
//
// Two levels deep, through the linger's expiry: the held child hangs under a child of the root,
// not under the root itself.
func TestTheChildHeldUnderAnAcceptedRootIsHandedOverWhenTheRootCloses(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 11, 4, 4, 0, 0, 0, time.UTC)
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: 48 * time.Hour,
		Clock: func() time.Time { return now }}, nil)
	root, child, held := "LEGION-208", "LEGION-209", "LEGION-210"
	rootKey, childKey := root, child
	seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Project: "LEGION", Title: "root", Phase: phase.AwaitingMerge, Generation: 7, Status: "retro",
		Rank: "U", LastDispatchSeq: 1})
	// The child between them finished, as a tree's children do before their root is accepted.
	seedIssue(t, pool, record.Issue{Key: child, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Parent: &rootKey, Project: "LEGION", Title: "child", Phase: phase.Done, Generation: 7,
		Status: "done", Rank: "V1", LastDispatchSeq: 1})
	// The issue a person moved to todo under that finished child while the root was accepted.
	seedIssue(t, pool, record.Issue{Key: held, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Parent: &childKey, Project: "LEGION", Title: "held", Phase: phase.Admitted, Generation: 7,
		Status: "todo", Rank: "V2", LastDispatchSeq: 1})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root, Repo: "sjawhar/legion", Number: 43,
		Branch: "legion/LEGION-208", HeadSHA: "head", Failing: []string{}})

	apply := applyFacts(t, pool, engine)
	apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge"})
	if at := issueAt(t, pool, root); at != phase.Done {
		t.Fatalf("%s stands at %s after its merge, want done: the issue it holds at admitted is left out of its wait", root, at)
	}

	// The merge of a root opens its production check, and a tree lingers past its deadline while
	// one is open: the check's own close is another rule's, so the fixture takes it away rather
	// than driving a look to its end.
	if _, err := pool.Exec(context.Background(), "delete from production_checks where issue = $1", root); err != nil {
		t.Fatalf("close the production check of %s: %v", root, err)
	}
	apply("linger", intake.LingerExpired{Issue: root, Generation: 7})

	assertHandedOver(t, pool, held, root, 7)
}

// The same promise when a person closes the accepted root by hand, which leaves no merged pull
// request at all: an architect's tree is signed off, and a person marks the root done on the
// Dispatch dashboard while it stands in retro. The linger read the merge, so the held child was
// skipped and stood todo at admitted in a closed tree for ever, though its architect had been
// told it starts once the root has closed. The expiry reads the child's own state instead — in
// the workflow and never started — which is the same answer whatever phase its root was closed
// from and whatever the close wrote over (unfinishedHandOvers).
//
// A root whose worker died in retro is accepted too: it stands held from retro, its reviewed
// head is the one the architect signed off, and the same child is held beneath it
// (record.Accepted). Read by the root's phase, such a root was not accepted, and the child
// stayed todo at admitted in the closed tree; read by the child's own state, both roots hand it
// over.
func TestTheChildHeldUnderAnAcceptedRootIsHandedOverWhenAPersonClosesItByHand(t *testing.T) {
	for _, standing := range []record.Issue{
		{Phase: phase.Retro},
		{Phase: phase.Held, Hold: &record.Hold{From: phase.Retro}},
	} {
		name := string(standing.Phase)
		if standing.Hold != nil {
			name += "-from-" + string(standing.Hold.From)
		}
		t.Run(name, func(t *testing.T) {
			pool := migratedPool(t)
			ctx := context.Background()
			now := time.Date(2026, 11, 4, 4, 0, 0, 0, time.UTC)
			engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: 48 * time.Hour,
				Clock: func() time.Time { return now }}, nil)
			root, held := "LEGION-208", "LEGION-210"
			rootKey := root
			seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: root, Project: "LEGION", Title: "root", Phase: standing.Phase, Hold: standing.Hold, Generation: 7,
				Status: "retro", DispatchStatus: "retro", Rank: "U", LastDispatchSeq: 1})
			// The issue a person moved to todo under the root while it was accepted, held at admitted.
			seedIssue(t, pool, record.Issue{Key: held, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: root, Parent: &rootKey, Project: "LEGION", Title: "held", Phase: phase.Admitted, Generation: 7,
				Status: "todo", Rank: "V2", LastDispatchSeq: 1})
			// The root's own head was approved and accepted, and never merged: nothing of its pull
			// request says it was accepted.
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: root, Repo: "sjawhar/legion", Number: 43,
				Branch: "legion/LEGION-208", HeadSHA: "head", Failing: []string{}})

			apply := applyFacts(t, pool, engine)
			apply("closed-by-hand", intake.DispatchIssue{Key: root, ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.closed",
				Status: "done", Title: "root", Rank: "U"})
			var lingering bool
			if err := pool.QueryRow(ctx, "select linger_until is not null from issues where key = $1", root).Scan(&lingering); err != nil || !lingering {
				t.Fatalf("the hand-closed root lingers = %v, %v; want its tree lingering", lingering, err)
			}
			// Admission, which runs after the workflow handler, is what records the status a person's
			// move shows (its recordObservation); this test runs the engine alone.
			if _, err := pool.Exec(ctx, "update issues set status = 'done', dispatch_status = 'done' where key = $1", root); err != nil {
				t.Fatalf("record the status the person's close shows: %v", err)
			}

			apply("linger", intake.LingerExpired{Issue: root, Generation: 7})

			assertHandedOver(t, pool, held, root, 7)
		})
	}
}

// assertHandedOver is the hand-over of key the linger of tree at generation made: the durable
// fence it is held under, and the one Dispatch write that puts the issue todo with the legion
// label under that fence, which is what makes it a root of its own.
func assertHandedOver(t *testing.T, pool *pgxpool.Pool, key, tree string, generation uint64) {
	t.Helper()
	handover := handoverOf(t, pool, key)
	if handover == nil || handover.Tree != tree || handover.Generation != generation || handover.Cancelled {
		t.Fatalf("the hand-over fence of %s = %+v, want %s at generation %d, standing", key, handover, tree, generation)
	}
	var writes int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where kind = 'dispatch_status' and issue = $1
		and payload->>'status' = 'todo' and payload->>'handOver' = 'true' and payload->>'handOverTree' = $2
		and payload->>'handOverLinger' = $3`, key, tree, fmt.Sprint(generation)).Scan(&writes); err != nil || writes != 1 {
		t.Fatalf("the hand-over writes of %s = %d, %v; want one fenced on %s's linger %d: nothing else would ever start it, though the architect was told it starts once %s closed",
			key, writes, err, tree, generation, tree)
	}
}

// handoverOf is the durable hand-over fence recorded for key, nil for none.
func handoverOf(t *testing.T, pool *pgxpool.Pool, key string) *record.Handover {
	t.Helper()
	var handover *record.Handover
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		found, err := record.NewStore().Handover(context.Background(), tx, key)
		handover = found
		return err
	}); err != nil {
		t.Fatalf("read the hand-over of %s: %v", key, err)
	}
	return handover
}
