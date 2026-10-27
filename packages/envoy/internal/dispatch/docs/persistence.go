package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// misreadWarning is the shared prefix Compact and stateThrough log when the stored updates
// through a version do not fold into a state that reads back as them.
const misreadWarning = "dispatch: a document's stored updates do not fold into a state that reads back as them"

// PgVersioned persists a room's Yjs V1 updates in Dispatch's Postgres store.
type PgVersioned struct {
	store *store.Store
	// locksMu guards locks: each room's in-process lock (lockRoom), kept while a caller holds it
	// or waits for it and dropped with the last, so it holds only the rooms being written.
	locksMu sync.Mutex
	locks   map[string]*roomLock
	// afterAppendCommit runs between an append's commit and the advance of the room cursor that
	// takes that version in, inside the document's advisory lock the append holds across both.
	// Nil outside tests; tests use it to hold a reader of the cursor and the head in that window
	// (TestAWriteDoesNotForkFromTheStoreInsideAnAppendsCursorWindow).
	afterAppendCommit func(room string, version persistence.Version)
	// processID is the id of the process whose Service was built on this store (Service.ProcessID),
	// which every append's documentHeadChannel notification names, so a listener can tell this
	// process's own writes from another's. New sets it (setProcessID) before the Service it builds
	// runs anything; a store no Service was built on - a test's, a seed's - leaves it empty.
	processID string
}

// roomLock is one room's in-process lock and how many callers hold it or wait for it.
type roomLock struct {
	sync.Mutex
	users int
}

// NewPgVersioned creates the versioned store for a Dispatch database.
func NewPgVersioned(database *store.Store) *PgVersioned {
	return &PgVersioned{store: database}
}

// setProcessID gives the store the id of the process whose Service was built on it
// (Service.ProcessID). It is part of VersionedStore so that New reaches the PgVersioned under any
// store that wraps one by embedding it.
func (p *PgVersioned) setProcessID(id string) {
	p.processID = id
}

// Load returns the room's state at its head (stateThrough), constrained by a pending prune
// checkpoint when one exists.
func (p *PgVersioned) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	if err := ctx.Err(); err != nil {
		return persistence.LoadResult{}, err
	}
	rooms, err := p.store.Pool.Rooms()
	if err != nil {
		return persistence.LoadResult{}, err
	}
	tx, err := rooms.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return persistence.LoadResult{}, fmt.Errorf("begin document load: %w", err)
	}
	defer tx.Rollback(ctx)
	head, err := p.head(ctx, tx, room)
	if err != nil {
		return persistence.LoadResult{}, err
	}
	if head == 0 {
		return persistence.LoadResult{}, tx.Commit(ctx)
	}
	update, err := stateThrough(ctx, tx, room, head)
	if err != nil {
		return persistence.LoadResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return persistence.LoadResult{}, fmt.Errorf("commit document load: %w", err)
	}
	return persistence.LoadResult{Update: update, Version: head}, nil
}

// Head is the version Load folds the room up to now, read through the pool that owns loads, as
// Load reads it: a room load (servicePersistenceAdapter.LoadDoc) asks it.
func (p *PgVersioned) Head(ctx context.Context, room string) (persistence.Version, error) {
	rooms, err := p.store.Pool.Rooms()
	if err != nil {
		return 0, err
	}
	return p.head(ctx, rooms, room)
}

// DocumentStamp is the room's DocumentStamp now, read through the pool that owns loads, as Head
// reads the head.
func (p *PgVersioned) DocumentStamp(ctx context.Context, room string) (DocumentStamp, error) {
	rooms, err := p.store.Pool.Rooms()
	if err != nil {
		return DocumentStamp{}, err
	}
	return readDocumentStamp(ctx, rooms, room)
}

// AppendUpdate validates and stores one incremental V1 update as content.
func (p *PgVersioned) AppendUpdate(ctx context.Context, room string, update []byte) (persistence.Version, error) {
	return p.appendUpdate(ctx, room, update, true, nil)
}

// AppendUpdateWithCredit validates and stores one incremental V1 update of a room, with its
// rendered-content classification and the browser edit's in-flight credit: in the update's own
// transaction, under the document's advisory lock, it writes the credit's authors to the
// document's pending authors unless a committed version already listed them (UpdateCredit.take),
// and it lands the credit before it releases that lock (UpdateCredit.landed).
func (p *PgVersioned) AppendUpdateWithCredit(ctx context.Context, room string, update []byte, contentChanged bool, credit *UpdateCredit) (persistence.Version, error) {
	return p.appendUpdate(ctx, room, update, contentChanged, credit)
}

func (p *PgVersioned) appendUpdate(ctx context.Context, room string, update []byte, contentChanged bool, credit *UpdateCredit) (persistence.Version, error) {
	if err := crdt.ApplyUpdateV1(newDocumentCopy(), update, nil); err != nil {
		return 0, err
	}
	var version persistence.Version
	err := p.withRoomLock(ctx, room, func(conn *pgxpool.Conn) error {
		// The credit lands while withRoomLock still holds the document's lock, whether the
		// transaction committed or not: no version reads the pending authors between its
		// commit and its leaving the room's in-flight credits.
		defer credit.landed()
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin document update: %w", err)
		}
		defer tx.Rollback(ctx)
		version, err = p.appendUpdateTxClass(ctx, tx, room, update, contentChanged, credit, appendBrowser)
		if err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit document update: %w", err)
		}
		credit.held(version)
		if p.afterAppendCommit != nil {
			p.afterAppendCommit(room, version)
		}
		// The room instance's cursor advances here, inside the lock the append holds, rather than
		// where the caller marked the context with it (StoreUpdateContext): a writer that took the
		// lock after this commit released it would otherwise read a cursor one version behind the
		// head it reads under that lock, and the gate would refuse an agent's edit while nothing
		// was wrong with its room.
		advanceRoomCursor(roomCursorFrom(ctx), version)
		return nil
	})
	return version, err
}

// AppendUpdateTx appends an already validated V1 update inside tx, recording whether it changes
// the document's content: settlement versions a document only past a content update.
func (p *PgVersioned) AppendUpdateTx(ctx context.Context, tx pgx.Tx, room string, update []byte, contentChanged bool) (persistence.Version, error) {
	if err := crdt.ApplyUpdateV1(newDocumentCopy(), update, nil); err != nil {
		return 0, itemCapError(update, err)
	}
	return p.appendUpdateTxClass(ctx, tx, room, update, contentChanged, nil, appendDerived)
}

// lockDocumentRoom serializes every durable mutation of one document. Callers
// that need a check-then-write guarantee must hold it before reading live state. Every durable
// writer that takes the room lock inside a transaction takes it here; withRoomLock takes the
// session-level form on its own connection, and lockSettlementCursor try-locks it at shutdown.
// The rule between this lock and a document's owner row has two halves, and both are
// load-bearing.
//
// Level. No lock on issues, artifacts, projects or asks is `for update`. Weaker levels are
// free - `for no key update` where a writer must serialise against other writers of the same
// row, `for share` or `for key share` where it need not - and TestNoForUpdateOnOwnerTables
// enforces the prohibition over the way these locks are written: a `for update` spelled in a
// string literal, or in a chain of literals and package-level constants, `var`s included,
// resolved to a fixpoint, which is every site here. An operand the check cannot resolve is
// read through its own string literals alone, spliced in with a space at each end, so a clause
// that forms across that splice is refused too, and only a clause the scanned text never
// spells, including a keyword the splice splits mid-word, is outside its reach and is a review
// matter. What `for update` costs is the `for key share` a foreign key takes: under it an
// insert into doc_updates, doc_snapshots, doc_checkpoints, comments, or any child table added
// later waits on the owner row, and a durable writer holding this lock then deadlocks against
// whoever holds that row. `for no key update` conflicts with itself exactly as `for update`
// did, so writers of one owner still serialise and the per-owner event sequence is unchanged.
//
// One transaction would still need `for update` on these tables: one that deletes such a row or
// changes a key column, which is what the weaker level does not cover. Nothing here does either.
// The check refuses it if something starts to, and that red is the prompt to revisit this rule,
// not to reach for a weaker level that would not hold.
//
// Order. A transaction that takes both an owner row and this lock takes the owner row first.
// The durable writers - appendUpdate, CaptureSnapshot, PruneAfter, and everything else reaching
// this through withRoomLock - take no owner row at all, which is why order alone could never
// have been the whole rule: withRoomLock holds a session-level pg_advisory_lock on its own
// connection before it opens a transaction, so no row lock can precede it there. But a
// transaction that does take both, such as an upload appending its event after writing the
// document, still deadlocks against a settlement if it takes them the other way round: two
// `for no key update` locks on the same row conflict with each other.
func lockDocumentRoom(ctx context.Context, tx pgx.Tx, room string) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext($1))`, room); err != nil {
		return fmt.Errorf("lock document room: %w", err)
	}
	return nil
}

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
// (queueAppendGate) or run alone (gateAppend), and, once that read has run, its decision (head).
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
	room    string
	through int64
	kind    appendKind
	// read is set once the read has run and scanned; err is its failure.
	read     bool
	err      error
	latest   int64
	earliest int64
}

// queueAppendGate queues the gate's read of room into b, the caller's batch, for an append of kind
// whose document holds every update through through (ungatedAppend for none). The caller holds the
// document's advisory lock in the transaction b runs in, and asks head once b has run.
func queueAppendGate(b *pgx.Batch, room string, through int64, kind appendKind) *appendGate {
	gate := &appendGate{room: room, through: through, kind: kind}
	b.Queue(appendGateStatement, room).QueryRow(gate.scan)
	return gate
}

// gateAppend is queueAppendGate's read and head's decision as one statement in tx, which holds the
// document's advisory lock: the head the append's versions follow, or ErrRoomBehindHead.
func gateAppend(ctx context.Context, tx pgx.Tx, room string, through int64, kind appendKind) (int64, error) {
	gate := &appendGate{room: room, through: through, kind: kind}
	if err := gate.scan(tx.QueryRow(ctx, appendGateStatement, room)); err != nil {
		return 0, err
	}
	return gate.head()
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
// follow, or ErrRoomBehindHead when the append's document does not stand there. A read that failed,
// or never ran because a statement before it in the batch failed, is an error too.
func (g *appendGate) head() (int64, error) {
	if g.err != nil {
		return 0, g.err
	}
	if !g.read {
		return 0, fmt.Errorf("read latest document update of room %s: the read did not run", g.room)
	}
	if err := refuseThroughBehindHead(g.room, g.through, g.latest); err != nil {
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
// in tx in turn.
func (p *PgVersioned) appendUpdateTxClass(ctx context.Context, tx pgx.Tx, room string, update []byte, contentChanged bool, credit *UpdateCredit, kind appendKind) (persistence.Version, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := lockDocumentRoom(ctx, tx, room); err != nil {
		return 0, err
	}
	if err := p.recoverPruneTx(ctx, tx, room); err != nil {
		return 0, err
	}
	head, err := gateAppend(ctx, tx, room, appendingThrough(ctx, room), kind)
	if err != nil {
		return 0, err
	}
	statements, version, err := p.appendWrites(room, head, []appendedUpdate{{update: update, contentChanged: contentChanged, credit: credit}})
	if err != nil {
		return 0, err
	}
	for _, statement := range statements {
		if err := statement.exec(ctx, tx); err != nil {
			return 0, err
		}
	}
	return version, nil
}

// appendedUpdate is one update an append transaction stores: its bytes, whether it changes the
// document's content, and the browser edit's in-flight credit, nil for an update no browser edit
// credited.
type appendedUpdate struct {
	update         []byte
	contentChanged bool
	credit         *UpdateCredit
}

// appendUpdatesStatement stores every update of one append transaction and announces the change,
// in one statement: the insert of one doc_updates row per update, at consecutive versions from the
// head the gate read, each with its own content class, is a data-modifying CTE, and the select it
// feeds sends both notifications, documentChangedChannel's bare id and documentHeadChannel's
// documentHeadNotice naming the last version. One update or a room's whole batch, the transaction
// spends one round trip under the document's advisory lock on its rows and its announcement, and
// sends each notification once. Why the announcement belongs inside this transaction is
// notifyDocumentChanged's doc comment, which still carries a rebuild's. The room is passed twice
// because its two parameters are different types: doc_updates.artifact_id is a uuid, and
// pg_notify takes text.
const appendUpdatesStatement = `
	with appended as (
		insert into doc_updates (artifact_id, version, update, content_changed)
		select $1::uuid, version, update, content_changed
		from unnest($2::bigint[], $3::bytea[], $4::boolean[]) as appended(version, update, content_changed)
	)
	select pg_notify('` + documentChangedChannel + `', $5), pg_notify('` + documentHeadChannel + `', $6)
`

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
func (p *PgVersioned) appendWrites(room string, head int64, updates []appendedUpdate) ([]writeStatement, persistence.Version, error) {
	if len(updates) == 0 {
		return nil, 0, errors.New("append document updates: no update to append")
	}
	versions := make([]int64, len(updates))
	contents := make([][]byte, len(updates))
	classes := make([]bool, len(updates))
	credited := false
	var lastActor *model.Actor
	var authors []pendingAuthor
	for index, appended := range updates {
		version := head + int64(index) + 1
		versions[index], contents[index], classes[index] = version, appended.update, appended.contentChanged
		updateAuthors, updateActor, include := appended.credit.take()
		// An edit credited to authors names its latest edit source, none included when no one
		// peer made it; an update no one is credited with keeps the row's.
		if include && (len(updateAuthors) > 0 || updateActor != nil) {
			credited, lastActor = true, updateActor
		}
		if include {
			authors = append(authors, pendingAuthorsAt(updateAuthors, persistence.Version(version))...)
		}
	}
	last := versions[len(versions)-1]
	notice, err := json.Marshal(documentHeadNotice{Room: room, Head: last, Process: p.processID})
	if err != nil {
		return nil, 0, fmt.Errorf("encode the document's head notification: %w", err)
	}
	owed, err := updateOwedStatement(room, credited, lastActor)
	if err != nil {
		return nil, 0, err
	}
	statements := []writeStatement{{
		sql:  appendUpdatesStatement,
		args: []any{room, versions, contents, classes, room, string(notice)},
		what: "append document update",
	}, owed}
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

// queue queues the statement into b; its error, wrapped as exec wraps it, is what b's Close
// returns when the statement is the first of b's to fail.
func (s writeStatement) queue(b *pgx.Batch) {
	what := s.what
	b.Queue(s.sql, s.args...).Fn = func(results pgx.BatchResults) error {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	}
}

// clearSettlementPending deletes the document's pending settlement inside the settlement
// transaction that has read every update, so it commits only with that settlement's writes. The
// settlement holds the document's advisory lock from before it reads the update cursor until it
// commits, so no update it has not read can be appended meanwhile.
func clearSettlementPending(ctx context.Context, tx pgx.Tx, room string) error {
	if _, err := tx.Exec(ctx, `delete from doc_settlements_pending where artifact_id = $1`, room); err != nil {
		return fmt.Errorf("clear the document's pending settlement: %w", err)
	}
	return nil
}

// settlementPending reports whether room owes a settlement. Shutdown's drain uses this to skip
// settling a document that already has none owed, rather than spending its budget repeating work
// a settlement already finished.
func settlementPending(ctx context.Context, q Queryer, room string) (bool, error) {
	var pending bool
	if err := q.QueryRow(ctx, `
		select exists(select 1 from doc_settlements_pending where artifact_id = $1)
	`, room).Scan(&pending); err != nil {
		return false, fmt.Errorf("read the document's pending settlement: %w", err)
	}
	return pending, nil
}

// ListVersions returns persisted incremental update metadata newest-first.
func (p *PgVersioned) ListVersions(ctx context.Context, room string) ([]persistence.VersionMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := p.pool().Query(ctx, `
		select version, created_at
		from doc_updates
		where artifact_id = $1
		  and version <= `+headVersion+`
		order by version desc
	`, room)
	if err != nil {
		return nil, fmt.Errorf("list document updates: %w", err)
	}
	defer rows.Close()
	versions := []persistence.VersionMeta{}
	for rows.Next() {
		var version int64
		var updatedAt time.Time
		if err := rows.Scan(&version, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan document update: %w", err)
		}
		versions = append(versions, persistence.VersionMeta{Version: persistence.Version(version), UpdatedAt: updatedAt})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list document updates: %w", err)
	}
	return versions, nil
}

// GetUpdate returns one stored incremental V1 update.
func (p *PgVersioned) GetUpdate(ctx context.Context, room string, version persistence.Version) ([]byte, persistence.VersionMeta, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, persistence.VersionMeta{}, false, err
	}
	var update []byte
	var updatedAt time.Time
	err := p.pool().QueryRow(ctx, `
		select update, created_at
		from doc_updates
		where artifact_id = $1
		  and version = $2
		  and version <= `+headVersion+`
	`, room, int64(version)).Scan(&update, &updatedAt)
	if err == pgx.ErrNoRows {
		return nil, persistence.VersionMeta{}, false, nil
	}
	if err != nil {
		return nil, persistence.VersionMeta{}, false, fmt.Errorf("get document update: %w", err)
	}
	return update, persistence.VersionMeta{Version: version, UpdatedAt: updatedAt}, true, nil
}

// MaterializeAt rebuilds the V1 state at version (stateThrough), or the latest state before it
// when a requested version is newer than the room head.
func (p *PgVersioned) MaterializeAt(ctx context.Context, room string, version persistence.Version) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if version == 0 {
		return nil, nil
	}
	// A version read opens its own transaction, so it marks its context like every other
	// opener: the pool refuses a second connection taken under it (store.ErrNestedAcquire). It reads
	// one snapshot, as Load does, since stateThrough can read the room's updates twice.
	ctx = store.WithTransactionTracking(ctx)
	tx, err := p.pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin materialize document: %w", err)
	}
	defer tx.Rollback(ctx)
	head, err := p.head(ctx, tx, room)
	if err != nil {
		return nil, err
	}
	if head == 0 {
		return nil, persistence.ErrRoomNotFound
	}
	if version > head {
		version = head
	}
	update, err := stateThrough(ctx, tx, room, version)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit materialize document: %w", err)
	}
	return update, nil
}

// CaptureSnapshot stores a named V1 state blob at the room's current head.
func (p *PgVersioned) CaptureSnapshot(ctx context.Context, room, name string, state []byte) (persistence.Version, error) {
	var version persistence.Version
	err := p.withRoomLock(ctx, room, func(conn *pgxpool.Conn) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin capture document snapshot: %w", err)
		}
		defer tx.Rollback(ctx)
		if err := lockDocumentRoom(ctx, tx, room); err != nil {
			return err
		}
		if err := p.recoverPruneTx(ctx, tx, room); err != nil {
			return err
		}
		version, err = p.head(ctx, tx, room)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			insert into doc_snapshots (artifact_id, name, version, state)
			values ($1, $2, $3, $4)
			on conflict (artifact_id, name)
			do update set version = excluded.version, state = excluded.state, created_at = now()
		`, room, name, int64(version), state); err != nil {
			return fmt.Errorf("capture document snapshot: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit document snapshot: %w", err)
		}
		return nil
	})
	return version, err
}

// RestoreSnapshot returns a named V1 state blob when it exists.
func (p *PgVersioned) RestoreSnapshot(ctx context.Context, room, name string) ([]byte, persistence.Version, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, false, err
	}
	var state []byte
	var version int64
	err := p.pool().QueryRow(ctx, `
		select state, version from doc_snapshots where artifact_id = $1 and name = $2
	`, room, name).Scan(&state, &version)
	if err == pgx.ErrNoRows {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("restore document snapshot: %w", err)
	}
	return state, persistence.Version(version), true, nil
}

// PruneAfter makes target the visible head. Its checkpoint is committed before
// deleting newer updates so a crash cannot make them visible again.
func (p *PgVersioned) PruneAfter(ctx context.Context, room string, target persistence.Version, rolledBack []byte) error {
	return p.withRoomLock(ctx, room, func(conn *pgxpool.Conn) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin checkpoint document prune: %w", err)
		}
		defer tx.Rollback(ctx)
		if err := lockDocumentRoom(ctx, tx, room); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `select exists(select 1 from doc_updates where artifact_id = $1)`, room).Scan(&exists); err != nil {
			return fmt.Errorf("check document room: %w", err)
		}
		if !exists && target != 0 {
			return persistence.ErrRoomNotFound
		}
		if _, err := tx.Exec(ctx, `
			insert into doc_checkpoints (artifact_id, ceiling, state) values ($1, $2, $3)
			on conflict (artifact_id) do update set ceiling = excluded.ceiling, state = excluded.state
		`, room, int64(target), rolledBack); err != nil {
			return fmt.Errorf("checkpoint document prune: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit checkpoint document prune: %w", err)
		}

		tx, err = conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin finish document prune: %w", err)
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `delete from doc_updates where artifact_id = $1 and version > $2`, room, int64(target)); err != nil {
			return fmt.Errorf("delete pruned document updates: %w", err)
		}
		if _, err := tx.Exec(ctx, `delete from doc_checkpoints where artifact_id = $1`, room); err != nil {
			return fmt.Errorf("clear document prune checkpoint: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit document prune: %w", err)
		}
		return nil
	})
}

// Compact folds a room's oldest updates into the oldest record it keeps, which then holds their
// state (foldThrough): what the document still holds of them, and none of the content they
// inserted that a later update deleted. The room's state is unchanged, and so is every retained
// version's. Updates that do not fold into a state that reads back as them are left as stored.
// Under a context compactIfIdle marked, it leaves a room whose lock another holder has for a later
// compaction.
func (p *PgVersioned) Compact(ctx context.Context, room string, keep int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if keep <= 0 {
		return 0, nil
	}
	deleted := 0
	_, idleOnly := ctx.Value(compactIfIdleKey{}).(bool)
	err := p.lockRoom(ctx, room, !idleOnly, func(conn *pgxpool.Conn) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin compact document: %w", err)
		}
		defer tx.Rollback(ctx)
		if err := lockDocumentRoom(ctx, tx, room); err != nil {
			return err
		}
		if err := p.recoverPruneTx(ctx, tx, room); err != nil {
			return err
		}
		var coveredCursor int64
		if err := tx.QueryRow(ctx, `
			select coalesce((
				select doc_update_version from artifact_versions where artifact_id = $1 order by number desc limit 1
			), 0)
		`, room).Scan(&coveredCursor); err != nil {
			return fmt.Errorf("read compactable document version cursor: %w", err)
		}
		// The list holds versions alone: foldThrough reads the updates it folds one at a time.
		rows, err := tx.Query(ctx, `
			select version, content_changed from doc_updates where artifact_id = $1 order by version asc
		`, room)
		if err != nil {
			return fmt.Errorf("list compactable document updates: %w", err)
		}
		var versions []int64
		var contentClasses []bool
		for rows.Next() {
			var version int64
			var contentChanged bool
			if err := rows.Scan(&version, &contentChanged); err != nil {
				rows.Close()
				return fmt.Errorf("scan compactable document update: %w", err)
			}
			versions = append(versions, version)
			contentClasses = append(contentClasses, contentChanged)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("list compactable document updates: %w", err)
		}
		rows.Close()
		if len(versions) <= keep {
			return tx.Commit(ctx)
		}
		through := persistence.Version(versions[len(versions)-keep])
		state, misread, err := foldThrough(ctx, tx, room, through)
		if err != nil {
			return fmt.Errorf("fold compacted document updates: %w", err)
		}
		if misread != nil {
			slog.Warn(misreadWarning+"; compaction leaves them as stored",
				"room", room, "version", int64(through), "error", misread)
			return tx.Commit(ctx)
		}
		deleted = len(versions) - keep
		contentChanged := false
		for index, class := range contentClasses[:deleted+1] {
			contentChanged = contentChanged || (versions[index] > coveredCursor && class)
		}
		if _, err := tx.Exec(ctx, `
			update doc_updates set update = $3, content_changed = $4 where artifact_id = $1 and version = $2
		`, room, versions[deleted], state, contentChanged); err != nil {
			return fmt.Errorf("fold compacted document updates: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			delete from doc_updates where artifact_id = $1 and version < $2
		`, room, versions[deleted]); err != nil {
			return fmt.Errorf("delete compacted document updates: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit compact document: %w", err)
		}
		return nil
	})
	if errors.Is(err, errRoomLockBusy) {
		return 0, nil
	}
	return deleted, err
}

// RebuildTx replaces a room's durable history with one fresh update at the next version, inside
// tx, which holds the room's lock until it ends. Its caller has already proved that ygo cannot load
// the old merged history; this method never makes that destructive decision itself.
func (p *PgVersioned) RebuildTx(ctx context.Context, tx pgx.Tx, room string, seed []byte) (RebuildReport, error) {
	if err := validateUpdate(seed); err != nil {
		return RebuildReport{}, wrapUnlessTooLarge(itemCapError(seed, err), "validate rebuilt document seed")
	}
	if err := lockDocumentRoom(ctx, tx, room); err != nil {
		return RebuildReport{}, err
	}
	if err := p.recoverPruneTx(ctx, tx, room); err != nil {
		return RebuildReport{}, err
	}
	head, err := p.head(ctx, tx, room)
	if err != nil {
		return RebuildReport{}, err
	}
	var report RebuildReport
	for _, deletion := range []struct {
		query string
		count *int64
	}{
		{`delete from doc_updates where artifact_id = $1`, &report.RemovedUpdates},
		{`delete from doc_checkpoints where artifact_id = $1`, &report.RemovedCheckpoints},
		{`delete from doc_snapshots where artifact_id = $1`, &report.RemovedSnapshots},
	} {
		result, err := tx.Exec(ctx, deletion.query, room)
		if err != nil {
			return RebuildReport{}, fmt.Errorf("delete rebuilt document data: %w", err)
		}
		*deletion.count = result.RowsAffected()
	}
	report.Head = int64(head) + 1
	if _, err := tx.Exec(ctx, `
		insert into doc_updates (artifact_id, version, update, content_changed)
		values ($1, $2, $3, true)
	`, room, report.Head, seed); err != nil {
		return RebuildReport{}, fmt.Errorf("seed rebuilt document: %w", err)
	}
	if err := notifyDocumentChanged(ctx, tx, room); err != nil {
		return RebuildReport{}, err
	}
	return report, nil
}

// Delete removes all persisted Yjs data for a document room.
func (p *PgVersioned) Delete(ctx context.Context, room string) error {
	return p.withRoomLock(ctx, room, func(conn *pgxpool.Conn) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin delete document: %w", err)
		}
		defer tx.Rollback(ctx)
		if err := lockDocumentRoom(ctx, tx, room); err != nil {
			return err
		}
		for _, query := range []string{
			`delete from doc_snapshots where artifact_id = $1`,
			`delete from doc_updates where artifact_id = $1`,
			`delete from doc_checkpoints where artifact_id = $1`,
		} {
			if _, err := tx.Exec(ctx, query, room); err != nil {
				return fmt.Errorf("delete document data: %w", err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit delete document: %w", err)
		}
		return nil
	})
}

// headVersion is the SQL expression for the version a room's stored history stands at, for the room
// $1 names: a prune's checkpoint ceiling while one stands, otherwise the greatest stored version, 0
// for none. Every query that reads the head builds on it (head, readDocumentStamp, ListVersions,
// GetUpdate), so what counts as the head cannot change for one of them and not the others.
const headVersion = `coalesce(
	(select ceiling from doc_checkpoints where artifact_id = $1),
	(select max(version) from doc_updates where artifact_id = $1),
	0
)`

func (p *PgVersioned) head(ctx context.Context, q Queryer, room string) (persistence.Version, error) {
	var version int64
	if err := q.QueryRow(ctx, `select `+headVersion, room).Scan(&version); err != nil {
		return 0, fmt.Errorf("read document head: %w", err)
	}
	return persistence.Version(version), nil
}

func (p *PgVersioned) recoverPruneTx(ctx context.Context, tx pgx.Tx, room string) error {
	var ceiling int64
	err := tx.QueryRow(ctx, `select ceiling from doc_checkpoints where artifact_id = $1 for update`, room).Scan(&ceiling)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read document prune checkpoint: %w", err)
	}
	if _, err := tx.Exec(ctx, `delete from doc_updates where artifact_id = $1 and version > $2`, room, ceiling); err != nil {
		return fmt.Errorf("recover pruned document updates: %w", err)
	}
	if _, err := tx.Exec(ctx, `delete from doc_checkpoints where artifact_id = $1`, room); err != nil {
		return fmt.Errorf("clear recovered document checkpoint: %w", err)
	}
	return nil
}

// compactIfIdleKey marks a context whose compaction skips a room whose lock another holder has
// (compactIfIdle).
type compactIfIdleKey struct{}

// compactIfIdle marks ctx so Compact leaves a room whose lock another holder has for a later
// compaction rather than waiting for the lock. The room's persistence worker compacts under it
// (servicePersistenceAdapter.Compact) as it exits. roomServer prevents that exit from racing a
// repair, but intentionally does not gate a published live write: it is already durable, and its
// room can still retire under publishLiveUpdate's Apply. Compaction is housekeeping, so that
// worker leaves a busy room for the next compaction rather than waiting behind a document-lock
// holder. A failed room's eviction is the exception: it compacts under the lock before recovery.
func compactIfIdle(ctx context.Context) context.Context {
	return context.WithValue(ctx, compactIfIdleKey{}, true)
}

// errRoomLockBusy is a room lock another holder has, for work that does not wait for it.
var errRoomLockBusy = errors.New("document room lock is held")

func (p *PgVersioned) withRoomLock(ctx context.Context, room string, fn func(*pgxpool.Conn) error) error {
	return p.lockRoom(ctx, room, true, fn)
}

// lockRoom runs fn holding a pooled connection and the room's advisory lock. When wait is false
// it takes neither lock another holder has, and returns errRoomLockBusy instead.
func (p *PgVersioned) lockRoom(ctx context.Context, room string, wait bool, fn func(*pgxpool.Conn) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lock := p.acquireRoomLock(room)
	defer p.releaseRoomLock(room, lock)
	if wait {
		lock.Lock()
	} else if !lock.TryLock() {
		return errRoomLockBusy
	}
	defer lock.Unlock()

	conn, err := p.pool().Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire document connection: %w", err)
	}
	defer conn.Release()
	// The work below holds this connection and the room's advisory lock; anything it reads
	// reads through them, never through a second pooled connection.
	ctx, releaseMark := store.HoldsConnection(ctx)
	defer releaseMark()
	if wait {
		if _, err := conn.Exec(ctx, `select pg_advisory_lock(hashtext($1))`, room); err != nil {
			return fmt.Errorf("lock document room: %w", err)
		}
	} else {
		var locked bool
		if err := conn.QueryRow(ctx, `select pg_try_advisory_lock(hashtext($1))`, room).Scan(&locked); err != nil {
			return fmt.Errorf("lock document room: %w", err)
		}
		if !locked {
			return errRoomLockBusy
		}
	}
	defer func() { _, _ = conn.Exec(context.Background(), `select pg_advisory_unlock(hashtext($1))`, room) }()
	return fn(conn)
}

// acquireRoomLock counts the caller among room's lock's users, creating the lock for the first, so
// every caller of one room shares one lock however many come and go.
func (p *PgVersioned) acquireRoomLock(room string) *roomLock {
	p.locksMu.Lock()
	defer p.locksMu.Unlock()
	if p.locks == nil {
		p.locks = make(map[string]*roomLock)
	}
	lock := p.locks[room]
	if lock == nil {
		lock = &roomLock{}
		p.locks[room] = lock
	}
	lock.users++
	return lock
}

// releaseRoomLock ends a use acquireRoomLock counted, dropping room's lock with its last user.
func (p *PgVersioned) releaseRoomLock(room string, lock *roomLock) {
	p.locksMu.Lock()
	defer p.locksMu.Unlock()
	lock.users--
	if lock.users == 0 {
		delete(p.locks, room)
	}
}

func (p *PgVersioned) pool() *store.Pool {
	return p.store.Pool
}

// compactKeep is how many stored updates a room's compaction leaves: one, the room's state. Nothing
// reads a stored update back but through stateThrough at the head - a version keeps its own
// markdown, and no route reads an update or a state before the head - so an update kept past the
// fold would hold only bytes every load applies.
const compactKeep = 1

// CompactAll folds every document's stored updates into its state.
func (s *Service) CompactAll(ctx context.Context) error {
	ctx = store.WithTransactionTracking(ctx)
	rooms, err := s.documentRooms(ctx, "document rooms", `select id::text from artifacts where kind = 'doc'`)
	if err != nil {
		return err
	}
	for _, artifactID := range rooms {
		if _, err := s.persistence.Compact(ctx, artifactID, compactKeep); err != nil {
			return fmt.Errorf("compact document %s: %w", artifactID, err)
		}
	}
	return nil
}

// documentRooms drains the id list before its caller does anything with it: the work each id
// leads to - a compaction, a room close - takes a pooled connection of its own, and holding
// the rows open across that would be a second connection for work the first is waiting on.
// The what argument names the list in the errors the caller reads.
func (s *Service) documentRooms(ctx context.Context, what, sql string, args ...any) ([]string, error) {
	rows, err := s.store.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", what, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var artifactID string
		if err := rows.Scan(&artifactID); err != nil {
			return nil, fmt.Errorf("scan document room: %w", err)
		}
		ids = append(ids, artifactID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s: %w", what, err)
	}
	return ids, nil
}

var _ persistence.VersionedPersistence = (*PgVersioned)(nil)

// maxUpdateItems is the most items one V1 document update may carry: ygo's maxV2Items, which it
// does not export. TestUploadRefusesADocumentTooLargeToStore (api) holds the two together: a
// document encoding to more than this is refused by ygo and answered as CAP_EXCEEDED with the count.
// No single upload reaches it: the heaviest document pmdoc's element limit admits encodes to about
// 131,000 items. Repeated versions can take a live state to it, so every joined write (applyLive)
// decodes its state before appending once its clocks pass this cap (refuseUndecodable, growth.go)
// - not settlement's own repairs, which write outside applyLive and never reach it (growth.go's
// own doc comment names the three).
//
// It is also the pending queue every decode of document bytes takes, whether the service builds
// the decoder (newDocumentCopy) or ygo builds it for the service (Server.MaxPendingItems: the
// rooms). ygo's decoder parks each item whose parent it cannot place yet, and each later item of
// that client behind it, and refuses the update once its queue is full, 100,000 items at ygo's
// default. An update decoded alone parks every item that leans on the document it was written
// against, and no update carries more items than this, so no decode parks past it. ygo's own
// check of each update the service broadcasts (Server.BroadcastUpdate) takes no pending queue at
// all: it is the check ygo's bundled stores run, since the room has already applied the update
// (reearth/ygo#268, in the pinned fork).
// TestTheStoreTakesOneBrowserUpdateItsRoomTook and TestASettlementStampsMoreBlocksThanYgosDefaultQueue
// are updates the store's check refuses at ygo's default queue.
const maxUpdateItems = 1 << 20

// updateItems is the number of items a V1 update of one client declares in its header: the
// client count, then that client's item count. A server-authored update - a seed, or a replace on
// one fork - has exactly one client; an update of several would need the whole update decoded to
// count, and answers false.
func updateItems(update []byte) (uint64, bool) {
	decoder := encoding.NewDecoder(update)
	clients, err := decoder.ReadVarUint()
	if err != nil || clients != 1 {
		return 0, false
	}
	items, err := decoder.ReadVarUint()
	return items, err == nil
}

// itemCapError is ygo's refusal err of update, an update the server encoded itself, named as the
// write too large to store it is when update's header declares more than maxUpdateItems items:
// ygo refuses such an update with the same error it gives a malformed one, and a seed or a
// replace over the cap is the caller's document to shrink, answered 413 CAP_EXCEEDED (LEGION-465).
// Any other refusal of a server-encoded update is a fault, and is err unchanged.
func itemCapError(update []byte, err error) error {
	if items, ok := updateItems(update); ok && items > maxUpdateItems {
		return fmt.Errorf("%w: its %d items exceed the %d one document update can hold (%v)", ErrDocumentTooLarge, items, maxUpdateItems, err)
	}
	return err
}
