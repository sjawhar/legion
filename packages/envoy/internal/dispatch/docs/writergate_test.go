package docs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A caller holding a pooled connection is refused a document's writer gate at once: the writer
// holding the gate may be waiting for a connection, or for a lock that caller's transaction holds.
func TestTheWriterGateRefusesACallerHoldingAConnection(t *testing.T) {
	service, artifactID := newTestService(t)
	ctx := store.WithTransactionTracking(context.Background())
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, _, err := service.AcquireDocumentWriter(ctx, artifactID); !errors.Is(err, ErrGateWhileHoldingConnection) {
		t.Fatalf("writer gate taken holding a transaction = %v, want ErrGateWhileHoldingConnection", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	// The refusal took nothing: the gate is free for the next writer.
	_, release, err := service.AcquireDocumentWriter(ctx, artifactID)
	if err != nil {
		t.Fatalf("writer gate taken once the connection went back = %v", err)
	}
	release()
}

// A live write joins a document only under that document's writer gate: not without one, not under
// another document's, and not under one its holder has released.
func TestJoinLiveWriteRefusesAnUngatedCaller(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "First.\n")
	other := createIssueDocument(t, service.store, 2, "# Other")
	actor := model.Actor{Kind: "user", ID: "alice"}
	ops := []model.EditOp{{Op: "insert", After: "end", Markdown: "Added."}}
	write := func(ctx context.Context) error {
		tx, err := service.store.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		joined, ledger := service.Join(ctx, tx)
		defer ledger.Discard()
		_, err = service.ApplyOps(joined, artifactID, ops, actor, nil)
		return err
	}

	if err := write(context.Background()); !errors.Is(err, ErrWriterGateNotHeld) {
		t.Errorf("a write holding no gate = %v, want ErrWriterGateNotHeld", err)
	}
	otherGated, releaseOther, err := service.AcquireDocumentWriter(context.Background(), other)
	if err != nil {
		t.Fatalf("take the other document's gate: %v", err)
	}
	if err := write(otherGated); !errors.Is(err, ErrWriterGateNotHeld) {
		t.Errorf("a write holding another document's gate = %v, want ErrWriterGateNotHeld", err)
	}
	releaseOther()
	gated, release, err := service.AcquireDocumentWriter(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("take the document's gate: %v", err)
	}
	if err := write(gated); err != nil {
		t.Errorf("a write holding the document's gate = %v, want it to apply", err)
	}
	release()
	if err := write(gated); !errors.Is(err, ErrWriterGateNotHeld) {
		t.Errorf("a write under a gate its holder released = %v, want ErrWriterGateNotHeld", err)
	}
}

// Writers take a document's gate in the order they asked for it.
func TestTheWriterGateAdmitsWritersInArrivalOrder(t *testing.T) {
	service, artifactID := newTestService(t)
	_, release, err := service.AcquireDocumentWriter(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("take the document's gate: %v", err)
	}
	const writers = 5
	order := make(chan int, writers)
	done := make(chan struct{}, writers)
	for index := range writers {
		go func() {
			_, release, err := service.AcquireDocumentWriter(context.Background(), artifactID)
			if err != nil {
				t.Errorf("writer %d: %v", index, err)
				done <- struct{}{}
				return
			}
			order <- index
			release()
			done <- struct{}{}
		}()
		waitForQueuedWriters(t, service, int64(index+1))
	}
	release()
	for range writers {
		<-done
	}
	close(order)
	next := 0
	for index := range order {
		if index != next {
			t.Fatalf("writer %d took the gate where writer %d was due", index, next)
		}
		next++
	}
}

// Writes to overlapping documents, named in opposite orders, never each hold a gate the other
// waits for: the gates are taken sorted.
func TestOverlappingMultiDocumentWritersDoNotDeadlock(t *testing.T) {
	service, first := newTestService(t)
	second := createIssueDocument(t, service.store, 2, "# Second")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	errs := make(chan error, 2)
	for _, ids := range [][]string{{first, second, first}, {second, first}} {
		go func() {
			for range 200 {
				_, release, err := service.AcquireDocumentWriter(ctx, ids...)
				if err != nil {
					errs <- err
					return
				}
				release()
			}
			errs <- nil
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("a writer of both documents = %v, want every take to succeed", err)
		}
	}
}

// A writer that has to wait for a document's gate gives its request's admission units back while
// it waits, so a write to another document finds room; once the gate passes to it, it takes its
// units back.
func TestAWriterQueuedForADocumentGivesItsAdmissionBack(t *testing.T) {
	service, artifactID := newTestService(t)
	limiter := admission.New(admission.Limits{Read: 1, Write: 1, Wait: 50 * time.Millisecond})
	_, release, err := service.AcquireDocumentWriter(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("take the document's gate: %v", err)
	}
	ticket, err := limiter.Admit(context.Background(), admission.Write, 1)
	if err != nil {
		t.Fatalf("admit the queued writer: %v", err)
	}
	defer ticket.Release()
	queued := make(chan error, 1)
	go func() {
		_, release, err := service.AcquireDocumentWriter(admission.WithTicket(context.Background(), ticket), artifactID)
		if err == nil {
			release()
		}
		queued <- err
	}()
	waitForQueuedWriters(t, service, 1)
	other, err := limiter.Admit(context.Background(), admission.Write, 1)
	if err != nil {
		t.Fatalf("a write admitted while the only write unit's holder queues for a document = %v, want it admitted", err)
	}
	other.Release()
	release()
	if err := <-queued; err != nil {
		t.Fatalf("the queued writer, once the gate passed to it = %v", err)
	}
	if _, err := limiter.Admit(context.Background(), admission.Write, 1); err == nil {
		t.Fatal("a write was admitted while the resumed writer holds the only write unit")
	}
}

// A writer whose admission units cannot be taken back once the gate passes to it is refused as
// admission refuses a request, and hands the gate on.
func TestAWriterThatCannotResumeItsAdmissionReleasesTheGate(t *testing.T) {
	service, artifactID := newTestService(t)
	limiter := admission.New(admission.Limits{Read: 1, Write: 1, Wait: 20 * time.Millisecond})
	_, release, err := service.AcquireDocumentWriter(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("take the document's gate: %v", err)
	}
	ticket, err := limiter.Admit(context.Background(), admission.Write, 1)
	if err != nil {
		t.Fatalf("admit the queued writer: %v", err)
	}
	defer ticket.Release()
	queued := make(chan error, 1)
	go func() {
		_, release, err := service.AcquireDocumentWriter(admission.WithTicket(context.Background(), ticket), artifactID)
		if err == nil {
			release()
		}
		queued <- err
	}()
	waitForQueuedWriters(t, service, 1)
	other, err := limiter.Admit(context.Background(), admission.Write, 1)
	if err != nil {
		t.Fatalf("admit the write that keeps the unit: %v", err)
	}
	defer other.Release()
	release()
	var refusal *admission.Refusal
	if err := <-queued; !errors.As(err, &refusal) {
		t.Fatalf("a writer that cannot take its unit back = %v, want admission's refusal", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, next, err := service.AcquireDocumentWriter(ctx, artifactID)
	if err != nil {
		t.Fatalf("the gate after the refused writer = %v, want it free", err)
	}
	next()
}
