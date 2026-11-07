package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A member held at admitted is handed over whatever becomes, during the linger, of the ancestor
// that held it. The tree is the spec's own: a root R waiting for its children, a child C whose
// own reviewed head the daemon has accepted, and a grandchild G a person moved to todo under C,
// which the daemon recorded at admitted and told C's architect starts once C has closed
// (record.HeldUnderAccepted). A person closes R by hand, and the tree lingers.
//
// An ancestor below the root does not keep its phase through that linger: C's pull request still
// merges, a person still moves C to done or to backlog, and record.OpenBeneath leaves G out of
// the wait any of them read, so nothing stops them. Decided from the hold, the expiry then read
// G as held by nobody and left it todo at admitted in a closed tree, with its architect told it
// would start. Decided from G's own state — in the workflow and never started — the hand-over
// is the same in all four, which is what the architect's notice promised.
func TestTheHeldGrandchildIsHandedOverWhateverBecomesOfItsAcceptedParent(t *testing.T) {
	root, child, held := "LEGION-208", "LEGION-209", "LEGION-210"
	for _, during := range []struct {
		name  string
		close func(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact) intake.Result)
	}{
		{
			name:  "the-parent-stands",
			close: func(*testing.T, *pgxpool.Pool, func(string, intake.Fact) intake.Result) {},
		},
		{
			name: "the-parents-pull-request-merges",
			close: func(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact) intake.Result) {
				t.Helper()
				apply("child-merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 44, MergeSHA: "merge"})
				if at := issueAt(t, pool, child); at != phase.Done {
					t.Fatalf("%s stands at %s after its merge inside the linger, want done: a member of a lingering tree still closes at its merge", child, at)
				}
				// That close opens the child's production check, and a tree lingers past its
				// deadline while one is open: the check's own close is another rule's, so the
				// fixture takes it away rather than driving a look to its end.
				if _, err := pool.Exec(context.Background(), "delete from production_checks where issue = $1", child); err != nil {
					t.Fatalf("close the production check of %s: %v", child, err)
				}
			},
		},
		{
			name: "a-person-moves-the-parent-to-done",
			close: func(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact) intake.Result) {
				t.Helper()
				apply("child-done", intake.DispatchIssue{Key: child, ResolvedRepo: "sjawhar/legion", Seq: 3, Type: "issue.closed",
					Status: "done", Title: "child", Rank: "V1"})
				recordTheStatusAMoveShows(t, pool, child, "done")
			},
		},
		{
			name: "a-person-moves-the-parent-to-backlog",
			close: func(t *testing.T, pool *pgxpool.Pool, apply func(string, intake.Fact) intake.Result) {
				t.Helper()
				apply("child-parked", intake.DispatchIssue{Key: child, ResolvedRepo: "sjawhar/legion", Seq: 3, Type: "issue.updated",
					Status: "backlog", Title: "child", Rank: "V1"})
				recordTheStatusAMoveShows(t, pool, child, "backlog")
			},
		},
	} {
		t.Run(during.name, func(t *testing.T) {
			pool := migratedPool(t)
			now := time.Date(2026, 11, 4, 4, 0, 0, 0, time.UTC)
			engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: 48 * time.Hour,
				Clock: func() time.Time { return now }}, nil)
			rootKey, childKey := root, child
			// The root's own first phase waits for the child beneath it, so it stands at admitted.
			seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: root, Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 7,
				Status: "in_progress", DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1})
			seedIssue(t, pool, record.Issue{Key: child, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: root, Parent: &rootKey, Project: "LEGION", Title: "child", Phase: phase.AwaitingMerge, Generation: 7,
				Status: "retro", DispatchStatus: "retro", Rank: "V1", LastDispatchSeq: 1})
			seedIssue(t, pool, record.Issue{Key: held, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: root, Parent: &childKey, Project: "LEGION", Title: "held", Phase: phase.Admitted, Generation: 7,
				Status: "todo", DispatchStatus: "todo", Rank: "V2", LastDispatchSeq: 1})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: child, Repo: "sjawhar/legion", Number: 44,
				Branch: "legion/LEGION-209", HeadSHA: "head", Failing: []string{}})

			apply := applyFacts(t, pool, engine)
			apply("root-closed-by-hand", intake.DispatchIssue{Key: root, ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.closed",
				Status: "done", Title: "root", Rank: "U"})
			var lingering bool
			if err := pool.QueryRow(t.Context(), "select linger_until is not null from issues where key = $1", root).Scan(&lingering); err != nil || !lingering {
				t.Fatalf("the hand-closed root lingers = %v, %v; want its tree lingering", lingering, err)
			}
			// Admission, which runs after the workflow handler, is what records the status a
			// person's move shows (its recordObservation); this test runs the engine alone.
			recordTheStatusAMoveShows(t, pool, root, "done")

			during.close(t, pool, apply)

			apply("linger", intake.LingerExpired{Issue: root, Generation: 7})

			assertHandedOver(t, pool, held, root, 7)
		})
	}
}

// A parent of the closing tree that is still waiting for its own Dispatch children when the
// linger expires is handed over as every other member that never started is. Nothing held it at
// admitted but the children-first wait (record.ChildWaits), which its new tree keeps: its
// children are open in Dispatch still, so its first phase waits for them there as it waited
// here. Left behind instead, it would stand todo at admitted in a closed tree, which is the one
// place no slot, no status and no person's move reaches.
func TestALingerHandsOverTheParentStillWaitingForItsChildren(t *testing.T) {
	pool := migratedPool(t)
	until := time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC)
	root, parent, openChild := "LEGION-208", "LEGION-209", "LEGION-300"
	rootKey := root
	seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Project: "LEGION", Title: "a finished root", Phase: phase.Done, Generation: 4, Status: "done",
		DispatchStatus: "done", Rank: "U", LingerUntil: &until})
	seedIssue(t, pool, record.Issue{Key: parent, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Parent: &rootKey, Project: "LEGION", Title: "a parent of its own", Phase: phase.Admitted, Generation: 4,
		Status: "todo", DispatchStatus: "todo", Rank: "V", LastDispatchSeq: 1})
	// What Dispatch shows: the member waits on nothing, and the child filed under it is open, so
	// its own first phase does its children's work first and never started.
	seedMirror(t, pool, parent, "todo")
	seedMirrorChild(t, pool, openChild, "todo", parent)

	if _, err := intake.ApplyFact(context.Background(), pool, "timer", "linger:LEGION-208:4",
		intake.LingerExpired{Issue: root, Generation: 4}, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("ApplyFact the linger's expiry: %v", err)
	}

	assertHandedOver(t, pool, parent, root, 4)
}

// What a closed tree keeps is what a person took out of the workflow, and nothing else. This
// root is closed by hand at admitted, before any phase of it ran, so no phase of it was ever
// accepted. The member's own state decides the hand-over, not the root's acceptance, so the
// member standing todo at admitted is handed over — its tree has closed and nothing would start
// it where it stands — while the sibling a person parked and the child its architect never
// released stay with the tree, their Dispatch status saying they wait on nobody
// (record.OutOfWorkflow).
func TestAClosedTreeKeepsOnlyTheMembersThatLeftTheWorkflow(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 11, 4, 4, 0, 0, 0, time.UTC)
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: 48 * time.Hour,
		Clock: func() time.Time { return now }}, nil)
	root, member, parked, unreleased := "LEGION-208", "LEGION-210", "LEGION-211", "LEGION-212"
	rootKey := root
	seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Project: "LEGION", Title: "root", Phase: phase.Admitted, Generation: 7,
		Status: "in_progress", DispatchStatus: "in_progress", Rank: "U", LastDispatchSeq: 1})
	for _, member := range []struct{ key, status string }{
		{key: member, status: "todo"},
		{key: parked, status: "backlog"},
		{key: unreleased, status: "triage"},
	} {
		seedIssue(t, pool, record.Issue{Key: member.key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: root, Parent: &rootKey, Project: "LEGION", Title: member.key, Phase: phase.Admitted, Generation: 7,
			Status: member.status, DispatchStatus: member.status, Rank: "V", LastDispatchSeq: 1})
	}

	apply := applyFacts(t, pool, engine)
	apply("closed-by-hand", intake.DispatchIssue{Key: root, ResolvedRepo: "sjawhar/legion", Seq: 2, Type: "issue.closed",
		Status: "done", Title: "root", Rank: "U"})
	recordTheStatusAMoveShows(t, pool, root, "done")

	apply("linger", intake.LingerExpired{Issue: root, Generation: 7})

	assertHandedOver(t, pool, member, root, 7)
	if got := handoverFences(t, pool); len(got) != 1 || got[0] != member {
		t.Fatalf("hand-over fences = %v, want %s's alone: a member out of the workflow stays with the tree that closed", got, member)
	}
}

// seedMirrorChild writes what Dispatch says of a child filed under parent: its status and the
// parent that makes it one of parent's open children (record.ChildWaits reads the pair).
func seedMirrorChild(t *testing.T, pool *pgxpool.Pool, key, status, parent string) {
	t.Helper()
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutDispatchIssue(t.Context(), tx, record.DispatchIssueRow{
			Key: key, Project: "LEGION", Status: status, Seq: 1, Parent: parent})
	})
}

// recordTheStatusAMoveShows records on the issue the status a person's move shows, which
// admission writes when it runs behind the workflow handler (its recordObservation): a test that
// drives the engine alone writes it itself.
func recordTheStatusAMoveShows(t *testing.T, pool *pgxpool.Pool, key, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "update issues set status = $2, dispatch_status = $2 where key = $1", key, status); err != nil {
		t.Fatalf("record the status %s's move shows: %v", key, err)
	}
}
