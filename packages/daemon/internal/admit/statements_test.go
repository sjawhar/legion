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

// statementCounter counts every statement a pool's connections send.
type statementCounter struct{ sent atomic.Int64 }

func (c *statementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.sent.Add(1)
	return ctx
}

func (c *statementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// countedPool is a pool on pool's database whose statements counter counts.
func countedPool(t *testing.T, pool *pgxpool.Pool, counter *statementCounter) *pgxpool.Pool {
	t.Helper()
	config := pool.Config().Copy()
	config.ConnConfig.Tracer = counter
	counted, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open the counted pool: %v", err)
	}
	t.Cleanup(counted.Close)
	return counted
}

func slottedKey(i int) string { return fmt.Sprintf("LEGION-%d", 1000+i) }

// Every fact runs admission under the one fact lock (intake.ApplyFact), the facts of the Dispatch
// and GitHub consumers, the API and the boot's replay alike, so what admission asks the database
// is paid by every fact. It asks the same number of statements however many slots the project
// holds: a statement per slotted issue cost a project running hundreds of trees hundreds of round
// trips on each fact, and the Dispatch consumer, whose events are mostly facts of recorded
// issues, fell behind. Releasing a finished tree's slot still reads every slot: another project's
// slot in the shared table is left as it is.
func TestAFactAsksAsManyStatementsWhateverNumberOfSlotsIsHeld(t *testing.T) {
	facts := []struct {
		name string
		fact intake.Fact
	}{
		{"a document version of a slotted root", intake.DispatchArtifact{Key: slottedKey(0), ArtifactID: "spec", Kind: intake.DispatchArtifactVersion, Version: 2}},
		{"an observation of a slotted root", intake.DispatchIssue{Key: slottedKey(0), Seq: 5, Type: "issue.updated", Status: "in_progress",
			Title: slottedKey(0), Rank: "A0000", HandedOver: handed, ResolvedRepo: "acme/widgets"}},
		{"an observation of an issue Legion does not record", intake.DispatchIssue{Key: "LEGION-9", Seq: 5, Type: "issue.updated", Status: "backlog",
			Title: "someone else's", Rank: "Z", ResolvedRepo: "acme/widgets"}},
	}
	run := func(t *testing.T, slots int) (map[string]int64, *pgxpool.Pool, []intake.Handler) {
		t.Helper()
		pool := migratedPool(t)
		for i := range slots {
			seedSlotted(t, pool, slottedKey(i), fmt.Sprintf("A%04d", i))
		}
		counter := &statementCounter{}
		counted := countedPool(t, pool, counter)
		admission := newAdmission(t, 2*slots+2, slog.New(slog.NewTextHandler(io.Discard, nil)))
		handlers := []intake.Handler{blockers.NewMirror(record.NewStore(), testProject), admission.engine, admission}
		sent := map[string]int64{}
		for _, tc := range facts {
			counter.sent.Store(0)
			if _, err := intake.ApplyFact(context.Background(), counted, "dispatch", tc.name, tc.fact, handlers...); err != nil {
				t.Fatalf("apply %s with %d slots held: %v", tc.name, slots, err)
			}
			sent[tc.name] = counter.sent.Load()
		}
		return sent, pool, handlers
	}

	few, _, _ := run(t, 1)
	many, pool, handlers := run(t, 40)
	for _, tc := range facts {
		if many[tc.name] != few[tc.name] {
			t.Errorf("%s: %d statements with 40 slots held, %d with 1; want as many", tc.name, many[tc.name], few[tc.name])
		}
	}

	finished := issue(t, pool, slottedKey(7))
	finished.Phase, finished.Status = phase.Done, "done"
	putIssue(t, pool, finished)
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		if err := records.PutIssue(context.Background(), tx, record.Issue{Key: "OTHER-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: "OTHER", Title: "OTHER-1", Tree: "OTHER-1", Phase: phase.Done, Generation: 1, Status: "done", Rank: "A", HandedOver: true}); err != nil {
			t.Fatalf("put another project's root: %v", err)
		}
		if err := records.PutSlot(context.Background(), tx, record.Slot{Issue: "OTHER-1", Index: 100, AdmittedAt: fixedNow}); err != nil {
			t.Fatalf("put another project's slot: %v", err)
		}
	})
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "after-the-close", facts[0].fact, handlers...); err != nil {
		t.Fatalf("apply a fact after %s finished: %v", slottedKey(7), err)
	}
	want := []record.Slot{}
	for i := range 40 {
		if i != 7 {
			want = append(want, record.Slot{Issue: slottedKey(i), Index: i, AdmittedAt: fixedNow})
		}
	}
	assertSlots(t, pool, append(want, record.Slot{Issue: "OTHER-1", Index: 100, AdmittedAt: fixedNow}))
}
