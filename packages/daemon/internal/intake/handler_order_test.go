// This file is the external test package of intake: it runs the daemon's own handler chain, and
// the handlers behind the mirror (workflow, blockers) import intake themselves.
package intake_test

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	legionstore "github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// orderProject is the Dispatch project of these issues, which is also the project the mirror
// writes its rows under.
const orderProject = "LEGION"

// A person adding or removing a blocked_by link changes nothing else about the issue: Dispatch
// makes one issue.updated carrying the same status the issue already had, and the engine returns
// on exactly that (engine.go:185-187, "the engine reacts only to a newer status"). So the engine
// cannot be what records what an issue waits on, and the mirror must run ahead of it.
//
// This asserts both halves by behaviour. The mirror handler ahead of the engine writes the link
// the edit carries; a probe standing where admission stands, last in the chain, reads the
// project's mirror inside the very transaction the event is applied in and already sees it, which
// is the whole reason the order is what it is. And the engine alone, over the same event, writes
// no row at all.
func TestABlockedByOnlyEditIsMirroredAheadOfTheEngineThatSkipsIt(t *testing.T) {
	const issue, blocker = "LEGION-208", "LEGION-300"
	pool := orderedPool(t)
	records := record.NewStore()
	seedOrderedIssue(t, pool, records, record.Issue{
		Key: issue, Tree: issue, Project: orderProject, Title: "a root in the waiting line",
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "U",
		HandedOver: true, LastDispatchSeq: 5,
	})
	// The edit: a newer event carrying the status the record already holds, and the new link.
	edit := intake.DispatchIssue{
		Key: issue, Seq: 6, Type: "issue.updated", Status: "todo", Title: "a root in the waiting line",
		Rank: "U", HandedOver: true, ResolvedRepo: "acme/widgets", Blockers: []string{blocker},
	}

	engine := workflow.New(records, workflow.Config{Project: orderProject}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	decider := &mirrorReader{records: records}
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "blocked-by-only-edit", edit,
		blockers.NewMirror(records, orderProject), engine, decider); err != nil {
		t.Fatalf("apply the blocked-by-only edit: %v", err)
	}

	if got := blockers.Unfinished(decider.rows, issue); !slices.Equal(got, []string{blocker}) {
		t.Fatalf("the decider behind the engine saw %s waiting on %v in the event's own transaction, want %v: "+
			"the mirror's row must already be written when the handlers that judge from it look", issue, got, []string{blocker})
	}
	if got := mirroredBlockers(t, pool, records, issue); !slices.Equal(got, []string{blocker}) {
		t.Fatalf("the committed mirror row of %s says it waits on %v, want %v", issue, got, []string{blocker})
	}
	// The engine's own early return ran: it consumed nothing of the edit, not even its sequence.
	if recorded := orderedIssue(t, pool, records, issue); recorded.LastDispatchSeq != 5 {
		t.Fatalf("the engine took the edit at sequence %d, want the record left at 5: this event is not one the engine reacts to, "+
			"and a mirror behind it would be a mirror that never ran", recorded.LastDispatchSeq)
	}

	// The control: the same event, the engine alone. Nothing records what the issue waits on.
	alone := orderedPool(t)
	seedOrderedIssue(t, alone, records, record.Issue{
		Key: issue, Tree: issue, Project: orderProject, Title: "a root in the waiting line",
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "U",
		HandedOver: true, LastDispatchSeq: 5,
	})
	if _, err := intake.ApplyFact(context.Background(), alone, "dispatch", "blocked-by-only-edit",
		edit, workflow.New(records, workflow.Config{Project: orderProject}, slog.New(slog.NewTextHandler(io.Discard, nil)))); err != nil {
		t.Fatalf("apply the blocked-by-only edit to the engine alone: %v", err)
	}
	if got := mirroredBlockers(t, alone, records, issue); got != nil {
		t.Fatalf("the engine alone recorded %s waiting on %v; it writes no mirror row, which is why the mirror handler exists", issue, got)
	}
}

// A claim event says nothing the engine reacts to, and the engine records no claim: the mirror
// ahead of it writes the claim, and a handler standing where admission stands already reads it in
// the event's own transaction. The same event through the engine alone leaves the row's claim
// unseen.
func TestAClaimEventIsMirroredAheadOfTheEngine(t *testing.T) {
	const issue = "LEGION-208"
	claimEvent := intake.DispatchClaim{Key: issue, Seq: 6, ClaimedBy: "user:alice"}
	seed := func(pool *pgxpool.Pool, records record.Store) {
		seedOrderedIssue(t, pool, records, record.Issue{
			Key: issue, Tree: issue, Project: orderProject, Title: "a root in the waiting line",
			Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "U",
			HandedOver: true, LastDispatchSeq: 5,
		})
		if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
			return records.PutDispatchIssue(context.Background(), tx, record.DispatchIssueRow{Key: issue, Project: orderProject, Status: "todo", Seq: 5})
		}); err != nil {
			t.Fatalf("seed the mirror row: %v", err)
		}
	}
	records := record.NewStore()
	pool := orderedPool(t)
	seed(pool, records)
	reader := &claimReader{records: records}
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "claimed", claimEvent,
		blockers.NewMirror(records, orderProject), workflow.New(records, workflow.Config{Project: orderProject}, slog.New(slog.NewTextHandler(io.Discard, nil))), reader); err != nil {
		t.Fatalf("apply the claim event: %v", err)
	}
	if reader.holder != "user:alice" || reader.seq != 6 {
		t.Fatalf("the handler behind the engine read the claim as %q at %d in the event's own transaction, want user:alice at 6", reader.holder, reader.seq)
	}

	alone := orderedPool(t)
	seed(alone, records)
	aloneReader := &claimReader{records: records}
	if _, err := intake.ApplyFact(context.Background(), alone, "dispatch", "claimed", claimEvent,
		workflow.New(records, workflow.Config{Project: orderProject}, slog.New(slog.NewTextHandler(io.Discard, nil))), aloneReader); err != nil {
		t.Fatalf("apply the claim event to the engine alone: %v", err)
	}
	if aloneReader.holder != "" || aloneReader.seq != 0 {
		t.Fatalf("the engine alone recorded the claim %q at %d; it writes no mirror row, which is why the mirror handler writes the claim", aloneReader.holder, aloneReader.seq)
	}
}

// claimReader stands where admission stands and reads the mirrored claim of LEGION-208 inside
// the fact's transaction.
type claimReader struct {
	records record.Store
	holder  string
	seq     int64
}

func (c *claimReader) Apply(ctx context.Context, tx pgx.Tx, _ intake.Fact) (intake.Result, error) {
	rows, err := c.records.DispatchIssues(ctx, tx, orderProject)
	if err != nil {
		return intake.Result{}, err
	}
	for _, row := range rows {
		if row.Key == "LEGION-208" {
			c.holder, c.seq = row.ClaimedBy, row.ClaimSeq
		}
	}
	return intake.Result{}, nil
}

// mirrorReader is a handler standing where admission stands, last in the daemon's chain: it reads
// the project's whole mirror exactly as admission and the engine's own held-child pass do
// (record.Store.DispatchIssues), inside the transaction the fact is applied in, and keeps what it
// saw for the test.
type mirrorReader struct {
	records record.Store
	rows    map[string]blockers.Row
}

func (m *mirrorReader) Apply(ctx context.Context, tx pgx.Tx, _ intake.Fact) (intake.Result, error) {
	mirrored, err := m.records.DispatchIssues(ctx, tx, orderProject)
	if err != nil {
		return intake.Result{}, err
	}
	m.rows = blockers.Rows(mirrored)
	return intake.Result{}, nil
}

// mirroredBlockers is what the committed mirror says key waits on, and nil when key has no row.
func mirroredBlockers(t *testing.T, pool *pgxpool.Pool, records record.Store, key string) []string {
	t.Helper()
	var rows []record.DispatchIssueRow
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		rows, err = records.DispatchIssues(context.Background(), tx, orderProject)
		return err
	}); err != nil {
		t.Fatalf("read the mirror: %v", err)
	}
	for _, row := range rows {
		if row.Key == key {
			return row.Blockers
		}
	}
	return nil
}

func orderedIssue(t *testing.T, pool *pgxpool.Pool, records record.Store, key string) record.Issue {
	t.Helper()
	var issue *record.Issue
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		issue, err = records.Issue(context.Background(), tx, key)
		return err
	}); err != nil {
		t.Fatalf("read the record of %s: %v", key, err)
	}
	if issue == nil {
		t.Fatalf("%s is not recorded", key)
	}
	return *issue
}

func seedOrderedIssue(t *testing.T, pool *pgxpool.Pool, records record.Store, issue record.Issue) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutIssue(context.Background(), tx, issue)
	}); err != nil {
		t.Fatalf("seed %s: %v", issue.Key, err)
	}
}

// orderedPool is a migrated database of this test's own, dropped when it ends. It is
// intake_test.go's migratedPool, which the internal test package cannot lend this one.
func orderedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testpg.Database(t, "legion_intake_order_test")
	st, err := legionstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.Migrate(context.Background()); err != nil {
		st.Close()
		t.Fatalf("migrate store: %v", err)
	}
	st.Close()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
