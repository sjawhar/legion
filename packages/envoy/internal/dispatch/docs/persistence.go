package docs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// misreadWarning is the shared prefix a compaction (compactRoom) and a load (stateThrough,
// documentThrough) log when the stored updates through a version do not fold into a state that
// reads back as them.
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
	// unfoldableMu guards unfoldable: for each room whose rows at or below a compaction's fold
	// point did not fold into a state that reads back as them, that fold point, so the next
	// compaction of the same rows skips them rather than folding and logging them again
	// (compactRoom). An entry goes when the room compacts.
	unfoldableMu sync.Mutex
	unfoldable   map[string]rowVersion
	// duringCompactionFold runs once a compaction has read and folded a room's rows, before it
	// takes the room's lock to write the fold back. Nil outside tests; tests use it to hold a
	// compaction inside its fold while another writer changes the document.
	duringCompactionFold func(room string)
	// beforeFold runs as a load or a compaction starts to fold a room's rows (foldRead): once its
	// read transaction has committed when it read them into memory, inside that transaction when
	// it streams them. Nil outside tests; tests use it to see which connection a fold holds.
	beforeFold func(room string)
	// foldBufferLimit is the most stored update bytes a fold reads into memory (foldBuffer), zero
	// for foldBufferBytes. Zero outside tests; tests lower it to fold a small log while it streams.
	foldBufferLimit int64
	// foldSlots holds a token for each buffered fold under way (foldRead), so at most its capacity,
	// bufferedFolds, hold their rows in memory at once across every load and compaction of the
	// process. NewPgVersioned sizes it; tests replace it with a smaller one before any fold.
	foldSlots chan struct{}
	// maxPendingItems is the pending queue this store's decodes of a room's stored history take -
	// its fold, that fold's read-back and the document the state decodes into (fold,
	// readsBackOtherwise, documentThrough), and its check of a rebuild's seed (validateUpdate) -
	// zero for maxUpdateItems, which every production decode of document bytes takes (see
	// maxUpdateItems). Zero outside tests; a test lowers it (shrinkPendingItems) to make a log
	// that does not fold out of a few dozen items.
	maxPendingItems int
	// noteStored is the note of the Service that took this store last (New), which each append
	// gives the version it committed and each load the head it read (noteStoredUpdates), so the
	// Service's compaction queue checks the room. Nil for a store no Service took.
	noteStored atomic.Pointer[func(room string, head int64)]
}

// pgVersioned is p itself, the PgVersioned the Service notes appends and loads through.
func (p *PgVersioned) pgVersioned() *PgVersioned {
	return p
}

// noted gives the Service that took p the head of the room's rows an append committed or a load
// read (noteStored).
func (p *PgVersioned) noted(room string, head int64) {
	if note := p.noteStored.Load(); note != nil {
		(*note)(room, head)
	}
}

// roomLock is one room's in-process lock and how many callers hold it or wait for it.
type roomLock struct {
	sync.Mutex
	users int
}

// NewPgVersioned creates the versioned store for a Dispatch database.
func NewPgVersioned(database *store.Store) *PgVersioned {
	return &PgVersioned{store: database, foldSlots: make(chan struct{}, bufferedFolds)}
}

// pendingItems is the pending queue this store's decodes of a room's stored history take:
// maxUpdateItems, or the lower queue a test set (maxPendingItems).
func (p *PgVersioned) pendingItems() int {
	if p.maxPendingItems != 0 {
		return p.maxPendingItems
	}
	return maxUpdateItems
}

// Load returns the room's state at its head (stateThrough), constrained by a pending prune
// checkpoint when one exists, read as every document load reads (loadStored): in one
// repeatable-read transaction on the rooms pool, folded once that transaction has committed when
// the rows total at most foldBufferBytes, so a fold holds no connection a room load waits for, and
// inside it while it streams them when they total more; and noted once it has read more than one
// row. A room's load decodes the state it returns once more, in the room's load hook
// (onLoadDocument).
func (p *PgVersioned) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	var update []byte
	stamp, err := p.loadStored(ctx, room, func(head persistence.Version, updates updateSource) (err error) {
		update, err = stateThrough(room, head, updates, p.pendingItems())
		return err
	})
	if err != nil {
		return persistence.LoadResult{}, err
	}
	return persistence.LoadResult{Update: update, Version: persistence.Version(stamp.Version)}, nil
}

// loadStored is the read every document load (Load, LoadDocument) makes of the room's stored
// updates: in one repeatable-read read-only transaction on the rooms pool, the stamp of the state
// at their head and the count and bytes of the rows at or below it (readStoredLog), then those
// rows, which it hands use with the head to fold (foldRead): once the transaction has committed
// when they total at most foldBufferBytes, while it streams them inside it when they total more. A
// room with no stored update commits and hands use nothing. It returns the stamp it read, and once
// its transaction has ended it notes the room (noted) when it read more than one row: one row has
// nothing to fold, and any later append notes the room itself.
func (p *PgVersioned) loadStored(ctx context.Context, room string, use func(head persistence.Version, updates updateSource) error) (DocumentStamp, error) {
	if err := ctx.Err(); err != nil {
		return DocumentStamp{}, err
	}
	rooms, err := p.store.Pool.Rooms()
	if err != nil {
		return DocumentStamp{}, err
	}
	tx, err := rooms.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return DocumentStamp{}, fmt.Errorf("begin document load: %w", err)
	}
	defer tx.Rollback(ctx)
	stored, err := readStoredLog(ctx, tx, room)
	if err != nil {
		return DocumentStamp{}, err
	}
	head := persistence.Version(stored.stamp.Version)
	if head == 0 {
		if err := tx.Commit(ctx); err != nil {
			return DocumentStamp{}, fmt.Errorf("commit document load: %w", err)
		}
		return stored.stamp, nil
	}
	if err := p.foldRead(ctx, tx, room, head, stored.rows, stored.bytes, "document load", func(updates updateSource) error {
		return use(head, updates)
	}); err != nil {
		return DocumentStamp{}, err
	}
	if stored.rows > 1 {
		p.noted(room, int64(head))
	}
	return stored.stamp, nil
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
		version, err = p.appendUpdateTxClass(ctx, tx, room, update, contentChanged, credit)
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
	if err != nil {
		return version, err
	}
	// The committed row is noted once the document's locks are gone: the note waits on nothing
	// the append holds.
	p.noted(room, int64(version))
	return version, nil
}

// AppendUpdateTx appends an already validated V1 update inside tx, recording whether it changes
// the document's content: settlement versions a document only past a content update.
func (p *PgVersioned) AppendUpdateTx(ctx context.Context, tx pgx.Tx, room string, update []byte, contentChanged bool) (persistence.Version, error) {
	if err := crdt.ApplyUpdateV1(newDocumentCopy(), update, nil); err != nil {
		return 0, itemCapError(update, err)
	}
	return p.appendUpdateTxClass(ctx, tx, room, update, contentChanged, nil)
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

func (p *PgVersioned) appendUpdateTxClass(ctx context.Context, tx pgx.Tx, room string, update []byte, contentChanged bool, credit *UpdateCredit) (persistence.Version, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := lockDocumentRoom(ctx, tx, room); err != nil {
		return 0, err
	}
	if _, err := p.recoverPruneTx(ctx, tx, room); err != nil {
		return 0, err
	}
	var latest int64
	if err := tx.QueryRow(ctx, `
		select coalesce(max(version), 0) from doc_updates where artifact_id = $1
	`, room).Scan(&latest); err != nil {
		return 0, fmt.Errorf("read latest document update: %w", err)
	}
	// The gate: the document this update was made on must stand at the head. Two envoy-dispatch
	// processes serve one database, and an append of the other process's room is not in this
	// one's document; appending from a room behind the head is itself harmless, since Yjs updates
	// commute and this lock serialises them, but everything the appending transaction renders
	// from that document - an upload's or a conditional edit's version, a settlement's repairs -
	// is rendered from text the writer never saw. The order is the lock, then the cursor, then
	// the head: a peer of this very room whose append committed under this lock has already
	// advanced the cursor (appendUpdate), so two peers of one room never refuse each other.
	if err := refuseRoomBehindHead(room, appendingCursor(ctx, room), latest); err != nil {
		return 0, err
	}
	version := persistence.Version(latest + 1)
	// The insert announces the change itself (documentChangedChannel), as a data-modifying CTE
	// rather than a statement of its own, so a durable append is one round trip under the
	// document's advisory lock instead of two: every browser update is its own append
	// (PersistCoalesceWindow = -1). Why the announcement belongs inside this transaction is
	// notifyDocumentChanged's doc comment, which still carries a rebuild's. The room is passed
	// twice because its two parameters are different types: doc_updates.artifact_id is a uuid,
	// and pg_notify takes text. The same statement names the document in doc_compaction_owed, the
	// documents the compaction sweep reads (compactionCandidates), once the row it appends is not
	// the document's first: a first row has nothing to fold into, and a document already named
	// stays as it is.
	if _, err := tx.Exec(ctx, `
		with appended as (
			insert into doc_updates (artifact_id, version, update, content_changed) values ($1, $2, $3, $4)
		), owed as (
			insert into doc_compaction_owed (artifact_id) select $1 where $6 on conflict do nothing
		)
		select pg_notify('`+documentChangedChannel+`', $5)
	`, room, int64(version), update, contentChanged, room, latest > 0); err != nil {
		return 0, fmt.Errorf("append document update: %w", err)
	}
	authors, lastActor, include := credit.take()
	// An edit credited to authors names its latest edit source, none included when no one
	// peer made it; an update no one is credited with keeps the row's. The row is taken here, with
	// the write, so a route that goes on to append an event (an upload, issue creation) holds it
	// before its event, as an answer marking this document a copy owed does (owedCopiesOf), and
	// Ledger.Commit's later recordLatestEditSource waits on nothing.
	if include && (len(authors) > 0 || lastActor != nil) {
		if err := markUpdateOwedBy(ctx, tx, room, lastActor, anyPerson(authors)); err != nil {
			return 0, err
		}
	} else if err := markUpdateOwed(ctx, tx, room); err != nil {
		return 0, err
	}
	if include {
		if err := upsertPendingAuthors(ctx, tx, room, pendingAuthorsAt(authors, version)); err != nil {
			return 0, err
		}
	}
	return version, nil
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
	update, err := stateThrough(room, version, storedUpdates(ctx, tx, room, version), p.pendingItems())
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
		if _, err := p.recoverPruneTx(ctx, tx, room); err != nil {
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

// RebuildTx replaces a room's durable history with one fresh update at the next version, inside
// tx, which holds the room's lock until it ends. Its caller has already proved that ygo cannot load
// the old merged history; this method never makes that destructive decision itself.
func (p *PgVersioned) RebuildTx(ctx context.Context, tx pgx.Tx, room string, seed []byte) (RebuildReport, error) {
	if err := validateUpdate(seed, p.pendingItems()); err != nil {
		return RebuildReport{}, wrapUnlessTooLarge(itemCapError(seed, err), "validate rebuilt document seed")
	}
	if err := lockDocumentRoom(ctx, tx, room); err != nil {
		return RebuildReport{}, err
	}
	if _, err := p.recoverPruneTx(ctx, tx, room); err != nil {
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
	// The seed is the document's one row, so it has nothing a compaction could fold.
	if _, err := tx.Exec(ctx, `delete from doc_compaction_owed where artifact_id = $1`, room); err != nil {
		return RebuildReport{}, fmt.Errorf("delete rebuilt document's compaction entry: %w", err)
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
			`delete from doc_compaction_owed where artifact_id = $1`,
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

// recoverPruneTx finishes a prune whose checkpoint committed and whose deletion of the updates past
// it did not (PruneAfter), inside tx, which holds the room's lock, and reports whether there was
// one to finish.
func (p *PgVersioned) recoverPruneTx(ctx context.Context, tx pgx.Tx, room string) (bool, error) {
	var ceiling int64
	err := tx.QueryRow(ctx, `select ceiling from doc_checkpoints where artifact_id = $1 for update`, room).Scan(&ceiling)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read document prune checkpoint: %w", err)
	}
	if _, err := tx.Exec(ctx, `delete from doc_updates where artifact_id = $1 and version > $2`, room, ceiling); err != nil {
		return false, fmt.Errorf("recover pruned document updates: %w", err)
	}
	if _, err := tx.Exec(ctx, `delete from doc_checkpoints where artifact_id = $1`, room); err != nil {
		return false, fmt.Errorf("clear recovered document checkpoint: %w", err)
	}
	return true, nil
}

// errRoomLockBusy is a room lock another holder has, for work that does not wait for it.
var errRoomLockBusy = errors.New("document room lock is held")

func (p *PgVersioned) withRoomLock(ctx context.Context, room string, fn func(*pgxpool.Conn) error) error {
	return p.lockRoom(ctx, room, true, p.pool(), fn)
}

// connectionSource is a pool lockRoom takes its connection from: the shared pool for the durable
// writers (withRoomLock), the compaction pool for a compaction's write (compactRoom).
type connectionSource interface {
	Acquire(ctx context.Context) (*pgxpool.Conn, error)
}

// lockRoom runs fn holding a connection of source and the room's advisory lock. When wait is false
// it takes neither lock another holder has, and returns errRoomLockBusy instead.
func (p *PgVersioned) lockRoom(ctx context.Context, room string, wait bool, source connectionSource, fn func(*pgxpool.Conn) error) error {
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

	conn, err := source.Acquire(ctx)
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

// documentRooms drains the id list before its caller does anything with it: the work each id
// leads to - a room close - takes a pooled connection of its own, and holding the rows open
// across that would be a second connection for work the first is waiting on.
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
// 131,000 items. Repeated versions can take a live state to it, so every joined write (applyLive
// and applyMargin, through recordLiveOperation) decodes its state before appending once its clocks
// pass this cap (refuseUndecodable, growth.go) - not settlement's own repairs, which write outside
// both and never reach it (growth.go's own doc comment names the three).
//
// It is also the pending queue every decode of document bytes takes, whether the service builds
// the decoder (newDocumentCopy, and newCappedDocumentCopy at the store's and the service's own
// cap: PgVersioned.pendingItems and Service.maxPendingItems, both this in production) or ygo
// builds it for the service (Server.MaxPendingItems: the rooms). ygo's decoder parks each item
// whose parent it cannot place yet, and each later item of that client behind it, and refuses the
// update once its queue is full, 100,000 items at ygo's default. An update decoded alone parks
// every item that leans on the document it was written against, and no update carries more items
// than this, so no decode parks past it. ygo's own check of each update the service broadcasts
// (Server.BroadcastUpdate) takes no pending queue at all: it is the check ygo's bundled stores
// run, since the room has already applied the update (reearth/ygo#268, in the pinned fork).
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
