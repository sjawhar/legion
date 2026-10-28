package api

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// A route joined to a document ledger (Docs.Join) runs its transaction in two phases (INFRA-636):
// a read phase that takes its locks, reads and does the document work holding no transaction id,
// and a write phase, Ledger.Commit, that runs every write back to back and commits. The document
// operations queue their writes on the ledger; the route queues its own beside them with these
// helpers, at the point it makes each write, so the write phase replays the route's statements and
// the document operations' in the order the route made them, and the events they append keep that
// order. A value the route needs from a write - an appended event's id - is written through a
// pointer the step fills in; only steps queued after it and code after Commit read it.

// queueWrite runs step in tx's write phase: queued on ledger after every write queued so far, or
// at once where the transaction runs no queue (a nil ledger: a caller that joined no ledger, as
// createArtifactReview's answer of an approval ask). A step's error stops the write phase and
// Ledger.Commit returns it unchanged, so the route answers it as it would a statement run at once.
func queueWrite(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, step docs.WriteStep) error {
	if ledger == nil {
		return step(ctx, tx)
	}
	return ledger.Queue(step)
}

// queueExec queues one statement whose result nothing reads.
func queueExec(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, sql string, args ...any) error {
	return queueWrite(ctx, tx, ledger, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

// queueEvent queues event's append (appendEvent). The returned event is the appended one, with its
// id, sequence and time, once the step has run: a step queued after it reads it there (queueStamp),
// and the route publishes it after Commit (appendedEvents).
func (s *server) queueEvent(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, event model.Event) (*model.Event, error) {
	appended := new(model.Event)
	return appended, queueWrite(ctx, tx, ledger, func(ctx context.Context, tx pgx.Tx) error {
		written, err := s.appendEvent(ctx, tx, event)
		if err != nil {
			return err
		}
		*appended = written
		return nil
	})
}

// queueStamp queues refs.Stamp of fromKind/fromID's new edges with event's id. It is queued after
// event's own step (queueEvent), which has filled event in by the time this one runs.
func queueStamp(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, fromKind, fromID string, event *model.Event) error {
	return queueWrite(ctx, tx, ledger, func(ctx context.Context, tx pgx.Tx) error {
		return refs.Stamp(ctx, tx, fromKind, fromID, event.ID)
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
	return changes, queueWrite(ctx, tx, ledger, func(ctx context.Context, tx pgx.Tx) error {
		return replacement.Apply(ctx, tx)
	})
}

// planReferences is replaceReferences' read half: the reconciliation of fromKind/fromID's
// references with body, planned against the edges the source holds, and the counted targets it
// moves, for the caller to write (Replacement.Apply) in its write phase. The resolution reads
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

// appendedEvents is the events queueEvent appended, read once Commit has run their steps, in the
// order given.
func appendedEvents(events ...*model.Event) []model.Event {
	appended := make([]model.Event, len(events))
	for index, event := range events {
		appended[index] = *event
	}
	return appended
}
