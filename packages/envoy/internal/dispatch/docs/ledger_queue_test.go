package docs

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// queueProbe makes the table the queue's tests write their steps' rows to, so what a commit left is
// read back from the database rather than taken on the steps' word.
func queueProbe(t *testing.T, service *Service) {
	t.Helper()
	if _, err := service.store.Pool.Exec(context.Background(), `create table ledger_queue_probe (step int primary key)`); err != nil {
		t.Fatalf("create the probe table: %v", err)
	}
}

// probedSteps is what the probe table holds, in step order.
func probedSteps(t *testing.T, service *Service) []int {
	t.Helper()
	rows, err := service.store.Pool.Query(context.Background(), `select step from ledger_queue_probe order by step`)
	if err != nil {
		t.Fatalf("read the probe table: %v", err)
	}
	steps, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatalf("read the probe table: %v", err)
	}
	return steps
}

// probeStep is a step that records n as it runs, in ran and in the probe table.
func probeStep(n int, ran *[]int) WriteStep {
	return func(ctx context.Context, tx pgx.Tx) error {
		*ran = append(*ran, n)
		_, err := tx.Exec(ctx, `insert into ledger_queue_probe (step) values ($1)`, n)
		return err
	}
}

// joinProbe begins a transaction on service's pool and joins it, for a test of the queue itself.
func joinProbe(t *testing.T, service *Service) (pgx.Tx, *Ledger) {
	t.Helper()
	tx, err := service.store.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	_, ledger := service.Join(context.Background(), tx)
	t.Cleanup(ledger.Discard)
	return tx, ledger
}

// Commit runs the queued steps in the order they were queued, none of them before, and a step
// that queues another once the write phase has begun is refused, as is one queued after Commit.
func TestLedgerQueueRunsItsStepsInOrderAtCommit(t *testing.T) {
	service, _ := newTestService(t)
	queueProbe(t, service)
	_, ledger := joinProbe(t, service)
	var ran []int
	for _, n := range []int{3, 1, 2} {
		if err := ledger.Queue(probeStep(n, &ran)); err != nil {
			t.Fatalf("queue step %d: %v", n, err)
		}
	}
	if len(ran) != 0 {
		t.Fatalf("steps ran when queued: %v", ran)
	}
	var lateErr error
	if err := ledger.Queue(func(context.Context, pgx.Tx) error {
		lateErr = ledger.Queue(probeStep(9, &ran))
		return nil
	}); err != nil {
		t.Fatalf("queue the step that queues another: %v", err)
	}
	if err := ledger.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !errors.Is(lateErr, ErrQueueSealed) {
		t.Fatalf("a step queued during the write phase: %v, want ErrQueueSealed", lateErr)
	}
	if !slices.Equal(ran, []int{3, 1, 2}) {
		t.Fatalf("steps ran in order %v, want the queue's order [3 1 2]", ran)
	}
	if got := probedSteps(t, service); !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("committed rows %v, want [1 2 3]", got)
	}
	if err := ledger.Queue(probeStep(4, &ran)); !errors.Is(err, ErrQueueSealed) {
		t.Fatalf("a step queued after Commit: %v, want ErrQueueSealed", err)
	}
}

// The first failing step stops the write phase: Commit returns its error as the step returned it,
// no later step runs, and the transaction rolls back what the earlier steps wrote.
func TestLedgerQueueStopsAtTheFirstFailingStepAndRollsBack(t *testing.T) {
	service, _ := newTestService(t)
	queueProbe(t, service)
	_, ledger := joinProbe(t, service)
	refused := &ErrInvalidOp{Field: "probe", Reason: "the step's own refusal"}
	var ran []int
	for _, step := range []WriteStep{
		probeStep(1, &ran),
		func(context.Context, pgx.Tx) error { return refused },
		probeStep(2, &ran),
	} {
		if err := ledger.Queue(step); err != nil {
			t.Fatalf("queue: %v", err)
		}
	}
	err := ledger.Commit(context.Background())
	var invalid *ErrInvalidOp
	if err != error(refused) || !errors.As(err, &invalid) || invalid != refused {
		t.Fatalf("commit returned %v, want the failing step's typed error unchanged", err)
	}
	if !slices.Equal(ran, []int{1}) {
		t.Fatalf("steps that ran: %v, want only the one before the failure", ran)
	}
	if got := probedSteps(t, service); len(got) != 0 {
		t.Fatalf("a failed write phase left rows %v", got)
	}
	if err := ledger.Queue(probeStep(3, &ran)); !errors.Is(err, ErrQueueSealed) {
		t.Fatalf("a step queued after a failed Commit: %v, want ErrQueueSealed", err)
	}
}

// Discard drops the queue: nothing it held runs, even when the transaction then commits, and the
// ledger takes no more steps.
func TestLedgerDiscardRunsNothingItQueued(t *testing.T) {
	service, _ := newTestService(t)
	queueProbe(t, service)
	tx, ledger := joinProbe(t, service)
	var ran []int
	if err := ledger.Queue(probeStep(1, &ran)); err != nil {
		t.Fatalf("queue: %v", err)
	}
	ledger.Discard()
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit the bare transaction: %v", err)
	}
	if len(ran) != 0 {
		t.Fatalf("Discard ran steps %v", ran)
	}
	if got := probedSteps(t, service); len(got) != 0 {
		t.Fatalf("Discard left rows %v", got)
	}
	if err := ledger.Queue(probeStep(2, &ran)); !errors.Is(err, ErrQueueSealed) {
		t.Fatalf("a step queued after Discard: %v, want ErrQueueSealed", err)
	}
}

// A joined document write - an edit, its named version, their anchor refresh, version index and
// approval move - takes its locks, does its reads and its document work, and writes nothing until
// Commit: until then its transaction has no transaction id, which only a write or a row lock gives
// it. Commit writes it all.
func TestAJoinedDocumentWriteHoldsNoTransactionIDUntilCommit(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "Alpha paragraph cites dispatch://DOC-1.\n")
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	actor := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	if _, err := service.ApplyOps(joined, artifactID, []model.EditOp{{Op: "replace", Find: "Alpha", With: "Beta"}}, actor, nil); err != nil {
		t.Fatalf("apply ops: %v", err)
	}
	written, err := service.NamedVersion(joined, artifactID, "Beta", actor)
	if err != nil {
		t.Fatalf("named version: %v", err)
	}
	var assigned bool
	if err := tx.QueryRow(ctx, `select pg_current_xact_id_if_assigned() is not null`).Scan(&assigned); err != nil {
		t.Fatalf("read the transaction id: %v", err)
	}
	if assigned {
		t.Fatal("the joined write's transaction holds a transaction id before Commit: a document write ran in its read phase")
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var markdown string
	var named bool
	if err := service.store.Pool.QueryRow(ctx, `
		select markdown, named from artifact_versions where artifact_id = $1 and number = $2
	`, artifactID, written.Version.Number).Scan(&markdown, &named); err != nil {
		t.Fatalf("read the committed version %d: %v", written.Version.Number, err)
	}
	if !named || markdown != "Beta paragraph cites dispatch://DOC-1.\n" {
		t.Fatalf("committed version: named=%v markdown=%q", named, markdown)
	}
}

// T7: one transaction marking a quote and then projecting the mark's margin record - two live
// operations on one room - judges its room not behind at the second operation, though the first's
// append is still queued, so the second forks warm from the room rather than loading the stored
// document; and both appends land as consecutive versions of the document's history.
func TestOwnPendingAppendsKeepTheRoomAtTheHead(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "The quick brown fox\n")
	// The room is resident, so its cursor gates the transaction's appends.
	if _, err := service.Text(context.Background(), artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("warm the room: %v", err)
	}
	head := func() int64 {
		t.Helper()
		cursor, err := currentUpdateCursor(context.Background(), service.store.Pool, artifactID)
		if err != nil {
			t.Fatalf("read the head: %v", err)
		}
		return cursor
	}
	before := head()
	var forks []bool
	service.afterForkRead = func(room string) {
		if room != artifactID {
			return
		}
		state := service.room(artifactID)
		state.mu.Lock()
		write := state.liveWriter
		state.mu.Unlock()
		forks = append(forks, write != nil && write.cold)
	}
	defer func() { service.afterForkRead = nil }()
	alice := model.Actor{Kind: "user", ID: "alice"}
	if err := joinedWrite(service, func(ctx context.Context) error {
		if _, err := service.MarkQuote(ctx, artifactID, MarkSpec{Kind: MarkComment, ID: "c1", By: alice}, "brown", nil); err != nil {
			return err
		}
		return service.ProjectMark(ctx, artifactID, "c1", MarkRecord{Kind: "comment", By: "alice", Text: "Colour?"}, alice)
	}); err != nil {
		t.Fatalf("mark and project in one transaction: %v", err)
	}
	if len(forks) != 2 || forks[0] || forks[1] {
		t.Fatalf("forks (cold?) = %v, want two warm forks: a room judged behind its head forks cold", forks)
	}
	if after := head(); after != before+2 {
		t.Fatalf("head after the transaction = %d, want %d: its two appends as consecutive versions", after, before+2)
	}
}
