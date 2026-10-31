package api

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// A route that writes runs its transaction in two phases (INFRA-636): a read phase that takes its
// locks, reads and does the document work holding no transaction id, and a write phase,
// Ledger.Commit, that sends every write as one pipelined batch and commits. Every such route joins
// a document ledger (Docs.Join), whatever document work it does: the document operations queue
// their writes on the ledger, and the route queues its own beside them with these helpers, at the
// point it makes each write (Ledger.Queue), so the batch carries the route's statements and the
// document operations' in the order the route made them, and the events they append keep that
// order. No statement's result is read in Go before a later statement is sent: a value a later
// statement needs - an appended event's id for its stamp - is named in that statement's SQL
// (store.LatestEventID), and a value the route needs after Commit - the appended event - is
// written through a pointer the statement's callback fills in.
//
// A failing statement or a callback's error stops the write phase and Ledger.Commit returns it
// unchanged, so the route answers it as it would a statement run at once. A write a transaction
// no ledger joined makes - one that writes outside a route's two phases, as the claim cycle's
// does - runs at once instead, through store.RunWrite.

// queueExec queues one statement whose result nothing reads. Its failure comes back unwrapped, as
// tx.Exec's would.
func queueExec(ledger *docs.Ledger, sql string, args ...any) error {
	return ledger.Queue(func(b *store.WriteBatch) error {
		b.Queue("", sql, args...)
		return nil
	})
}

// queueEvent queues event's append (events.Broker.QueueAppend). The returned event is the appended
// one, with its id, sequence and time, once Commit has run the write phase: the route publishes it
// after Commit (appendedEvents).
func (s *server) queueEvent(ledger *docs.Ledger, event model.Event) (*model.Event, error) {
	return s.queueStampedEvent(ledger, event, "", "")
}

// queueStampedEvent is queueEvent followed, in the same step, by the stamp of fromKind/fromID's new
// reference edges with the event's id (refs.QueueStamp), which the stamp names as the id sequence
// left it: nothing comes between the event's insert and the stamp. An empty fromKind stamps
// nothing.
func (s *server) queueStampedEvent(ledger *docs.Ledger, event model.Event, fromKind, fromID string) (*model.Event, error) {
	return s.queueLoggedEvent(ledger, event, nil, fromKind, fromID)
}

// queueLoggedEvent is queueStampedEvent for an event whose payload names thread_logins, which the
// event's insert reads (events.Broker.QueueAppendWithLogins): the logins the route's earlier
// queued writes, and the user_threads triggers they fire, leave, as a read after those writes
// would see them. A nil logins names none.
func (s *server) queueLoggedEvent(ledger *docs.Ledger, event model.Event, logins *events.Logins, fromKind, fromID string) (*model.Event, error) {
	appended := new(model.Event)
	return appended, ledger.Queue(func(b *store.WriteBatch) error {
		if err := s.deps.Events.QueueAppendWithLogins(b, event, logins, func(written model.Event) { *appended = written }); err != nil {
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
	return changes, queueReplacement(ledger, replacement)
}

// queueEditedReferences is queueReferences for an edit of an ask's question or a comment's body
// from previous to body: the counted targets also name every target the source still cites when
// the edit moves the head of its text, which each of its edges shows (refs.EditedTargets).
func (s *server) queueEditedReferences(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, fromKind, fromID, previous, body string) (model.ReferenceChanges, error) {
	found := text.Extract(body, s.deps.ServerURL)
	replacement, changes, err := planFoundReferences(ctx, tx, fromKind, fromID, found, refs.EditedTargets(previous, body, found))
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	return changes, queueReplacement(ledger, replacement)
}

// queueItemReferences is refs.ReplaceCountedItem split in two, for a comment or an issue message
// this transaction writes at `at`: it plans the index of the item's blocks and the mentions each
// holds (refs.PlanItemIndex) and the reconciliation of the item's own references, resolves the
// counted targets both move, and queues the index's statements and the reconciliation's, in that
// order, in tx's write phase. A direct message (cites false) writes no mention, so its blocks
// hold none either: its blocks alone are indexed, and it moves nothing.
func (s *server) queueItemReferences(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, kind refs.ItemKind, itemID, body string, at time.Time, cites bool) (model.ReferenceChanges, error) {
	changes, write, err := s.planItemWrite(ctx, tx, kind, itemID, body, at, cites, nil, nil)
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	return changes, ledger.Queue(write)
}

// queueEditedItemReferences is queueItemReferences for an edit of a comment from previous to
// body, written at `at`, with the block ids the editor carried (carried; nil: the ids follow the
// text, refs.IndexItem). The counted targets also name every target the comment still cites when
// the edit moves the head of its text, which each of its edges shows (refs.EditedTargets). A
// carried id the index refuses returns its *itemblocks.CarriedError.
func (s *server) queueEditedItemReferences(ctx context.Context, tx pgx.Tx, ledger *docs.Ledger, kind refs.ItemKind, itemID, previous, body string, at time.Time, carried *refs.CarriedEdit) (model.ReferenceChanges, error) {
	changes, write, err := s.planItemWrite(ctx, tx, kind, itemID, body, at, true, carried, func(found []text.Ref) []refs.Target {
		return refs.EditedTargets(previous, body, found)
	})
	if err != nil {
		return model.ReferenceChanges{}, err
	}
	return changes, ledger.Queue(write)
}

// planItemWrite plans the item's index and, when the item cites its mentions, the reconciliation
// of its own references, counts what they and edited (the targets an edit moves although its
// edges stay; nil for a first write) move, and returns those counts with the step that writes
// both, for the route to queue where it makes the write: on its ledger (queueItemReferences), or
// on the batch of a transaction that runs its write phase itself (createStoredMessage).
func (s *server) planItemWrite(
	ctx context.Context,
	tx pgx.Tx,
	kind refs.ItemKind,
	itemID, body string,
	at time.Time,
	cites bool,
	carried *refs.CarriedEdit,
	edited func(found []text.Ref) []refs.Target,
) (model.ReferenceChanges, docs.WriteStep, error) {
	index, err := refs.ReadItemIndex(ctx, tx, kind, itemID)
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	if edited != nil {
		// The update that sets edited_at is queued, so the read above does not show it.
		index = index.Edited()
	}
	var located []text.Located
	if cites {
		located = text.ExtractAt(body, s.deps.ServerURL)
	}
	indexing, err := refs.PlanItemIndex(index, kind, itemID, body, at, located, carried)
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	var replacement refs.Replacement
	var changes model.ReferenceChanges
	if cites {
		found := text.Refs(located)
		moved := indexing.Moved()
		if edited != nil {
			moved = slices.Concat(moved, edited(found))
		}
		if replacement, changes, err = planFoundReferences(ctx, tx, string(kind), itemID, found, moved); err != nil {
			return model.ReferenceChanges{}, nil, err
		}
	}
	return changes, func(b *store.WriteBatch) error {
		indexing.Queue(b)
		if cites {
			replacement.Queue(b)
		}
		return nil
	}, nil
}

// queueReplacement queues replacement's delete and insert in the transaction's write phase.
func queueReplacement(ledger *docs.Ledger, replacement refs.Replacement) error {
	return ledger.Queue(func(b *store.WriteBatch) error {
		replacement.Queue(b)
		return nil
	})
}

// planReferences is replaceReferences' read half: the reconciliation of fromKind/fromID's
// references with body, planned against the edges the source holds, and the counted targets it
// moves, for the caller to queue (Replacement.Queue) in its write phase.
func (s *server) planReferences(ctx context.Context, tx pgx.Tx, fromKind, fromID, body string) (refs.Replacement, model.ReferenceChanges, error) {
	return planFoundReferences(ctx, tx, fromKind, fromID, text.Extract(body, s.deps.ServerURL), nil)
}

// planFoundReferences plans the reconciliation of fromKind/fromID's references with found and
// resolves the counted targets it and moved move. The resolution reads issues, asks and
// artifacts, which the reconciliation does not write, so it reads what it read after the write.
// No document operation reconciles a comment's, an ask's or a message's references, so the
// source's held edges are the stored ones.
func planFoundReferences(ctx context.Context, tx pgx.Tx, fromKind, fromID string, found []text.Ref, moved []refs.Target) (refs.Replacement, model.ReferenceChanges, error) {
	replacement, err := refs.PlanReplace(ctx, tx, fromKind, fromID, found, nil)
	if err != nil {
		return refs.Replacement{}, model.ReferenceChanges{}, err
	}
	changes, err := refs.CountedChanges(ctx, tx, slices.Concat(replacement.Changed(), moved))
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
