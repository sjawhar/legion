package docs

// A document's compaction: the fold of its oldest stored rows into one (compactRoom), and the
// rules that pick how far it folds (compactRule).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/reearth/ygo/persistence"
)

// compactTail is how long a stored row stays as it was written: no compaction folds or rewrites a
// row created within it of the database's clock. A document being edited keeps appending rows, and
// a compaction that folded them as they came would fold the whole document again for every few
// edits and rewrite the head row each time, which moves the stamp every cold read of the document
// is cached by (DocumentStamp). Rows that wait out the tail fold together, once.
const compactTail = 2 * time.Minute

// compactRule picks the row a compaction folds into, its fold point: every row at or below it is
// folded into it, and every row below it is deleted.
type compactRule struct {
	// keep is, when positive, how many of the newest rows at or below the head stand after the
	// compaction, the fold point the oldest of them (the keep-N rule, PgVersioned.Compact). Zero
	// is the tail rule: the fold point is the newest row at or below the head that is older than
	// compactTail on the database's clock, so nothing younger is touched.
	keep int
}

// tailRule is the rule every compaction Dispatch schedules folds under.
var tailRule = compactRule{}

// foldPointSQL selects the rule's fold point for the room $1 names, null when there is none: $2 is
// the tail in seconds for the tail rule, and for the keep-N rule how many newer rows to pass over.
func (r compactRule) foldPointSQL() (string, any) {
	if r.keep > 0 {
		return `select (
			select version from doc_updates
			where artifact_id = $1 and version <= ` + headVersion + `
			order by version desc offset $2 limit 1
		)`, r.keep - 1
	}
	return `select max(version) from doc_updates
		where artifact_id = $1 and version <= ` + headVersion + `
			and created_at < now() - make_interval(secs => $2)`, compactTail.Seconds()
}

// compactOutcome is what one compaction of a room did, for the caller that decides when to try the
// room again.
type compactOutcome int

const (
	// compactNothing: the room has no fold point, or no row below it.
	compactNothing compactOutcome = iota
	// compactCompacted: the rows below the fold point were folded into it and deleted.
	compactCompacted
	// compactDropped: the rows changed between the read and the write - a prune, a rebuild, a
	// delete, another compaction - or the write found a prune to finish first, so the fold was
	// thrown away and a later compaction folds what the rows are then.
	compactDropped
	// compactBusy: the compaction did not wait for the room's lock, and another holder had it.
	compactBusy
	// compactUnfoldable: the rows do not fold into a state that reads back as them, now or at an
	// earlier compaction of the same fold point, and are left as stored.
	compactUnfoldable
)

// compactResult is a compaction's outcome and how many rows it deleted.
type compactResult struct {
	outcome compactOutcome
	deleted int
}

// rowVersion names a row: its version and the transaction that wrote it, Postgres's xmin, which
// every rewrite of the row changes.
type rowVersion struct {
	version int64
	xmin    string
}

// foldedRow is one row at or below a fold point as a compaction's read found it.
type foldedRow struct {
	rowVersion
	contentChanged bool
}

// compactFold is what a compaction's read and fold found: the rows at or below the fold point,
// oldest first, and the state they fold into, or misread naming why they do not fold.
type compactFold struct {
	rows    []foldedRow
	state   []byte
	misread error
}

// point is the fold point, the newest row the fold read.
func (f compactFold) point() rowVersion {
	return f.rows[len(f.rows)-1].rowVersion
}

// compactRoom folds the room's rows at or below the fold point rule picks into that row, which then
// holds their state (fold): what the document still holds of them, and none of the content they
// inserted that a later update deleted. It deletes the rows below it, so the room's state is
// unchanged, and so is every retained version's. A row newer than the fold point is left as
// written, byte for byte.
//
// It holds no lock while it folds, so no write waits for a fold. It reads on the compaction pool
// (store.Pool.Compaction), in one repeatable-read transaction, each row's version and xmin and
// either the rows themselves, for a fold with no connection held, or, past foldBufferBytes, a
// stream of them it folds before that transaction ends. Its write takes a connection of the same
// pool and the room's lock (lockRoom), waiting for the lock only when wait is set and otherwise
// leaving a busy room (compactBusy), then checks inside its transaction that the rows at or below
// the fold point are the ones it folded, version for version and xmin for xmin. A write that found
// them changed - a prune, a rebuild, a delete, a compaction by another process or by an older
// build, an append that reused a pruned number - drops the fold (compactDropped); so does one that
// had to finish a stranded prune (recoverPruneTx), which it commits, so the next compaction folds
// the rows that prune left. A row appended above the fold point changes nothing it folded.
//
// The folded row's content_changed says whether any row it folded past the settlement cursor -
// the latest version's doc_update_version, read inside the write's transaction, under the lock
// every settlement takes - changed content, as each folded row said for itself.
//
// Rows that do not fold into a state that reads back as them are left as stored and logged once
// for their fold point; a later compaction of the same fold point skips them without folding
// (compactUnfoldable), and one after the rows changed folds them again.
//
// A compaction that waits for the lock and writes nothing - nothing to fold, or rows that do not
// fold - still waits for the lock and lets it go (passRoomLock).
func (p *PgVersioned) compactRoom(ctx context.Context, room string, rule compactRule, wait bool) (compactResult, error) {
	if err := ctx.Err(); err != nil {
		return compactResult{}, err
	}
	pool, err := p.store.Pool.Compaction()
	if err != nil {
		return compactResult{}, err
	}
	folded, ended, err := p.readCompaction(ctx, pool, room, rule)
	if err != nil {
		return compactResult{}, err
	}
	if folded.rows == nil {
		return compactResult{outcome: ended}, p.passRoomLock(ctx, pool, room, wait)
	}
	if p.duringCompactionFold != nil {
		p.duringCompactionFold(room)
	}
	if folded.misread != nil {
		p.recordUnfoldable(room, folded.point(), folded.misread)
		return compactResult{outcome: compactUnfoldable}, p.passRoomLock(ctx, pool, room, wait)
	}
	result, err := p.writeCompaction(ctx, pool, room, wait, folded)
	if err == nil && result.outcome == compactCompacted {
		p.forgetUnfoldable(room)
	}
	return result, err
}

// passRoomLock waits for room's lock and lets it go, for a compaction that waits (wait) and writes
// nothing. A failed room's close (servicePersistenceAdapter.Compact) is one, and it must end only
// after a writer holding that lock has committed: a repair written into the failed instance
// before it is stored (backfillBlockIDs, settleRoomWithin) is replayed into no room that loads
// later, so the eviction that recovery waits for has to queue behind the repair's commit, and the
// room that loads next reads the repair from the store and is born at the head. A compaction that
// does not wait passes nothing.
func (p *PgVersioned) passRoomLock(ctx context.Context, pool *pgxpool.Pool, room string, wait bool) error {
	if !wait {
		return nil
	}
	return p.lockRoom(ctx, room, true, pool, func(*pgxpool.Conn) error { return nil })
}

// readCompaction reads the rows at or below the room's fold point and folds them: in memory once
// its transaction has ended when they total at most foldBufferBytes, streamed inside it otherwise
// (foldRead, which a load shares). It returns no rows, and the outcome the compaction ends with,
// when there is nothing to fold or the negative cache already holds this fold point.
func (p *PgVersioned) readCompaction(ctx context.Context, pool *pgxpool.Pool, room string, rule compactRule) (compactFold, compactOutcome, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return compactFold{}, compactNothing, fmt.Errorf("begin compaction read: %w", err)
	}
	defer tx.Rollback(ctx)
	query, argument := rule.foldPointSQL()
	var point *int64
	if err := tx.QueryRow(ctx, query, room, argument).Scan(&point); err != nil {
		return compactFold{}, compactNothing, fmt.Errorf("read compaction fold point: %w", err)
	}
	if point == nil {
		return compactFold{}, compactNothing, nil
	}
	rows, err := tx.Query(ctx, `
		select version, xmin::text, content_changed, octet_length(update)
		from doc_updates where artifact_id = $1 and version <= $2
		order by version asc
	`, room, *point)
	if err != nil {
		return compactFold{}, compactNothing, fmt.Errorf("list compacted document updates: %w", err)
	}
	var folded compactFold
	var total int64
	for rows.Next() {
		var row foldedRow
		var size int64
		if err := rows.Scan(&row.version, &row.xmin, &row.contentChanged, &size); err != nil {
			rows.Close()
			return compactFold{}, compactNothing, fmt.Errorf("scan compacted document update: %w", err)
		}
		folded.rows = append(folded.rows, row)
		total += size
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return compactFold{}, compactNothing, fmt.Errorf("list compacted document updates: %w", err)
	}
	if len(folded.rows) < 2 {
		return compactFold{}, compactNothing, nil
	}
	if p.knownUnfoldable(room, folded.point()) {
		return compactFold{}, compactUnfoldable, nil
	}
	through := persistence.Version(*point)
	if err := p.foldRead(ctx, tx, room, through, len(folded.rows), total, "compaction read", func(updates updateSource) error {
		var err error
		if folded.state, _, folded.misread, err = fold(updates, p.maxPendingItems); err != nil {
			return fmt.Errorf("fold compacted document updates: %w", err)
		}
		return nil
	}); err != nil {
		return compactFold{}, compactNothing, err
	}
	return folded, compactNothing, nil
}

// writeCompaction writes folded back under the room's lock, on a connection of pool, when the rows
// at or below its fold point are still the ones it folded (compactRoom), and takes the document off
// the sweep's list (doc_compaction_owed) when no row stands above the fold point.
func (p *PgVersioned) writeCompaction(ctx context.Context, pool *pgxpool.Pool, room string, wait bool, folded compactFold) (compactResult, error) {
	result := compactResult{outcome: compactDropped}
	point := folded.point()
	err := p.lockRoom(ctx, room, wait, pool, func(conn *pgxpool.Conn) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin compact document: %w", err)
		}
		defer tx.Rollback(ctx)
		if err := lockDocumentRoom(ctx, tx, room); err != nil {
			return err
		}
		recovered, err := p.recoverPruneTx(ctx, tx, room)
		if err != nil {
			return err
		}
		if recovered {
			if err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("commit recovered document prune: %w", err)
			}
			return nil
		}
		standing, err := standingRows(ctx, tx, room, point.version)
		if err != nil {
			return err
		}
		if !slices.EqualFunc(standing, folded.rows, func(stands rowVersion, read foldedRow) bool {
			return stands == read.rowVersion
		}) {
			return nil
		}
		var cursor int64
		if err := tx.QueryRow(ctx, `
			select coalesce((
				select doc_update_version from artifact_versions where artifact_id = $1 order by number desc limit 1
			), 0)
		`, room).Scan(&cursor); err != nil {
			return fmt.Errorf("read compactable document version cursor: %w", err)
		}
		contentChanged := false
		for _, row := range folded.rows {
			contentChanged = contentChanged || (row.version > cursor && row.contentChanged)
		}
		if _, err := tx.Exec(ctx, `
			update doc_updates set update = $3, content_changed = $4 where artifact_id = $1 and version = $2
		`, room, point.version, folded.state, contentChanged); err != nil {
			return fmt.Errorf("fold compacted document updates: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			delete from doc_updates where artifact_id = $1 and version < $2
		`, room, point.version); err != nil {
			return fmt.Errorf("delete compacted document updates: %w", err)
		}
		// A document left with no row above the fold point has nothing more to fold, so it leaves
		// the sweep's list (compactionCandidates); one with younger rows above it stays there.
		// Every append that names it takes the lock this transaction holds.
		if _, err := tx.Exec(ctx, `
			delete from doc_compaction_owed where artifact_id = $1
				and not exists (select 1 from doc_updates where artifact_id = $1 and version > $2)
		`, room, point.version); err != nil {
			return fmt.Errorf("delete compacted document's compaction entry: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit compact document: %w", err)
		}
		result = compactResult{outcome: compactCompacted, deleted: len(folded.rows) - 1}
		return nil
	})
	if errors.Is(err, errRoomLockBusy) {
		return compactResult{outcome: compactBusy}, nil
	}
	if err != nil {
		return compactResult{}, err
	}
	return result, nil
}

// standingRows is the version and xmin of each of the room's rows through version, oldest first,
// as tx reads them.
func standingRows(ctx context.Context, tx pgx.Tx, room string, version int64) ([]rowVersion, error) {
	rows, err := tx.Query(ctx, `
		select version, xmin::text from doc_updates
		where artifact_id = $1 and version <= $2
		order by version asc
	`, room, version)
	if err != nil {
		return nil, fmt.Errorf("reread compacted document updates: %w", err)
	}
	defer rows.Close()
	var standing []rowVersion
	for rows.Next() {
		var row rowVersion
		if err := rows.Scan(&row.version, &row.xmin); err != nil {
			return nil, fmt.Errorf("scan compacted document update: %w", err)
		}
		standing = append(standing, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reread compacted document updates: %w", err)
	}
	return standing, nil
}

// knownUnfoldable reports whether an earlier compaction found the room's rows through point do not
// fold (recordUnfoldable).
func (p *PgVersioned) knownUnfoldable(room string, point rowVersion) bool {
	p.unfoldableMu.Lock()
	defer p.unfoldableMu.Unlock()
	known, ok := p.unfoldable[room]
	return ok && known == point
}

// recordUnfoldable remembers that the room's rows through point do not fold into a state that reads
// back as them, and logs it unless it was already remembered: two compactions of one room can fold
// the same rows at once, and the log line names the rows once.
func (p *PgVersioned) recordUnfoldable(room string, point rowVersion, misread error) {
	p.unfoldableMu.Lock()
	known, ok := p.unfoldable[room]
	already := ok && known == point
	if !already {
		if p.unfoldable == nil {
			p.unfoldable = make(map[string]rowVersion)
		}
		p.unfoldable[room] = point
	}
	p.unfoldableMu.Unlock()
	if already {
		return
	}
	slog.Warn(misreadWarning+"; compaction leaves them as stored",
		"room", room, "version", point.version, "error", misread)
}

// forgetUnfoldable drops the room's entry once it has compacted: its rows through any earlier fold
// point are folded now.
func (p *PgVersioned) forgetUnfoldable(room string) {
	p.unfoldableMu.Lock()
	defer p.unfoldableMu.Unlock()
	delete(p.unfoldable, room)
}

// Compact is ygo's VersionedPersistence compaction, under the keep-N rule: it folds the room's rows
// at or below the head into the keep-th newest of them (compactRoom), waiting for the room's lock.
// Dispatch schedules none of these; every compaction it runs is the tail rule's. An older build
// sharing the database runs its own binary's Compact, never this one. This stays for ygo's
// VersionedPersistence interface and for tests that ask for a fixed keep.
func (p *PgVersioned) Compact(ctx context.Context, room string, keep int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if keep <= 0 {
		return 0, nil
	}
	result, err := p.compactRoom(ctx, room, compactRule{keep: keep}, true)
	return result.deleted, err
}
