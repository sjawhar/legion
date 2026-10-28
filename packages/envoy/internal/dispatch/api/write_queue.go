package api

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// A route joined to a document ledger (Docs.Join) runs its transaction in two phases (INFRA-636):
// a read phase that takes its locks, reads and does the document work holding no transaction id,
// and a write phase, Ledger.Commit, that sends every write as one pipelined batch and commits. The
// document operations queue their writes on the ledger; the route queues its own beside them with
// these helpers, at the point it makes each write, so the batch carries the route's statements and
// the document operations' in the order the route made them, and the events they append keep that
// order. No statement's result is read in Go before a later statement is sent: a value a later
// statement needs - an appended event's id for its stamp - is named in that statement's SQL
// (store.LatestEventID), and a value the route needs after Commit - the appended event - is
// written through a pointer the statement's callback fills in.

// queueWrite runs step in tx's write phase: queued on ledger after every write queued so far, or
// at once, as a batch of its own, where the transaction runs no queue (a nil ledger: a caller that
// joined no ledger, as createArtifactReview's answer of an approval ask). A failing statement or a
// callback's error stops the write phase and Ledger.Commit returns it unchanged, so the route
// answers it as it would a statement run at once.
func queueWrite(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, step docs.WriteStep) error {
	if ledger == nil {
		return store.RunWrite(ctx, tx, step)
	}
	return ledger.Queue(step)
}

// queueExec queues one statement whose result nothing reads. Its failure comes back unwrapped, as
// tx.Exec's would.
func queueExec(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, sql string, args ...any) error {
	return queueWrite(ctx, tx, ledger, func(b *store.WriteBatch) error {
		b.Queue("", sql, args...)
		return nil
	})
}

// queueEvent queues event's append (events.Broker.QueueAppend). The returned event is the appended
// one, with its id, sequence and time, once Commit has run the write phase: the route publishes it
// after Commit (appendedEvents).
func (s *server) queueEvent(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, event model.Event) (*model.Event, error) {
	return s.queueStampedEvent(ctx, tx, ledger, event, "", "")
}

// queueStampedEvent is queueEvent followed, in the same step, by the stamp of fromKind/fromID's new
// reference edges with the event's id (refs.QueueStamp), which the stamp names as the id sequence
// left it: nothing comes between the event's insert and the stamp. An empty fromKind stamps
// nothing.
func (s *server) queueStampedEvent(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, event model.Event, fromKind, fromID string) (*model.Event, error) {
	appended := new(model.Event)
	return appended, queueWrite(ctx, tx, ledger, func(b *store.WriteBatch) error {
		if err := s.deps.Events.QueueAppend(b, event, func(written model.Event) { *appended = written }); err != nil {
			return err
		}
		if fromKind != "" {
			refs.QueueStamp(b, fromKind, fromID)
		}
		return nil
	})
}

// queueReferences is replaceReferences split in two: it plans the reconciliation of
// fromKind/fromID's references with body (planReferences), here, and queues the delete and insert
// that write it.
func (s *server) queueReferences(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, fromKind, fromID, body string) (model.ReferenceChanges, error) {
	replacement, changes, err := s.planReferences(ctx, tx, fromKind, fromID, body)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	return changes, queueWrite(ctx, tx, ledger, func(b *store.WriteBatch) error {
		replacement.Queue(b)
		return nil
	})
}

// planReferences is replaceReferences' read half: the reconciliation of fromKind/fromID's
// references with body, planned against the edges the source holds, and the counted targets it
// moves, for the caller to queue (Replacement.Queue) in its write phase. The resolution reads
// issues and asks, which the reconciliation does not write, so it reads what it read after the
// write. No document operation reconciles a comment's, an ask's or a message's references, so the
// source's held edges are the stored ones.
func (s *server) planReferences(ctx context.Context, tx pgx.Tx, fromKind, fromID, body string) (refs.Replacement, model.ReferenceChanges, error) {
	replacement, err := refs.PlanReplace(ctx, tx, fromKind, fromID, text.Extract(body, s.deps.ServerURL), nil)
	if err != nil {
		return refs.Replacement{}, model.ReferenceChanges{}, err
	}
	changes, err := refs.CountedChanges(ctx, tx, replacement.Changed())
	if err != nil {
		return refs.Replacement{}, model.ReferenceChanges{}, err
	}
	return replacement, changes, nil
}

// appendedEvents is the events queueEvent appended, read once Commit has run the write phase, in
// the order given.
func appendedEvents(events ...*model.Event) []model.Event {
	appended := make([]model.Event, len(events))
	for index, event := range events {
		appended[index] = *event
	}
	return appended
}
