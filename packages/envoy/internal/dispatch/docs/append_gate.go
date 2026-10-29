package docs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// appendKind is what an append stores, which the gate (queueAppendGate, gateAppend) is told: a room's
// own browser update (appendBrowser), or an update the service derived and appends in a caller's
// transaction (appendDerived) - a live write's, a settlement's repair, the block-id backfill's
// stamp, a seed. The gate decides alike for both kinds: the cursor the append hands it must stand
// at the head (refuseThroughBehindHead).
type appendKind int

const (
	// appendBrowser is a room's own update, which a browser peer sent and the room applied.
	appendBrowser appendKind = iota
	// appendDerived is an update the service wrote and appends in a caller's transaction
	// (AppendUpdateTx).
	appendDerived
)

// ungatedAppend is the through an append hands the gate when no room cursor gates it
// (appendingCursor is nil): a cold write, which renders from the store and holds no room, a new
// document's seed, a test's own append. No version is negative, so the gate never takes it for a
// cursor standing at the head, and refuseThroughBehindHead passes it without comparing.
const ungatedAppend int64 = -1

// appendGateStatement is the gate's one read: the room's greatest stored version, the head an
// append's versions follow, and its least, the oldest version the room still stores, 0 each for a
// room with none.
const appendGateStatement = `
	select coalesce(max(version), 0), coalesce(min(version), 0) from doc_updates where artifact_id = $1
`

// appendGate is the gate of one append transaction: its read, queued into the caller's batch
// (queueAppendGate) or run alone (gateAppend), and, once that read has run, its decision on the
// cursor the append hands it (head).
//
// The gate: the document the appended updates were made on must stand at the head. Two
// envoy-dispatch processes serve one database, and an append of the other process's room is not
// in this one's document; appending from a room behind the head is itself harmless, since Yjs
// updates commute and the document's advisory lock serialises them, but everything the appending
// transaction renders from that document - an upload's or a conditional edit's version, a
// settlement's repairs - is rendered from text the writer never saw. The caller holds that lock in
// the same transaction, and the order is the lock, then the cursor (through, appendingThrough),
// then the head: a peer of this very room whose append committed under the lock has already
// advanced the cursor, so two peers of one room never refuse each other.
type appendGate struct {
	room string
	kind appendKind
	// read is set once the read has run and scanned; err is its failure.
	read     bool
	err      error
	latest   int64
	earliest int64
}

// queueAppendGate queues the gate's read of room into b, the caller's batch, for an append of
// kind. The caller holds the document's advisory lock in the transaction b runs in, and once b has
// run asks head with the cursor it reads then, under that lock. A batch of a room's browser
// updates queues the read into the pipeline that takes the lock
// (PgVersioned.AppendUpdatesWithCredit), so a cursor read before b ran would be read before the
// lock, and a writer that held the lock meanwhile - a settlement's repair, a live write - can have
// advanced the room's cursor since.
func queueAppendGate(b *pgx.Batch, room string, kind appendKind) *appendGate {
	gate := &appendGate{room: room, kind: kind}
	b.Queue(appendGateStatement, room).QueryRow(gate.scan)
	return gate
}

// gateAppend is queueAppendGate's read and head's decision as one statement in tx, which holds the
// document's advisory lock: the head the append's versions follow, or ErrRoomBehindHead.
func gateAppend(ctx context.Context, tx pgx.Tx, room string, through int64, kind appendKind) (int64, error) {
	gate := &appendGate{room: room, kind: kind}
	if err := gate.scan(tx.QueryRow(ctx, appendGateStatement, room)); err != nil {
		return 0, err
	}
	return gate.head(through)
}

// scan reads the gate's row.
func (g *appendGate) scan(row pgx.Row) error {
	if err := row.Scan(&g.latest, &g.earliest); err != nil {
		g.err = fmt.Errorf("read latest document update: %w", err)
		return g.err
	}
	g.read = true
	return nil
}

// head is the gate's decision once its read has run: the room's head, which the append's versions
// follow, or ErrRoomBehindHead when through, the version the append's document holds every update
// through (ungatedAppend for none), does not stand there. The caller reads through while it holds
// the document's advisory lock; under it neither the cursor nor the head the read saw can move, so
// a cursor read after the read is read in the gate's order. A read that failed, or never ran
// because a statement before it in the batch failed, is an error too.
func (g *appendGate) head(through int64) (int64, error) {
	if g.err != nil {
		return 0, g.err
	}
	if !g.read {
		return 0, fmt.Errorf("read latest document update of room %s: the read did not run", g.room)
	}
	if err := refuseThroughBehindHead(g.room, through, g.latest); err != nil {
		return 0, err
	}
	return g.latest, nil
}

// oldest is the least version the room stored when the gate read it, 0 for none: with the head
// and the versions the transaction appends, what a committed batch hands noteStoredUpdates.
func (g *appendGate) oldest() int64 {
	return g.earliest
}

// appendUpdateTxClass appends one update inside tx under the gate: the document's advisory lock,
// prune recovery, the gate (gateAppend) and the append's writes (appendWrites), each statement run
// in tx in turn. From the gate on it is a write of this process the head tracker follows until tx
// ends (headTracker.ownWriteIn).
func (p *PgVersioned) appendUpdateTxClass(ctx context.Context, tx pgx.Tx, room string, update []byte, contentChanged bool, kind appendKind) (persistence.Version, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := lockDocumentRoom(ctx, tx, room); err != nil {
		return 0, err
	}
	if err := p.recoverPruneTx(ctx, tx, room); err != nil {
		return 0, err
	}
	wrote := p.heads.ownWriteIn(tx, room)
	head, err := gateAppend(ctx, tx, room, appendingThrough(ctx, room), kind)
	if err != nil {
		return 0, err
	}
	statements, version, err := p.appendWrites(room, head, []BrowserUpdate{{Update: update, ContentChanged: contentChanged}})
	if err != nil {
		return 0, err
	}
	for _, statement := range statements {
		if err := statement.exec(ctx, tx); err != nil {
			return 0, err
		}
	}
	wrote(int64(version))
	return version, nil
}

// appendUpdatesStatement stores every update of one append transaction and announces the change,
// in one statement: the insert of one doc_updates row per update, at consecutive versions from the
// head the gate read, each with its own content class, is a data-modifying CTE, and the select it
// feeds is notifyDocumentHeadStatement, which sends both notifications, documentChangedChannel's
// bare id and documentHeadChannel's documentHeadNotice naming the last version. One update or a
// room's whole batch, the transaction spends one round trip under the document's advisory lock on
// its rows and its announcement, and sends each notification once. Why the announcement belongs
// inside this transaction is notifyDocumentChanged's doc comment, which carries a rebuild's, a
// prune's and a deletion's. The room is passed twice because its two parameters are different
// types: pg_notify takes text ($1), and doc_updates.artifact_id is a uuid ($3).
const appendUpdatesStatement = `
	with appended as (
		insert into doc_updates (artifact_id, version, update, content_changed)
		select $3::uuid, version, update, content_changed
		from unnest($4::bigint[], $5::bytea[], $6::boolean[]) as appended(version, update, content_changed)
	)
	` + notifyDocumentHeadStatement

// appendWrites is every write of an append transaction whose gate passed at head, in the order the
// transaction runs them, and the last version they store: the insert and its notifications
// (appendUpdatesStatement), the owed settlement written once (updateOwedStatement), and one
// doc_pending_authors upsert of every credited update's authors at that update's own version
// (pendingAuthorsStatement), left out when no update credits anyone. The rows are exactly those
// the same updates leave appended one at a time. A caller runs the statements in its transaction
// (writeStatement.exec) or queues them into the batch it commits with (writeStatement.queue).
//
// Each update's credit is taken here (UpdateCredit.take), so the caller holds the document's
// advisory lock from before the gate until the transaction ends. The owed row is taken with the
// write, so a route that goes on to append an event (an upload, issue creation) holds it before
// the events' commit-order lock, as an answer marking this document a copy owed does
// (owedCopiesOf); Ledger.Commit's later recordLatestEditSource then waits on nothing.
func (p *PgVersioned) appendWrites(room string, head int64, updates []BrowserUpdate) ([]writeStatement, persistence.Version, error) {
	if len(updates) == 0 {
		return nil, 0, errors.New("append document updates: no update to append")
	}
	versions := make([]int64, len(updates))
	contents := make([][]byte, len(updates))
	classes := make([]bool, len(updates))
	var owed owedUpdates
	var authors []pendingAuthor
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
	}
	last := versions[len(versions)-1]
	notice, err := p.headNotice(room, last)
	if err != nil {
		return nil, 0, err
	}
	owedStatement, err := updateOwedStatement(room, owed)
	if err != nil {
		return nil, 0, err
	}
	statements := []writeStatement{{
		sql:  appendUpdatesStatement,
		args: []any{room, notice, room, versions, contents, classes},
		what: "append document update",
	}, owedStatement}
	if len(authors) > 0 {
		upsert, err := pendingAuthorsStatement(room, authors)
		if err != nil {
			return nil, 0, err
		}
		statements = append(statements, upsert)
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
