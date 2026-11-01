package docs

// The compaction sweep: the hourly pass over every document with rows to fold, as
// doc_compaction_owed names them, which compacts what the compaction queue never saw or let go -
// the rooms a restart, a Shutdown or a Quiesce dropped from it, and the documents another process
// wrote and never loaded here.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// compactionSweepLease names the lease a sweep pass runs under, and compactionSweepPeriod the
// period one pass serves (duty.Exclusive): a pass reads the rows of every document with rows to
// fold, so two envoy-dispatch processes sweeping, or one process sweeping again after a restart,
// inside one aligned hour is that whole read again for what the queue already compacts.
const (
	compactionSweepLease  = "document-compaction"
	compactionSweepPeriod = time.Hour
)

// RunCompactionSweep compacts, once an aligned hour across every process that serves the database
// and across restarts (duty.Exclusive, compactionSweepLease), every document due a compaction by
// the queue's own rule (checkCompaction): a row after its first older than compactMaxTailAge, or
// compactMinRows of them older than compactTail. It runs until ctx ends. Its ticks are
// duty.RunLeased's, a ticker's of compactionSweepPeriod: the first at the moment it starts, so a
// document a restart dropped from the queue is swept at once unless some process has already swept
// this hour, then one each period, which falls in the next aligned hour while a pass takes less
// than an hour. It runs whether or not NATS is configured: the compaction is what keeps text a
// write deleted out of doc_updates and its backups, and it needs nothing but the database. leases
// is required, as it is of every leased loop.
func (s *Service) RunCompactionSweep(ctx context.Context, leases duty.Leases) {
	ctx = store.WithTransactionTracking(ctx)
	duty.RunLeased(ctx, leases, compactionSweepLease, compactionSweepPeriod, duty.TickAtStart, s.compactionSweepPass)
}

// compactionSweepPass lists the documents with rows to fold (compactionCandidates) and tries each
// one's compaction as the queue's check does (tryCompaction): compacted when due, under the tail
// rule and without waiting for its lock, one at a time on the compaction pool. The list is read
// whole before the first compaction, so no cursor holds a connection while one runs. A listed
// document with no row after its first - one a prune or an artifact's deletion left, or one another
// process compacted - is taken off the list (forgetCompactionCandidate). A try that should be made
// again later - a compaction that found the room's lock held or another writer changing its rows
// (compactBusy, compactDropped), or a check or compaction that failed - is handed to this service's
// compaction queue rather than retried here: the queue retries it with backoff, and the pass goes
// on to the next document. It ends at the document it is on once ctx ends.
func (s *Service) compactionSweepPass(ctx context.Context) {
	pool, err := s.store.Pool.Compaction()
	if err != nil {
		slog.Error("dispatch: document compaction sweep", "error", err)
		return
	}
	rooms, err := compactionCandidates(ctx, pool)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("dispatch: document compaction sweep", "error", err)
		}
		return
	}
	compacted, deleted, left, forgotten := 0, 0, 0, 0
	leave := func(room string) {
		s.compactions.note(room)
		left++
	}
	for _, room := range rooms {
		if ctx.Err() != nil {
			return
		}
		attempt := s.tryCompaction(ctx, room, false)
		switch attempt.try {
		case tryCompacted:
			compacted++
			deleted += attempt.deleted
		case tryLater:
			leave(room)
		case tryNotDue:
			if attempt.check.later > 0 {
				continue
			}
			removed, err := forgetCompactionCandidate(ctx, pool, room)
			if err != nil {
				warnCompaction(ctx, warnCompactionForget, room, err)
			} else if removed {
				forgotten++
			}
		}
	}
	if ctx.Err() != nil {
		return
	}
	slog.Info("dispatch: document compaction sweep",
		"candidates", len(rooms), "compacted", compacted, "rows_deleted", deleted, "left_to_queue", left,
		"forgotten", forgotten)
}

// compactionCandidates lists every document doc_compaction_owed names: the documents an append
// gave a row after their first that no compaction has folded since (appendUpdateTxClass,
// writeCompaction). It reads that table alone, so the pass reads no row of a document with nothing
// to fold, and it drains the list before it returns.
func compactionCandidates(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `select artifact_id::text from doc_compaction_owed`)
	if err != nil {
		return nil, fmt.Errorf("list the documents to compact: %w", err)
	}
	rooms, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list the documents to compact: %w", err)
	}
	return rooms, nil
}

// warnCompactionForget is the message a failed forgetCompactionCandidate logs.
const warnCompactionForget = "dispatch: take a document with nothing to fold off the compaction sweep's list"

// forgetCompactionCandidate takes room off the sweep's list when it holds no row after its first,
// and reports whether it did. It deletes in one transaction on pool under the document's lock
// (lockDocumentRoom), which every append that names a document takes too, so an append committing
// meanwhile either committed before the delete reads the rows, which then keeps the entry, or
// names the document again after it.
func forgetCompactionCandidate(ctx context.Context, pool *pgxpool.Pool, room string) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin forget the document's compaction entry: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := lockDocumentRoom(ctx, tx, room); err != nil {
		return false, err
	}
	result, err := tx.Exec(ctx, `
		delete from doc_compaction_owed where artifact_id = $1
			and not exists (select 1 from doc_updates where artifact_id = $1 offset 1)
	`, room)
	if err != nil {
		return false, fmt.Errorf("forget the document's compaction entry: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit forget the document's compaction entry: %w", err)
	}
	return result.RowsAffected() > 0, nil
}
