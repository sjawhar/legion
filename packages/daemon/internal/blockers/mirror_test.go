package blockers

import (
	"context"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	legionstore "github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/testpg"
)

const testProject = "LEGION"

// The mirror writes the row of a Dispatch issue event and nothing else: the status, the keys the
// event says the issue waits on, and the sequence it was observed at, under the daemon's project.
func TestMirrorWritesTheRowOfADispatchIssueFact(t *testing.T) {
	pool := migratedPool(t)
	records := record.NewStore()
	mirror := NewMirror(records, testProject)
	fact := intake.DispatchIssue{
		Key: "LEGION-900", Seq: 4, Status: "todo", Title: "a blocked child",
		Blockers: []string{"LEGION-901", "LEGION-902"},
	}
	inTx(t, pool, func(tx pgx.Tx) {
		result, err := mirror.Apply(context.Background(), tx, fact)
		if err != nil {
			t.Fatalf("apply the issue fact: %v", err)
		}
		if result.Refusal != nil {
			t.Fatalf("the mirror refused %#v; it decides nothing", result.Refusal)
		}
	})
	want := []record.DispatchIssueRow{{
		Key: "LEGION-900", Project: testProject, Status: "todo",
		Blockers: []string{"LEGION-901", "LEGION-902"}, Seq: 4,
	}}
	if got := mirrored(t, pool, records); !reflect.DeepEqual(got, want) {
		t.Fatalf("the mirror holds %#v, want %#v", got, want)
	}
}

// Every other kind of fact passes through the mirror untouched: it is one handler among three,
// and only an issue event, or the periodic turn's own re-read, says anything about what an issue
// waits on.
func TestMirrorWritesNothingForAnotherFactKind(t *testing.T) {
	pool := migratedPool(t)
	records := record.NewStore()
	mirror := NewMirror(records, testProject)
	inTx(t, pool, func(tx pgx.Tx) {
		result, err := mirror.Apply(context.Background(), tx, intake.RepoMappingChanged{Project: testProject})
		if err != nil {
			t.Fatalf("apply the mapping change: %v", err)
		}
		if result.Refusal != nil {
			t.Fatalf("the mirror refused %#v; it decides nothing", result.Refusal)
		}
	})
	if got := mirrored(t, pool, records); len(got) != 0 {
		t.Fatalf("the mirror holds %#v after a fact that is no issue event, want nothing", got)
	}
}

// The periodic turn's re-read rides on the controller tick, and every row it observed is written
// through the same fence an event's row takes: a view fresher than the row replaces it, and one
// older than a row an event already wrote leaves that row exactly as it stands.
func TestMirrorWritesEveryRowAControllerTickObserved(t *testing.T) {
	pool := migratedPool(t)
	records := record.NewStore()
	mirror := NewMirror(records, testProject)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := records.PutDispatchIssue(context.Background(), tx, record.DispatchIssueRow{
			Key: "LEGION-900", Project: testProject, Status: "in_progress", Seq: 9}); err != nil {
			t.Fatalf("seed a fresher event's row: %v", err)
		}
		result, err := mirror.Apply(context.Background(), tx, intake.ControllerTick{Observed: []intake.IssueStatus{
			{Key: "LEGION-900", Status: "todo", Seq: 4},
			{Key: "LEGION-901", Status: "done", Blockers: []string{"LEGION-902"}, Seq: 6},
		}})
		if err != nil {
			t.Fatalf("apply the controller tick: %v", err)
		}
		if result.Refusal != nil {
			t.Fatalf("the mirror refused %#v; it decides nothing", result.Refusal)
		}
	})
	want := []record.DispatchIssueRow{
		{Key: "LEGION-900", Project: testProject, Status: "in_progress", Blockers: []string{}, Seq: 9},
		{Key: "LEGION-901", Project: testProject, Status: "done", Blockers: []string{"LEGION-902"}, Seq: 6},
	}
	if got := mirrored(t, pool, records); !reflect.DeepEqual(got, want) {
		t.Fatalf("the mirror holds %#v, want %#v", got, want)
	}
}

// A turn with nothing waiting reads nothing from Dispatch, so its tick observes nothing and
// writes nothing: that is the tick exactly as it was before the re-read existed, which is almost
// every turn.
func TestMirrorWritesNothingForATickThatObservedNothing(t *testing.T) {
	pool := migratedPool(t)
	records := record.NewStore()
	mirror := NewMirror(records, testProject)
	inTx(t, pool, func(tx pgx.Tx) {
		result, err := mirror.Apply(context.Background(), tx, intake.ControllerTick{})
		if err != nil {
			t.Fatalf("apply the controller tick: %v", err)
		}
		if result.Refusal != nil {
			t.Fatalf("the mirror refused %#v; it decides nothing", result.Refusal)
		}
	})
	if got := mirrored(t, pool, records); len(got) != 0 {
		t.Fatalf("the mirror holds %#v after a tick that observed nothing, want nothing", got)
	}
}

// Reconcile is the boot listing's own write of every summary, through the same fenced write: a
// listing older than an event already applied leaves that event's row alone.
func TestReconcileMirrorsEverySummaryUnderTheFence(t *testing.T) {
	pool := migratedPool(t)
	records := record.NewStore()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := records.PutDispatchIssue(context.Background(), tx, record.DispatchIssueRow{
			Key: "LEGION-900", Project: testProject, Status: "in_progress", Seq: 9}); err != nil {
			t.Fatalf("seed a fresher event's row: %v", err)
		}
		if err := Reconcile(context.Background(), tx, records, testProject, []dispatch.IssueSummary{
			{Key: "LEGION-900", Status: "todo", LastSeq: 4},
			{Key: "LEGION-901", Status: "done", LastSeq: 6, BlockedBy: []string{"LEGION-902"}},
		}); err != nil {
			t.Fatalf("reconcile the listing: %v", err)
		}
	})
	want := []record.DispatchIssueRow{
		{Key: "LEGION-900", Project: testProject, Status: "in_progress", Blockers: []string{}, Seq: 9},
		{Key: "LEGION-901", Project: testProject, Status: "done", Blockers: []string{"LEGION-902"}, Seq: 6},
	}
	if got := mirrored(t, pool, records); !reflect.DeepEqual(got, want) {
		t.Fatalf("the mirror holds %#v, want %#v", got, want)
	}
}

func mirrored(t *testing.T, pool *pgxpool.Pool, records record.Store) []record.DispatchIssueRow {
	t.Helper()
	var rows []record.DispatchIssueRow
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if rows, err = records.DispatchIssues(context.Background(), tx, testProject); err != nil {
			t.Fatalf("list the mirror: %v", err)
		}
	})
	return rows
}

func inTx(t *testing.T, pool *pgxpool.Pool, fn func(pgx.Tx)) {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	defer func() {
		if err := tx.Rollback(context.Background()); err != nil && err != pgx.ErrTxClosed {
			t.Errorf("rollback transaction: %v", err)
		}
	}()
	fn(tx)
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit transaction: %v", err)
	}
}

// migratedPool is a migrated database of this test's own: the mirror's writes are SQL — an upsert
// fenced in its own where clause — so they are tested against the real schema, as every other
// package's record tests are.
func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := testpg.Database(t, "legion_blockers_test")
	st, err := legionstore.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	if _, err := st.Migrate(context.Background()); err != nil {
		st.Close()
		t.Fatalf("migrate the test database: %v", err)
	}
	st.Close()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open a pool on the test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
