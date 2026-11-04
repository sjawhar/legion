package docs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/persistence"
)

// browser is what a room's own append of one of its updates carries, nil for every other append
// (BrowserAppend); flush is the run of a room instance's updates the transaction is storing, when
// this append is one of them (RoomFlush); roomLocked is whether the caller already holds
// room's advisory lock, so a second acquisition inside the transaction would cost a round trip
// and grant what the caller has: appendUpdate takes the session-level lock on this connection
// before the transaction begins and keeps it past the commit (withRoomLock), and every append of
// a flush runs under a lock its caller took (AppendRoomUpdateTx, Service.flushThrough's rule);
// and owes is whether the update marks the document owing a settlement, false only for a room's
// parked bytes, which change nothing a settlement reads (AppendParkedUpdate).
func (p *PgVersioned) appendUpdateTxClass(ctx context.Context, tx pgx.Tx, room string, update []byte, contentChanged bool, credit *UpdateCredit, browser *BrowserAppend, flush *RoomFlush, roomLocked, owes bool) (persistence.Version, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !roomLocked {
		if err := lockDocumentRoom(ctx, tx, room); err != nil {
			return 0, err
		}
	}
	// The gate: the room the update was made in, read after the lock and before the head, so a
	// peer of this very room whose append committed under this lock has already advanced the
	// cursor (appendUpdate) and two peers of one room never refuse each other. A browser's delta,
	// which carries when its room took it (BrowserAppend), is admitted from a room behind an
	// intact history, since Yjs updates commute and this lock serialises them, and from one
	// behind a history a compaction folded, which still holds what the delta builds on
	// (storedHistoryHolds) - its room's cursor is marked, for the room to be closed and reloaded
	// once the append is stored (StoreUpdateContext) - unless the document's issue is closed and
	// the room took the delta later than closeMargin after the close (ErrIssueClosed); a derived
	// write - an upload's or a conditional edit's version, a settlement's repairs - only from a
	// room holding the head, since what it renders from that room's document would be rendered
	// from text the writer has not seen (appendKind). An append no cursor gates - a cold write, a
	// new document's seed, a test's own append - reads the head alone.
	//
	// One read answers for all three (readAppendGate), and carries the prune checkpoint with it:
	// a crashed prune's recovery (recoverPruneTx) runs only where a checkpoint stands, and the
	// gate is read again after it, since the recovery deletes the rows above the ceiling.
	gated := appendingCursor(ctx, room, flush)
	kind, observed := appendDerived, time.Time{}
	var replacedCursor *roomCursor
	if browser != nil {
		// A room's own append of a browser's update: its single append carries the instance's
		// cursor itself, and a flush's appends are gated on the flush, which folds in the
		// versions it has stored for that instance already (appendingCursor).
		kind, observed = appendBrowser, browser.observed
		if browser.cursor != nil {
			gated, replacedCursor = browser.cursor, browser.cursor
		} else if flush != nil {
			replacedCursor = flush.cursor
		}
	}
	var through int64
	if gated != nil {
		through = gated.heldThrough()
	}
	read, err := readAppendGate(ctx, tx, room, through)
	if err != nil {
		return 0, err
	}
	if read.pruneCeiling != nil {
		if _, err := p.recoverPruneTx(ctx, tx, room); err != nil {
			return 0, err
		}
		if read, err = readAppendGate(ctx, tx, room, through); err != nil {
			return 0, err
		}
	}
	latest := read.latest
	if gated != nil {
		var replaced bool
		// Only a browser's append is admitted on a replaced history, so a derived append reads
		// the decision alone.
		latest, replaced, err = gateAppend(ctx, tx, room, through, kind, observed, update, read, p.maxPendingItems)
		if replaced && replacedCursor != nil {
			replacedCursor.markReplaced()
			if p.storedHistoryHeld != nil {
				p.storedHistoryHeld(room)
			}
		}
	}
	if err != nil {
		return 0, err
	}
	version := persistence.Version(latest + 1)
	// Every write the append owes goes in one round trip (appendWrites): the row itself, the
	// pending settlement the update leaves owed, the pending authors it credits and the authors of
	// the ask blocks its render first showed. None of them reads another's result, and they run on
	// the server in the order they are queued, as they did when each was a statement of its own.
	//
	// The insert announces the change itself (announceDocumentHead), as a data-modifying CTE
	// rather than a statement of its own. Why the announcement belongs inside this transaction is
	// announceDocumentChange's doc comment. A transaction a caller joined (Service.Join) can append
	// several times, so it announces once, at its commit, with the last version it appended
	// (Ledger.announceAppends): this append only tells its ledger. Only Join puts a ledger with a
	// transaction in a context, and every joined append runs in that transaction (txFromContext),
	// so the ledger's transaction is tx; the two are not compared, since a pgx.Tx's dynamic type
	// need not be comparable. The room is passed twice because its two parameters are different
	// types: doc_updates.artifact_id is a uuid, and pg_notify takes text.
	//
	// The same statement names the document in doc_compaction_owed, the documents the compaction
	// sweep reads (compactionCandidates), once the row it appends is not the document's first: a
	// first row has nothing to fold into, and a document already named stays as it is.
	var writes appendWrites
	if ledger := ledgerFrom(ctx); ledger != nil && ledger.tx != nil {
		writes.queue("append document update", `
			with appended as (
				insert into doc_updates (artifact_id, version, update, content_changed) values ($1, $2, $3, $4)
			)
			insert into doc_compaction_owed (artifact_id) select $1 where $5 on conflict do nothing
		`, room, int64(version), update, contentChanged, latest > 0)
		ledger.deferAnnouncement(room, version)
	} else {
		writes.queue("append document update", `
			with appended as (
				insert into doc_updates (artifact_id, version, update, content_changed) values ($1, $2, $3, $4)
			), owed as (
				insert into doc_compaction_owed (artifact_id) select $1 where $7 on conflict do nothing
			)
			select `+announceDocumentHead("$5::text", "$2::bigint", "$6::text"),
			room, int64(version), update, contentChanged, room, p.processID, latest > 0)
	}
	if !owes {
		if err := writes.send(ctx, tx); err != nil {
			return 0, err
		}
		return version, nil
	}
	authors, lastActor, include := credit.take()
	// An edit credited to authors names its latest edit source, none included when no one
	// peer made it; an update no one is credited with keeps the row's. The row is taken here, with
	// the write, so a route that goes on to append an event (an upload, issue creation) holds it
	// before its event, as an answer marking this document a copy owed does (owedCopiesOf), and
	// Ledger.Commit's later recordLatestEditSource waits on nothing.
	credited := include && (len(authors) > 0 || lastActor != nil)
	owedArgs, err := updateOwedArgs(room, credited, lastActor, anyPerson(authors))
	if err != nil {
		return 0, err
	}
	writes.queue("record the document's pending settlement", settlementPendingStatement, owedArgs...)
	if include {
		pendingArgs, err := pendingAuthorsArgs(room, pendingAuthorsAt(authors, version))
		if err != nil {
			return 0, err
		}
		if pendingArgs != nil {
			writes.queue("record the document's pending authors", pendingAuthorsStatement, pendingArgs...)
		}
	}
	// The authors of the ask blocks this update's render first showed are recorded with it, so a
	// settlement on any process finds them (doc_ask_block_authors), consumed credit or not: a version
	// lists authors, not ask blocks.
	askArgs, err := askAuthorsArgs(room, credit.takeAskAuthors())
	if err != nil {
		return 0, err
	}
	if askArgs != nil {
		writes.queue("record the document's ask block authors", askAuthorsStatement, askArgs...)
	}
	if err := writes.send(ctx, tx); err != nil {
		return 0, err
	}
	return version, nil
}

// appendWrites gathers the writes one append owes, each with what to say when it fails, and sends
// them in one round trip (appendUpdateTxClass). They go under the document's advisory lock the
// append holds, in the order they were queued, and no later one reads an earlier one's result, so
// the pipeline writes exactly what the same statements wrote one at a time. A round trip is what
// a browser update costs most of under that lock, and every keystroke is its own append
// (PersistCoalesceWindow = -1), so the count is what the rooms on the other processes see the
// typing arrive at (room_sync.go).
type appendWrites struct {
	batch pgx.Batch
	what  []string
}

func (w *appendWrites) queue(what, sql string, args ...any) {
	w.batch.Queue(sql, args...)
	w.what = append(w.what, what)
}

// send runs every queued write, reading the results in the order they were queued so a failure is
// reported as its own statement's.
func (w *appendWrites) send(ctx context.Context, tx pgx.Tx) error {
	results := tx.SendBatch(ctx, &w.batch)
	for _, what := range w.what {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return fmt.Errorf("%s: %w", what, err)
		}
	}
	if err := results.Close(); err != nil {
		return fmt.Errorf("send the append's writes: %w", err)
	}
	return nil
}
