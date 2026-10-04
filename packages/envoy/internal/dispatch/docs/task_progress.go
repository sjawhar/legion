package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// RecordTaskProgress stores on the issue owning artifactID, when that artifact is the issue's
// primary document, the task items tree holds (pmdoc.CountTasks): tasks_total every task item and
// tasks_done every checked one, so the issue read and list answer progress without parsing a
// document (LEGION-542). It runs in the transaction that writes the document's version, so the
// counts and the version commit together; a project document or a non-primary artifact owns no
// issue row and records nothing. The caller holds the issue's owner row already (lockArtifactOwner,
// requireOpenOwner), so this update waits on nothing.
func RecordTaskProgress(ctx context.Context, tx pgx.Tx, artifactID string, tree *pmdoc.Node) error {
	progress := pmdoc.CountTasks(tree)
	if _, err := tx.Exec(ctx, `
		update issues set tasks_done = $2, tasks_total = $3
		where key = (select issue_key from artifacts where id = $1 and is_primary)
	`, artifactID, progress.Done, progress.Total); err != nil {
		return fmt.Errorf("record issue task progress: %w", err)
	}
	return nil
}

// taskProgressBackfillBatch is how many issues one backfill transaction counts; between batches the
// pool is free for requests.
const taskProgressBackfillBatch = 50

// RunTaskProgressBackfill counts the primary document of every issue whose task counts were never
// recorded (both columns null, as migration 0074 leaves every row) and stores them, in batches, so
// an issue nobody has edited since the deploy shows its progress too and a null `tasks` on the API
// always means the spec holds no task list. Each issue's latest version markdown is parsed as the
// stored rendering it is (pmdoc.ParseRendering); an issue whose spec does not parse is logged and
// counted as holding none, so one broken document does not hold the rest back. It ends when no
// uncounted issue remains or ctx ends; a later start finds nothing to do.
func (s *Service) RunTaskProgressBackfill(ctx context.Context) {
	ctx = store.WithTransactionTracking(ctx)
	counted := 0
	for ctx.Err() == nil {
		n, err := s.backfillTaskProgressBatch(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("dispatch: backfill issue task progress", "error", err)
			}
			return
		}
		counted += n
		if n < taskProgressBackfillBatch {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	if counted > 0 {
		slog.Info("dispatch: backfilled issue task progress", "issues", counted)
	}
}

// backfillTaskProgressBatch counts one batch of uncounted issues in one transaction and returns
// how many it counted. Each issue row is locked `for no key update skip locked` - the level every
// owner-row writer takes (lockArtifactOwner), enough for two non-key columns - so a concurrent
// version write, which holds the owner row and writes the same columns, is left to record its own
// count.
func (s *Service) backfillTaskProgressBatch(ctx context.Context) (int, error) {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin task progress backfill: %w", err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		select i.key, coalesce((
			select v.markdown from artifacts a join artifact_versions v on v.artifact_id = a.id
			where a.issue_key = i.key and a.is_primary and v.markdown is not null
			order by v.number desc limit 1
		), '')
		from issues i
		where i.tasks_total is null
		order by i.key
		limit $1
		for no key update of i skip locked
	`, taskProgressBackfillBatch)
	if err != nil {
		return 0, fmt.Errorf("list uncounted issues: %w", err)
	}
	type uncounted struct {
		key      string
		markdown string
	}
	var batch []uncounted
	for rows.Next() {
		var issue uncounted
		if err := rows.Scan(&issue.key, &issue.markdown); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan uncounted issue: %w", err)
		}
		batch = append(batch, issue)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("list uncounted issues: %w", err)
	}
	for _, issue := range batch {
		var progress pmdoc.TaskProgress
		if issue.markdown != "" {
			tree, err := pmdoc.ParseRendering(issue.markdown)
			if err != nil {
				slog.Warn("dispatch: issue task progress backfill cannot parse the spec; counting none", "issue", issue.key, "error", err)
			} else {
				progress = pmdoc.CountTasks(tree)
			}
		}
		if _, err := tx.Exec(ctx, `
			update issues set tasks_done = $2, tasks_total = $3 where key = $1
		`, issue.key, progress.Done, progress.Total); err != nil {
			return 0, fmt.Errorf("record issue %s task progress: %w", issue.key, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return 0, err
		}
		return 0, fmt.Errorf("commit task progress backfill: %w", err)
	}
	return len(batch), nil
}
