package workflow

import (
	"context"
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

	handover := handoverOf(t, pool, held)
	if handover == nil {
		t.Fatalf("the tree's close left %s at admitted with no hand-over: nothing would ever start it, though the architect was told it starts once %s closed", held, root)
	}
	if handover.Tree != root || handover.Generation != 7 {
		t.Fatalf("the hand-over of %s names tree %s at generation %d, want %s at 7", held, handover.Tree, handover.Generation, root)
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
