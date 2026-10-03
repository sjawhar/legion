package docs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// PgVersioned persists a room's Yjs V1 updates in Dispatch's Postgres store.
type PgVersioned struct {
	store *store.Store
	locks sync.Map
}

// NewPgVersioned creates the versioned store for a Dispatch database.
func NewPgVersioned(database *store.Store) *PgVersioned {
	return &PgVersioned{store: database}
}

// Load returns the room's materialized head, constrained by a pending prune
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
	update, err := p.mergeUpTo(ctx, tx, room, head)
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

// AppendUpdate validates and stores one incremental V1 update as content.
func (p *PgVersioned) AppendUpdate(ctx context.Context, room string, update []byte) (persistence.Version, error) {
	return p.appendUpdate(ctx, room, update, true)
}

// AppendUpdateWithClass validates and stores one incremental V1 update with its rendered-content classification.
func (p *PgVersioned) AppendUpdateWithClass(ctx context.Context, room string, update []byte, contentChanged bool) (persistence.Version, error) {
	return p.appendUpdate(ctx, room, update, contentChanged)
}

func (p *PgVersioned) appendUpdate(ctx context.Context, room string, update []byte, contentChanged bool) (persistence.Version, error) {
	if err := crdt.ApplyUpdateV1(newDocumentCopy(), update, nil); err != nil {
		return 0, err
	}
	var version persistence.Version
	err := p.withRoomLock(ctx, room, func(conn *pgxpool.Conn) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin document update: %w", err)
		}
		defer tx.Rollback(ctx)
		version, err = p.appendUpdateTxClass(ctx, tx, room, update, contentChanged)
		if err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit document update: %w", err)
		}
		return nil
	})
	return version, err
}

// AppendUpdateTx appends an already validated V1 update inside tx, recording whether it changes
// the document's content: settlement versions a document only past a content update.
func (p *PgVersioned) AppendUpdateTx(ctx context.Context, tx pgx.Tx, room string, update []byte, contentChanged bool) (persistence.Version, error) {
	if err := crdt.ApplyUpdateV1(newDocumentCopy(), update, nil); err != nil {
		// ygo refuses an update declaring more than maxUpdateItems items with the same error it
		// gives a malformed one. The server encoded this update itself, so when its header
		// declares more than the cap, the cap is the cause and the document is the caller's to
		// shrink (LEGION-465); any other refusal of a server-encoded update is a fault to log.
		if items, ok := updateItems(update); ok && items > maxUpdateItems {
			return 0, fmt.Errorf("%w: its %d items exceed the %d one document update can hold (%v)", ErrDocumentTooLarge, items, maxUpdateItems, err)
		}
		return 0, err
	}
	return p.appendUpdateTxClass(ctx, tx, room, update, contentChanged)
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

func (p *PgVersioned) appendUpdateTxClass(ctx context.Context, tx pgx.Tx, room string, update []byte, contentChanged bool) (persistence.Version, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := lockDocumentRoom(ctx, tx, room); err != nil {
		return 0, err
	}
	if err := p.recoverPruneTx(ctx, tx, room); err != nil {
		return 0, err
	}
	var latest int64
	if err := tx.QueryRow(ctx, `
		select coalesce(max(version), 0) from doc_updates where artifact_id = $1
	`, room).Scan(&latest); err != nil {
		return 0, fmt.Errorf("read latest document update: %w", err)
	}
	version := persistence.Version(latest + 1)
	if _, err := tx.Exec(ctx, `
		insert into doc_updates (artifact_id, version, update, content_changed) values ($1, $2, $3, $4)
	`, room, int64(version), update, contentChanged); err != nil {
		return 0, fmt.Errorf("append document update: %w", err)
	}
	if err := markSettlementPending(ctx, tx, room); err != nil {
		return 0, err
	}
	return version, nil
}

// markSettlementPending records, in the transaction that appends a document update, that the
// document owes a settlement. Every update a room persists arms a settlement, and the timer that
// runs it lives only in memory, so a settlement a shutdown cut short is found here by the room's
// next load (onLoadDocument) and by the resumption (RunSettlementResumption). The settlement that
// covers the update deletes the row in the transaction that commits its writes
// (clearSettlementPending). The caller holds the document's advisory lock, which orders this row's
// writers as it orders the updates.
func markSettlementPending(ctx context.Context, tx pgx.Tx, room string) error {
	if _, err := tx.Exec(ctx, `
		insert into doc_settlements_pending (artifact_id) values ($1)
		on conflict (artifact_id) do update
		set marked_at = now()
	`, room); err != nil {
		return fmt.Errorf("record the document's pending settlement: %w", err)
	}
	return nil
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

// settlementPending reports whether the document owes a settlement that no settlement has
// committed.
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
		  and version <= coalesce(
			(select ceiling from doc_checkpoints where artifact_id = $1),
			(select max(version) from doc_updates where artifact_id = $1),
			0
		  )
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
		  and version <= coalesce(
			(select ceiling from doc_checkpoints where artifact_id = $1),
			(select max(version) from doc_updates where artifact_id = $1),
			0
		  )
	`, room, int64(version)).Scan(&update, &updatedAt)
	if err == pgx.ErrNoRows {
		return nil, persistence.VersionMeta{}, false, nil
	}
	if err != nil {
		return nil, persistence.VersionMeta{}, false, fmt.Errorf("get document update: %w", err)
	}
	return update, persistence.VersionMeta{Version: version, UpdatedAt: updatedAt}, true, nil
}

// MaterializeAt rebuilds the V1 state at version, or the latest state before
// it when a requested version is newer than the room head.
func (p *PgVersioned) MaterializeAt(ctx context.Context, room string, version persistence.Version) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if version == 0 {
		return nil, nil
	}
	// A version read opens its own transaction, so it marks its context like every other
	// opener: the pool refuses a second connection taken under it (store.ErrNestedAcquire).
	ctx = store.WithTransactionTracking(ctx)
	tx, err := p.pool().Begin(ctx)
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
	update, err := p.mergeUpTo(ctx, tx, room, version)
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

// Compact folds older updates into the oldest retained record without changing
// the room's materialized state. Under a context compactIfIdle marked, it leaves a room whose lock
// another holder has for a later compaction.
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
		rows, err := tx.Query(ctx, `
			select version, update, content_changed from doc_updates where artifact_id = $1 order by version asc
		`, room)
		if err != nil {
			return fmt.Errorf("list compactable document updates: %w", err)
		}
		var versions []int64
		var updates [][]byte
		var contentClasses []bool
		for rows.Next() {
			var version int64
			var update []byte
			var contentChanged bool
			if err := rows.Scan(&version, &update, &contentChanged); err != nil {
				rows.Close()
				return fmt.Errorf("scan compactable document update: %w", err)
			}
			versions = append(versions, version)
			updates = append(updates, update)
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
		deleted = len(versions) - keep
		merged, err := crdt.MergeUpdatesV1(updates[:deleted+1]...)
		if err != nil {
			return fmt.Errorf("merge compacted document updates: %w", err)
		}
		contentChanged := false
		for index, class := range contentClasses[:deleted+1] {
			contentChanged = contentChanged || (versions[index] > coveredCursor && class)
		}
		if _, err := tx.Exec(ctx, `
			update doc_updates set update = $3, content_changed = $4 where artifact_id = $1 and version = $2
		`, room, versions[deleted], merged, contentChanged); err != nil {
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
		return RebuildReport{}, fmt.Errorf("validate rebuilt document seed: %w", err)
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

func (p *PgVersioned) head(ctx context.Context, q Queryer, room string) (persistence.Version, error) {
	var version int64
	if err := q.QueryRow(ctx, `
		select coalesce(
			(select ceiling from doc_checkpoints where artifact_id = $1),
			(select max(version) from doc_updates where artifact_id = $1),
			0
		)
	`, room).Scan(&version); err != nil {
		return 0, fmt.Errorf("read document head: %w", err)
	}
	return persistence.Version(version), nil
}

func (p *PgVersioned) mergeUpTo(ctx context.Context, tx pgx.Tx, room string, version persistence.Version) ([]byte, error) {
	rows, err := tx.Query(ctx, `
		select update from doc_updates
		where artifact_id = $1 and version <= $2
		order by version asc
	`, room, int64(version))
	if err != nil {
		return nil, fmt.Errorf("read document updates: %w", err)
	}
	defer rows.Close()
	var updates [][]byte
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			return nil, fmt.Errorf("scan document update: %w", err)
		}
		updates = append(updates, update)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read document updates: %w", err)
	}
	if len(updates) == 0 {
		return nil, nil
	}
	update, err := crdt.MergeUpdatesV1(updates...)
	if err != nil {
		return nil, fmt.Errorf("merge document updates: %w", err)
	}
	return update, nil
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
	value, _ := p.locks.LoadOrStore(room, &sync.Mutex{})
	lock := value.(*sync.Mutex)
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

func (p *PgVersioned) pool() *store.Pool {
	return p.store.Pool
}

func (s *Service) CompactAll(ctx context.Context, keep int) error {
	ctx = store.WithTransactionTracking(ctx)
	rooms, err := s.documentRooms(ctx, "document rooms", `select id::text from artifacts where kind = 'doc'`)
	if err != nil {
		return err
	}
	for _, artifactID := range rooms {
		if _, err := s.persistence.Compact(ctx, artifactID, keep); err != nil {
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
//
// It is also the pending queue every decode of document bytes takes, whether the service builds
// the decoder (newDocumentCopy) or ygo builds it for the service (Server.MaxPendingItems: the
// rooms, and ygo's check of each update the service broadcasts). ygo's decoder parks each item
// whose parent it cannot place yet and refuses the update once its queue is full, 100,000 items at
// ygo's default. An update decoded alone parks every item that leans on the document it was
// written against, and no update carries more items than this, so no decode parks past it.
// TestTheStoreTakesOneBrowserUpdateItsRoomTook and TestASettlementStampsMoreBlocksThanYgosDefaultQueue
// are the updates ygo's default refuses.
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
