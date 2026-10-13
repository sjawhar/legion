package delivery

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Change is one delivery_changes row (migration 0100): what changed, its run's kind when it is a
// run or a job, and the time that decides which windows it can touch, before and after (a pull
// request's merged_at; a run's later head commit or start). The row triggers write one only when a
// row's content changed, so a reconcile pass that finds nothing new logs none.
type Change struct {
	Seq     int64
	At      time.Time
	Kind    string // "pull_request" | "run" | "run_job" | "session_group" | "settings"
	RunKind string // "deploy" | "pr_checks" | "" for the rest
	Repo    string
	Ref     string
	TOld    *time.Time
	TNew    *time.Time
}

// ChangeRetention is how long delivery_changes keeps a row: a page away longer reads its window once.
const ChangeRetention = 24 * time.Hour

// ChangesSince reads up to limit changes after seq, oldest first. seq is allocated in commit order
// (each trigger takes one advisory lock before it inserts), so a reader that has read through seq
// never meets a later commit below it.
func ChangesSince(ctx context.Context, q Querier, seq int64, limit int) ([]Change, error) {
	rows, err := q.Query(ctx, `
		select seq, at, kind, coalesce(run_kind, ''), coalesce(repo, ''), coalesce(ref, ''), t_old, t_new
		from delivery_changes
		where seq > $1
		order by seq
		limit $2
	`, seq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changes := []Change{}
	for rows.Next() {
		var change Change
		if err := rows.Scan(&change.Seq, &change.At, &change.Kind, &change.RunKind, &change.Repo, &change.Ref, &change.TOld, &change.TNew); err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, rows.Err()
}

// LatestChangeSeq is the newest change's seq, or 0 when the log is empty.
func LatestChangeSeq(ctx context.Context, q Querier) (int64, error) {
	var seq int64
	err := q.QueryRow(ctx, `select coalesce(max(seq), 0) from delivery_changes`).Scan(&seq)
	return seq, err
}

// OldestChangeSeq is the oldest kept change's seq, or 0 when the log is empty: a reader whose
// cursor is below it has missed changes PruneChanges removed.
func OldestChangeSeq(ctx context.Context, q Querier) (int64, error) {
	var seq int64
	err := q.QueryRow(ctx, `select coalesce(min(seq), 0) from delivery_changes`).Scan(&seq)
	return seq, err
}

// ChangeAt is when the change at seq was recorded, or nil when no row holds seq (pruned, or the log
// was reset): a reader compares it with the time it read for its cursor to tell a reset log.
func ChangeAt(ctx context.Context, q Querier, seq int64) (*time.Time, error) {
	var at time.Time
	err := q.QueryRow(ctx, `select at from delivery_changes where seq = $1`, seq).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &at, nil
}

// PruneChanges deletes every change recorded more than ChangeRetention before now, and answers how
// many it deleted.
func PruneChanges(ctx context.Context, pool *store.Pool, now time.Time) (int64, error) {
	tag, err := pool.Exec(ctx, `delete from delivery_changes where at < $1`, now.Add(-ChangeRetention))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
