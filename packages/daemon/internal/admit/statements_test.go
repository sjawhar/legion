package admit

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
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
// issues, fell behind. Finished trees' slots are still released in slot order, whatever order the
// records come in, and another project's slot in the shared table is neither released nor counted.
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
	run := func(t *testing.T, held int) ([]int64, *pgxpool.Pool) {
		t.Helper()
		pool := migratedPool(t)
		for i := range held {
			seedSlotted(t, pool, slottedKey(i), fmt.Sprintf("A%04d", i))
		}
		counter := &statementCounter{}
		counted := countedPool(t, pool, counter)
		admission := newAdmission(t, 2*held+2, slog.New(slog.NewTextHandler(io.Discard, nil)))
		handlers := []intake.Handler{blockers.NewMirror(record.NewStore(), testProject), admission.engine, admission}
		sent := make([]int64, len(facts))
		for i, tc := range facts {
			counter.sent.Store(0)
			if _, err := intake.ApplyFact(context.Background(), counted, "dispatch", tc.name, tc.fact, handlers...); err != nil {
				t.Fatalf("apply %s with %d slots held: %v", tc.name, held, err)
			}
			if sent[i] = counter.sent.Load(); sent[i] == 0 {
				t.Fatalf("apply %s with %d slots held: the counted pool saw no statement", tc.name, held)
			}
		}
		return sent, pool
	}

	const slotsHeld = 40
	few, _ := run(t, 1)
	many, pool := run(t, slotsHeld)
	for i, tc := range facts {
		if many[i] != few[i] {
			t.Errorf("%s: %d statements with %d slots held, %d with 1; want as many", tc.name, many[i], slotsHeld, few[i])
		}
	}

	// LEGION-100 sorts before every other slotted key but holds the project's last slot, so the two
	// releases come in slot order only if releaseSlots keeps it rather than the records' key order.
	seedSlotted(t, pool, "LEGION-100", "B")
	for _, key := range []string{slottedKey(7), "LEGION-100"} {
		finished := issue(t, pool, key)
		finished.Phase, finished.Status = phase.Done, "done"
		putIssue(t, pool, finished)
	}
	putIssue(t, pool, record.Issue{Key: "OTHER-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "OTHER", Title: "OTHER-1", Tree: "OTHER-1", Phase: phase.Done, Generation: 1, Status: "done", Rank: "A", HandedOver: true})
	others := record.Slot{Issue: "OTHER-1", Index: 100, AdmittedAt: fixedNow}
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutSlot(context.Background(), tx, others); err != nil {
			t.Fatalf("put another project's slot: %v", err)
		}
	})
	var logged bytes.Buffer
	admission := newAdmission(t, 2*slotsHeld+2, slog.New(slog.NewTextHandler(&logged, nil)))
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "after-the-close", facts[0].fact,
		blockers.NewMirror(record.NewStore(), testProject), admission.engine, admission); err != nil {
		t.Fatalf("apply a fact after %s and LEGION-100 finished: %v", slottedKey(7), err)
	}
	var want []record.Slot
	for i := range slotsHeld {
		if i != 7 {
			want = append(want, record.Slot{Issue: slottedKey(i), Index: i, AdmittedAt: fixedNow})
		}
	}
	assertSlots(t, pool, append(want, others))
	var released []string
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		if strings.Contains(line, `msg="admission: slot released"`) {
			released = append(released, line[strings.Index(line, "issue="):])
		}
	}
	wantReleased := []string{
		fmt.Sprintf("issue=%s slots=%d cap=%d", slottedKey(7), slotsHeld, 2*slotsHeld+2),
		fmt.Sprintf("issue=LEGION-100 slots=%d cap=%d", slotsHeld-1, 2*slotsHeld+2),
	}
	if !slices.Equal(released, wantReleased) {
		t.Fatalf("released %q, want %q: slot order, counting this project's slots alone", released, wantReleased)
	}
}
