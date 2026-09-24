package docs

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// PgVersioned persists a room's Yjs V1 updates in Dispatch's Postgres store.
type PgVersioned struct {
	store     *store.Store
	locks     sync.Map
	openRooms sync.Once
	rooms     *pgxpool.Pool
	roomsErr  error
}

// NewPgVersioned creates the versioned store for a Dispatch database.
func NewPgVersioned(database *store.Store) *PgVersioned {
	return &PgVersioned{store: database}
}

// roomsPoolSize bounds the connections document loads use. Loading takes no lock and always
// finishes, so one is enough for the pool to be deadlock-free; a few let cold rooms load
// concurrently without enlarging the shared pool.
const roomsPoolSize = 4

// roomsPool is where a document load takes its connection. A load runs while the request that
// triggered it holds an open transaction on the shared pool - it happens when an anchored write
// stamps its mark in a room nobody has opened yet - and it is deliberately not part of that
// transaction, because the loaded room outlives the request and must not roll back with it. A
// second connection from the shared pool is exactly what deadlocks it (store.ErrNestedAcquire),
// so this one is the document service's own.
func (p *PgVersioned) roomsPool() (*pgxpool.Pool, error) {
	p.openRooms.Do(func() {
		config := p.store.Pool.Config().Copy()
		config.MaxConns = roomsPoolSize
		config.MinConns = 0
		p.rooms, p.roomsErr = pgxpool.NewWithConfig(context.Background(), config)
		if p.roomsErr != nil {
			p.roomsErr = fmt.Errorf("open document rooms pool: %w", p.roomsErr)
		}
	})
	return p.rooms, p.roomsErr
}

// Close releases the document rooms pool.
func (p *PgVersioned) Close() {
	p.openRooms.Do(func() {})
	if p.rooms != nil {
		p.rooms.Close()
	}
}

// Load returns the room's materialized head, constrained by a pending prune
// checkpoint when one exists.
func (p *PgVersioned) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	if err := ctx.Err(); err != nil {
		return persistence.LoadResult{}, err
	}
	rooms, err := p.roomsPool()
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

// AppendUpdate validates and stores one incremental V1 update as content.
func (p *PgVersioned) AppendUpdate(ctx context.Context, room string, update []byte) (persistence.Version, error) {
	return p.appendUpdate(ctx, room, update, true)
}

// AppendUpdateWithClass validates and stores one incremental V1 update with its rendered-content classification.
func (p *PgVersioned) AppendUpdateWithClass(ctx context.Context, room string, update []byte, contentChanged bool) (persistence.Version, error) {
	return p.appendUpdate(ctx, room, update, contentChanged)
}

func (p *PgVersioned) appendUpdate(ctx context.Context, room string, update []byte, contentChanged bool) (persistence.Version, error) {
	if err := crdt.ApplyUpdateV1(crdt.New(), update, nil); err != nil {
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
	if err := crdt.ApplyUpdateV1(crdt.New(), update, nil); err != nil {
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
	return version, nil
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
// the room's materialized state.
func (p *PgVersioned) Compact(ctx context.Context, room string, keep int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if keep <= 0 {
		return 0, nil
	}
	deleted := 0
	err := p.withRoomLock(ctx, room, func(conn *pgxpool.Conn) error {
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
	return deleted, err
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

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (p *PgVersioned) head(ctx context.Context, q queryRower, room string) (persistence.Version, error) {
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

func (p *PgVersioned) withRoomLock(ctx context.Context, room string, fn func(*pgxpool.Conn) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	value, _ := p.locks.LoadOrStore(room, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	conn, err := p.pool().Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire document connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `select pg_advisory_lock(hashtext($1))`, room); err != nil {
		return fmt.Errorf("lock document room: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `select pg_advisory_unlock(hashtext($1))`, room) }()
	return fn(conn)
}

func (p *PgVersioned) pool() *store.Pool {
	return p.store.Pool
}

func (s *Service) CompactAll(ctx context.Context, keep int) error {
	rows, err := s.store.Pool.Query(ctx, `select id::text from artifacts where kind = 'doc'`)
	if err != nil {
		return fmt.Errorf("list document rooms: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var artifactID string
		if err := rows.Scan(&artifactID); err != nil {
			return fmt.Errorf("scan document room: %w", err)
		}
		if _, err := s.persistence.Compact(ctx, artifactID, keep); err != nil {
			return fmt.Errorf("compact document %s: %w", artifactID, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list document rooms: %w", err)
	}
	return nil
}

var _ persistence.VersionedPersistence = (*PgVersioned)(nil)
