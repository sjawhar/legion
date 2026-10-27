package docs

// The compaction sweep: the hourly pass over every document with rows to fold, which compacts what
// the compaction queue never saw or let go - the rooms a restart, a Shutdown or a Quiesce dropped
// from it, and the documents another process wrote and never loaded here.

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// compactionSweepLease names the lease a sweep pass runs under, and compactionSweepPeriod the
// period one pass serves (duty.Exclusive): a pass reads every document's row count, so two
// envoy-dispatch processes sweeping, or one process sweeping again after a restart, inside one
// aligned hour is that whole read again for what the queue already compacts.
const (
	compactionSweepLease  = "document-compaction"
	compactionSweepPeriod = time.Hour
)

// compactionSweepFirstTick is the span the sweep's first tick is drawn from, uniformly, after it
// starts: soon enough that a document a restart dropped from the queue folds within its
// compactMaxTailAge and an hour, and spread so the processes a deploy starts together do not all
// read the database at one moment. compactionSweepInterval is the wait between later ticks, give or
// take compactionSweepJitter: six ticks an aligned hour, so a process that boots or takes the
// lease late in the hour still makes the hour's pass when the process that held it went away.
const (
	compactionSweepFirstTick = 30 * time.Second
	compactionSweepInterval  = 10 * time.Minute
	compactionSweepJitter    = time.Minute
)

// RunCompactionSweep compacts, once an aligned hour across every process that serves the database
// and across restarts (duty.Exclusive, compactionSweepLease), every document due a compaction by
// the queue's own rule (checkCompaction): a row after its first older than compactMaxTailAge, or
// compactMinRows of them older than compactTail. It runs until ctx ends. Its first tick comes within
// compactionSweepFirstTick of its start, then one every compactionSweepInterval, jittered; a tick
// in an hour some process has already swept skips. It runs whether or not NATS is configured: the
// compaction is what keeps text a write deleted out of doc_updates and its backups, and it needs
// nothing but the database. leases is required, as it is of every leased loop.
func (s *Service) RunCompactionSweep(ctx context.Context, leases duty.Leases) {
	ctx = store.WithTransactionTracking(ctx)
	// #nosec G404 — jitter, not a secret.
	wait := time.Duration(rand.Int64N(int64(compactionSweepFirstTick)))
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		s.compactionSweepTick(ctx, leases)
		// #nosec G404 — jitter, not a secret.
		wait = compactionSweepInterval - compactionSweepJitter + time.Duration(rand.Int64N(int64(2*compactionSweepJitter)))
	}
}

// compactionSweepTick is one tick of the sweep: the pass, when this process takes the lease and no
// process has swept this hour.
func (s *Service) compactionSweepTick(ctx context.Context, leases duty.Leases) {
	duty.Exclusive(ctx, leases, compactionSweepLease, compactionSweepPeriod, s.compactionSweepPass)
}

// compactionSweepPass lists the documents holding more than one row and compacts each one due
// (checkCompaction), under the tail rule and without waiting for its lock, one at a time on the
// compaction pool. The list is read whole before the first compaction, so no cursor holds a
// connection while one runs. A compaction that found the room's lock held or another writer
// changing its rows (compactBusy, compactDropped), or that failed, is handed to this service's
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
	compacted, deleted, left := 0, 0, 0
	for _, room := range rooms {
		if ctx.Err() != nil {
			return
		}
		check, err := s.checkCompaction(ctx, room)
		if err != nil {
			warnCompaction(ctx, "dispatch: check whether a document is due a compaction", room, err)
			s.compactions.note(room)
			left++
			continue
		}
		if !check.due {
			continue
		}
		result, err := s.persistence.compactRoom(ctx, room, tailRule, false)
		switch {
		case err != nil:
			warnCompaction(ctx, "dispatch: compact a document", room, err)
			s.compactions.note(room)
			left++
		case result.outcome == compactCompacted:
			compacted++
			deleted += result.deleted
		case result.outcome == compactBusy || result.outcome == compactDropped:
			s.compactions.note(room)
			left++
		}
	}
	if ctx.Err() != nil {
		return
	}
	slog.Info("dispatch: document compaction sweep",
		"candidates", len(rooms), "compacted", compacted, "rows_deleted", deleted, "left_to_queue", left)
}

// compactionCandidates lists every document holding more than one row, the only ones a compaction
// can fold. It reads the (artifact_id, version) primary key alone, which Postgres can answer with
// an index-only scan, so the pass reads no row's bytes or created_at to choose them; and it drains
// the list before it returns.
func compactionCandidates(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `select artifact_id::text from doc_updates group by artifact_id having count(*) > 1`)
	if err != nil {
		return nil, fmt.Errorf("list the documents to compact: %w", err)
	}
	rooms, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list the documents to compact: %w", err)
	}
	return rooms, nil
}
