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
// primary document, the task items tree holds (pmdoc.CountTasks) - tasks_total every task item,
// tasks_done every checked one - and tasks_version, the number of the document's latest version,
// which the caller has written already; so the issue read and list answer progress without parsing
// a document, and a count that names an older version than the document's latest is one the
// reconciliation (RunTaskProgressReconciliation) takes again (LEGION-542). It runs in the
// transaction that writes the version, so the counts and the version commit together; a project
// document or a non-primary artifact owns no issue row and records nothing. The caller holds the
// issue's owner row already (lockArtifactOwner, requireOpenOwner), so this update waits on nothing.
func RecordTaskProgress(ctx context.Context, tx pgx.Tx, artifactID string, tree *pmdoc.Node) error {
	progress := pmdoc.CountTasks(tree)
	if _, err := tx.Exec(ctx, `
		update issues i
		set tasks_done = $2, tasks_total = $3,
		    tasks_version = (select coalesce(max(number), 0) from artifact_versions where artifact_id = $1)
		where i.key = (select issue_key from artifacts where id = $1 and is_primary)
	`, artifactID, progress.Done, progress.Total); err != nil {
		return fmt.Errorf("record issue task progress: %w", err)
	}
	return nil
}

// RecordTaskProgressMarkdown is RecordTaskProgress for a caller that holds the canonical markdown
// it has just stored as the document's version rather than the tree: issue creation and an upload
// insert their version row themselves, after the document write, and record the count once the
// row exists so tasks_version names it. The markdown is the server's own rendering, so it reads
// back (pmdoc.ParseRendering); one that does not is the write's error.
func RecordTaskProgressMarkdown(ctx context.Context, tx pgx.Tx, artifactID, markdown string) error {
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		return fmt.Errorf("count the stored document's task items: %w", err)
	}
	return RecordTaskProgress(ctx, tx, artifactID, tree)
}

// latestPrimaryVersion is the number of the latest version of issue i's primary document, in a
// query whose `from issues i` is in scope: the greatest artifact_versions.number of that document,
// or 0 for an issue whose document has no version row, which a count taken from no version
// matches. The reconciliation's drift predicate and its batch both read it, so the two never
// disagree on which version a count names.
const latestPrimaryVersion = `(
	select coalesce(max(v.number), 0)
	from artifacts a join artifact_versions v on v.artifact_id = a.id
	where a.issue_key = i.key and a.is_primary
)`

// taskProgressDrift is the issues whose stored task count is not the count of their primary
// document's latest version: never counted (every row the migration that added these columns
// found), versioned by a server that wrote no count (the task a deploy replaces, until it stops),
// or whose latest version was written while the row was locked by another writer. The row's own
// lock is taken only by the batch that writes it, so the predicate reads nothing it would wait on.
const taskProgressDrift = `i.tasks_version is distinct from ` + latestPrimaryVersion

// taskProgressBatch is how many issues one reconciliation transaction counts; between batches the
// pool is free for requests.
const taskProgressBatch = 50

// TaskProgressReconcileInterval is how often the server reconciles the task counts after the pass
// it runs at start. One pass after start would miss what a deploy's overlap leaves: the task being
// replaced still writes versions through code that records no count until it stops, and a row
// another writer holds during the pass is skipped (skip locked), so the count a reader sees is
// stale for at most this long.
const TaskProgressReconcileInterval = 5 * time.Minute

// RunTaskProgressReconciliation counts the primary document of every issue whose stored task count
// is not its latest version's (taskProgressDrift), in batches, at start and every
// TaskProgressReconcileInterval until ctx ends, so an issue nobody has edited since the deploy
// shows its progress, an issue the old task versioned during the deploy is counted again, and a
// null `tasks` on the API means the spec holds no task list for longer than that interval only
// while a row stays locked. Each pass runs until no drift it can lock remains; a batch that fails,
// or that was short because rows were locked, is retried after a short wait, a bounded number of
// times, so one bad batch ends the pass and not the loop. Each issue's latest version markdown is
// parsed as the stored rendering it is (pmdoc.ParseRendering); an issue whose spec does not parse
// is logged and counted as holding none, so one broken document does not hold the rest back.
func (s *Service) RunTaskProgressReconciliation(ctx context.Context) {
	for {
		s.ReconcileTaskProgress(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(TaskProgressReconcileInterval):
		}
	}
}

// taskProgressPassRetries is how many failed or short batches one reconciliation pass retries
// before leaving the rest to the next pass, and taskProgressRetryBackoffStep the wait before the
// first retry, multiplied by the retry's number.
const (
	taskProgressPassRetries      = 5
	taskProgressRetryBackoffStep = 200 * time.Millisecond
)

// ReconcileTaskProgress runs one reconciliation pass: batches until no drift it can lock remains.
// A full batch with drift left goes straight to the next batch, as a backfill of more than one
// batch does. A batch that errs, or that was short because the rows left were locked, is retried
// after a backoff, up to taskProgressPassRetries times in the pass; drift still left is the next
// pass's.
func (s *Service) ReconcileTaskProgress(ctx context.Context) {
	ctx = store.WithTransactionTracking(ctx)
	counted, retries := 0, 0
	for ctx.Err() == nil {
		n, remaining, err := s.reconcileTaskProgressBatch(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("dispatch: reconcile issue task progress", "error", err)
		}
		counted += n
		if err == nil && !remaining {
			break
		}
		if err == nil && n == taskProgressBatch {
			continue
		}
		retries++
		if retries > taskProgressPassRetries {
			slog.Warn("dispatch: issue task progress reconciliation left drift to the next pass",
				"counted", counted)
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(retries) * taskProgressRetryBackoffStep):
		}
	}
	if counted > 0 {
		slog.Info("dispatch: reconciled issue task progress", "issues", counted)
	}
}

// reconcileTaskProgressBatch counts one batch of drifted issues in one transaction and returns how
// many it counted and whether drift remained past them. Each issue row is locked `for no key
// update skip locked` - the level every owner-row writer takes (lockArtifactOwner), enough for
// three non-key columns - so a concurrent writer holding the row is left alone: a version write
// records its own count, and any other writer leaves the row's drift to a later batch.
func (s *Service) reconcileTaskProgressBatch(
	ctx context.Context,
) (counted int, remaining bool, err error) {
	tx, err := s.store.Pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("begin task progress reconciliation: %w", err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		select i.key,
		       coalesce((
		         select v.markdown from artifacts a join artifact_versions v on v.artifact_id = a.id
		         where a.issue_key = i.key and a.is_primary and v.markdown is not null
		         order by v.number desc limit 1
		       ), ''),
		       `+latestPrimaryVersion+`
		from issues i
		where `+taskProgressDrift+`
		order by i.key
		limit $1
		for no key update of i skip locked
	`, taskProgressBatch)
	if err != nil {
		return 0, false, fmt.Errorf("list drifted issue task counts: %w", err)
	}
	type drifted struct {
		key      string
		markdown string
		version  int
	}
	var batch []drifted
	for rows.Next() {
		var issue drifted
		if err := rows.Scan(&issue.key, &issue.markdown, &issue.version); err != nil {
			rows.Close()
			return 0, false, fmt.Errorf("scan drifted issue task count: %w", err)
		}
		batch = append(batch, issue)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("list drifted issue task counts: %w", err)
	}
	for _, issue := range batch {
		var progress pmdoc.TaskProgress
		if issue.markdown != "" {
			tree, err := pmdoc.ParseRendering(issue.markdown)
			if err != nil {
				slog.Warn("dispatch: issue task progress reconciliation cannot parse the spec; counting none",
					"issue", issue.key, "error", err)
			} else {
				progress = pmdoc.CountTasks(tree)
			}
		}
		if _, err := tx.Exec(ctx, `
			update issues set tasks_done = $2, tasks_total = $3, tasks_version = $4 where key = $1
		`, issue.key, progress.Done, progress.Total, issue.version); err != nil {
			return 0, false, fmt.Errorf("record issue %s task progress: %w", issue.key, err)
		}
	}
	// Drift past this batch - rows the limit left, or rows another writer holds - is still there
	// once it commits; the pass asks again. Read before the commit so the rows this batch wrote are
	// not counted as drift.
	if err := tx.QueryRow(ctx, `
		select exists(select 1 from issues i where `+taskProgressDrift+` and i.key <> all($1::text[]))
	`, keysOf(batch, func(issue drifted) string { return issue.key })).Scan(&remaining); err != nil {
		return 0, false, fmt.Errorf("check remaining task count drift: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return 0, false, err
		}
		return 0, false, fmt.Errorf("commit task progress reconciliation: %w", err)
	}
	return len(batch), remaining, nil
}

func keysOf[T any](items []T, key func(T) string) []string {
	keys := make([]string, len(items))
	for index, item := range items {
		keys[index] = key(item)
	}
	return keys
}
