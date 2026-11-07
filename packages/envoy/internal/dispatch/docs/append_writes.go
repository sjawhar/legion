package docs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// appendUpdateTxClass appends one update inside tx under the gate: a run of one
// (appendUpdatesTx), with no credit and no flush gating it. The version returned is the one it
// stored; a run of one is stored whole or refused.
func (p *PgVersioned) appendUpdateTxClass(ctx context.Context, tx pgx.Tx, room string, update []byte, contentChanged bool, kind appendKind) (persistence.Version, error) {
	version, _, err := p.appendUpdatesTx(ctx, tx, room, []BrowserUpdate{{Update: update, ContentChanged: contentChanged}}, kind, nil, false)
	return version, err
}

// AppendRoomUpdatesTx appends inside tx the run of updates a room of this process has already
// applied, in the room's commit order, with the class and the in-flight credit its update observer
// recorded for each, as one append of flush, the run the appending transaction stores of that room
// instance (flushThrough). The room decoded each when it applied it, so none is decoded again.
// flush is what gates the append: the instance's cursor with the versions this flush has appended
// for it folded in, since a transaction that flushes a room it holds no write of (a settlement, a
// version's capture, the block-id backfill) gates its flush appends by that flush
// (appendingCursor). The room's own persistence worker reaches the gate another way, in its
// batch's first pipeline (queueAppendGate, PgVersioned.AppendRoomFlush).
//
// It takes no lock of its own: every flush runs under the room's advisory lock already, the
// session-level one on the room's own persistence worker's connection and the transaction-level
// one on every other writer's, taken before the head the flush is gated on was read
// (Service.flushThrough states the rule and names the callers). A second acquisition would cost a
// round trip and grant what the caller holds.
//
// It answers the first version the run stored and how many of the run it stored, which the caller
// judges (Service.flushThrough): the gate admits a prefix of a browser's run and no more, cutting
// it at the first update its document's closed issue no longer admits (gateAppend), and a flush
// left short of its run fails with ErrIssueClosed, so the transaction that joined it rolls back
// and stores nothing of the run. The prefix rule is the room's own batch path's (appendRoomTx),
// which stores what the room took in time and leaves the refused rest queued, unstored and
// unmarked, for its own call to meet the gate again, which refuses it and closes the room for its
// issue.
func (p *PgVersioned) AppendRoomUpdatesTx(ctx context.Context, tx pgx.Tx, room string, flush *RoomFlush, updates []BrowserUpdate) (persistence.Version, int, error) {
	return p.appendUpdatesTx(ctx, tx, room, updates, appendBrowser, flush, true)
}

// appendUpdatesTx appends updates inside tx under the gate, at consecutive versions from the head
// it reads: the document's advisory lock where the caller holds none (roomLocked), the gate's read
// (readAppendGate), a crashed prune's recovery where one stands, the gate's verdict (gateAppend)
// and the append's writes (appendWrites), each statement run in tx in turn. It answers the first
// version the run stored and how many of the run it stored, which the gate's prefix rule can
// leave short of the run it was handed.
func (p *PgVersioned) appendUpdatesTx(ctx context.Context, tx pgx.Tx, room string, updates []BrowserUpdate, kind appendKind, flush *RoomFlush, roomLocked bool) (persistence.Version, int, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	if !roomLocked {
		if err := lockDocumentRoom(ctx, tx, room); err != nil {
			return 0, 0, err
		}
	}
	// One read answers the gate's three questions and carries the prune checkpoint with it: a
	// crashed prune's recovery (recoverPruneTx) runs only where a checkpoint stands, and the gate
	// is read again after it, since the recovery deletes the rows above the ceiling.
	read, err := readAppendGate(ctx, tx, room)
	if err != nil {
		return 0, 0, err
	}
	if read.pruneCeiling != nil {
		if _, err := p.recoverPruneTx(ctx, tx, room); err != nil {
			return 0, 0, err
		}
		if read, err = readAppendGate(ctx, tx, room); err != nil {
			return 0, 0, err
		}
	}
	through, gated := appendingThrough(ctx, room, flush)
	head, admitted := read.latest, len(updates)
	if gated {
		var replaced bool
		head, admitted, replaced, err = gateAppend(ctx, tx, room, through, kind, updates, read, p.maxPendingItems)
		if err != nil {
			return 0, 0, err
		}
		if replaced {
			p.markRunReplaced(room, roomCursorFrom(ctx), flush)
		}
	}
	// A closed issue admits only the updates the room took in time, a prefix of the run
	// (gateAppend): the rest are not stored here and are left queued, as the room's own batch
	// path leaves them (appendRoomTx). A flush left short this way is failed by its caller, which
	// rolls this prefix back with it (Service.flushThrough).
	updates = updates[:admitted]
	statements, _, err := p.appendWrites(ctx, room, head, updates, true)
	if err != nil {
		return 0, 0, err
	}
	for _, statement := range statements {
		if err := statement.exec(ctx, tx); err != nil {
			return 0, 0, err
		}
	}
	return persistence.Version(head + 1), len(updates), nil
}

// markRunReplaced marks the room instance an admitted run was made in as standing on a replaced
// history, once for the run: its caller closes and reloads the room after the append
// (roomCursor.takeReplaced, Service.closeRoomOnReplacedHistory). The instance is the one the
// context carries, or the flush's when the run is a flush's.
func (p *PgVersioned) markRunReplaced(room string, cursor *roomCursor, flush *RoomFlush) {
	if cursor == nil && flush != nil {
		cursor = flush.cursor
	}
	if cursor != nil {
		cursor.markReplaced()
	}
	if p.storedHistoryHeld != nil {
		p.storedHistoryHeld(room)
	}
}

// appendUpdatesStatement stores every update of one append transaction and announces the change,
// in one statement: the insert of one doc_updates row per update, at consecutive versions from the
// head the gate read, each with its own content class, is a data-modifying CTE, and so is the
// document's name in doc_compaction_owed, the documents the compaction sweep reads
// (compactionCandidates); the select they feed is announceDocumentHead, which sends both
// notifications, documentHeadChannel's {r,v,p} naming the run's last version and
// documentChangedChannel's bare id. One update or a room's whole batch, the transaction spends one
// round trip under the document's advisory lock on its rows and its announcement, and sends each
// notification once. Why the announcement belongs inside this transaction is
// announceDocumentChange's doc comment, which carries a rebuild's, a prune's and a deletion's. The
// room is passed twice because its two parameters are different types: pg_notify takes text ($1),
// and doc_updates.artifact_id is a uuid ($2).
var appendUpdatesStatement = `
	with appended as (
		insert into doc_updates (artifact_id, version, update, content_changed)
		select $2::uuid, version, update, content_changed
		from unnest($4::bigint[], $5::bytea[], $6::boolean[]) as appended(version, update, content_changed)
	), owed as (
		insert into doc_compaction_owed (artifact_id) select $2::uuid where $7 on conflict do nothing
	)
	select ` + announceDocumentHead("$1::text", "$3::bigint", "$8::text")

// appendUpdatesJoinedStatement is appendUpdatesStatement inside a transaction a caller joined
// (Service.Join), which announces once at its commit with the last version it appended
// (Ledger.announceAppends) rather than once per append. It takes the document's id once, as the
// uuid its rows carry: with no announcement it sends no notification, so it needs neither the
// text spelling of the id nor the version and the process the notification names, and a
// parameter it did not read would be one Postgres could find no type for.
const appendUpdatesJoinedStatement = `
	with appended as (
		insert into doc_updates (artifact_id, version, update, content_changed)
		select $1::uuid, version, update, content_changed
		from unnest($2::bigint[], $3::bytea[], $4::boolean[]) as appended(version, update, content_changed)
	)
	insert into doc_compaction_owed (artifact_id) select $1::uuid where $5 on conflict do nothing`

// appendWrites is every write of an append transaction whose gate passed at head, in the order the
// transaction runs them, and the last version they store: the insert with its announcement and the
// document's compaction entry (appendUpdatesStatement), the owed settlement written once
// (updateOwedStatement), one doc_pending_authors upsert of every credited update's authors at that
// update's own version (pendingAuthorsStatement), left out when no update credits anyone, and the
// authors of the ask blocks the run's renders first showed (askAuthorsStatement), recorded with
// the updates so a settlement on any process finds them, consumed credit or not: a version lists
// authors, not ask blocks. The rows are exactly those the same updates leave appended one at a
// time. A caller runs the statements in its transaction (writeStatement.exec) or queues them into
// the batch it commits with (writeStatement.queue).
//
// owes is whether the document owes a settlement from these updates. It is false for a room's
// parked bytes alone (AppendParkedUpdate), which change nothing a settlement reads and credit
// nobody: they write no owed row and no pending authors, and the commit that later integrates
// them marks the document owed itself.
//
// Each update's credit is taken here (UpdateCredit.take), so the caller holds the document's
// advisory lock from before the gate until the transaction ends. The owed row is taken with the
// write, so a route that goes on to append an event (an upload, issue creation) holds it before
// its event, as an answer marking this document a copy owed does (owedCopiesOf);
// Ledger.Commit's later recordLatestEditSource then waits on nothing.
func (p *PgVersioned) appendWrites(ctx context.Context, room string, head int64, updates []BrowserUpdate, owes bool) ([]writeStatement, persistence.Version, error) {
	if len(updates) == 0 {
		return nil, 0, errors.New("append document updates: no update to append")
	}
	versions := make([]int64, len(updates))
	contents := make([][]byte, len(updates))
	classes := make([]bool, len(updates))
	var owed owedUpdates
	var authors []pendingAuthor
	var askAuthors map[string]model.Actor
	for index, appended := range updates {
		version := head + int64(index) + 1
		versions[index], contents[index], classes[index] = version, appended.Update, appended.ContentChanged
		updateAuthors, updateActor, include := appended.Credit.take()
		// An edit credited to authors names its latest edit source, none included when no one
		// peer made it, and does to the latest person sender what latestPersonOf says; an update
		// no one is credited with keeps the row's.
		if include && (len(updateAuthors) > 0 || updateActor != nil) {
			owed.credit(updateActor, anyPerson(updateAuthors))
		}
		if include {
			authors = append(authors, pendingAuthorsAt(updateAuthors, persistence.Version(version))...)
		}
		// The ask block authors of the whole run go in one statement, earliest update first:
		// the statement writes each block once (on conflict do nothing), so the earliest update
		// that showed a block names its author, as it would have appending alone.
		for block, actor := range appended.Credit.takeAskAuthors() {
			if askAuthors == nil {
				askAuthors = make(map[string]model.Actor)
			}
			if _, named := askAuthors[block]; !named {
				askAuthors[block] = actor
			}
		}
	}
	last := versions[len(versions)-1]
	// The document is named in doc_compaction_owed once a row the run appends is not the
	// document's first: a first row has nothing to fold into, and a document already named stays
	// as it is.
	folds := head > 0 || len(updates) > 1
	insert := writeStatement{
		sql:  appendUpdatesStatement,
		args: []any{room, room, last, versions, contents, classes, folds, p.processID},
		what: "append document update",
	}
	// Inside a transaction a caller joined (Service.Join) the insert carries no announcement: the
	// ledger announces once at its commit, with the last version the transaction appended. Its
	// statement reads the id, the rows and that one flag, and nothing else.
	if ledger := ledgerFrom(ctx); ledger != nil && ledger.tx != nil {
		insert.sql = appendUpdatesJoinedStatement
		insert.args = []any{room, versions, contents, classes, folds}
		ledger.deferAnnouncement(room, persistence.Version(last))
	}
	statements := []writeStatement{insert}
	if !owes {
		return statements, persistence.Version(last), nil
	}
	owedStatement, err := updateOwedStatement(room, owed)
	if err != nil {
		return nil, 0, err
	}
	statements = append(statements, owedStatement)
	if len(authors) > 0 {
		upsert, err := pendingAuthorsStatement(room, authors)
		if err != nil {
			return nil, 0, err
		}
		statements = append(statements, upsert)
	}
	if len(askAuthors) > 0 {
		askArgs, err := askAuthorsArgs(room, askAuthors)
		if err != nil {
			return nil, 0, err
		}
		if askArgs != nil {
			statements = append(statements, writeStatement{
				sql:  askAuthorsStatement,
				args: askArgs,
				what: "record the document's ask block authors",
			})
		}
	}
	return statements, persistence.Version(last), nil
}

// writeStatement is one statement of a durable write, with the words its error is wrapped in. A
// caller runs it in its own transaction (exec), or queues it into a batch it sends itself (queue),
// so one SQL serves a write made either way.
type writeStatement struct {
	sql  string
	args []any
	what string
}

// exec runs the statement in tx.
func (s writeStatement) exec(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
		return fmt.Errorf("%s: %w", s.what, err)
	}
	return nil
}

// queue queues the statement into b (store.QueueExec); its error, wrapped as exec wraps it, is
// what b's Close returns when the statement is the first of b's to fail.
func (s writeStatement) queue(b *pgx.Batch) {
	store.QueueExec(b, s.what, s.sql, s.args...)
}
