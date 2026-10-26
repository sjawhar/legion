package refs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Pool is the part of the Dispatch connection pool a rebuild uses.
type Pool interface {
	Queryer
	Begin(ctx context.Context) (pgx.Tx, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Rebuild counts what RebuildAll reconciled.
type Rebuild struct {
	Documents int
	Asks      int
	Comments  int
	Messages  int
	// Orphans is the number of edges deleted because no live source writes them.
	Orphans int
	// Edges is the number of mention edges after the rebuild.
	Edges int
}

// RebuildAll reparses every reference source and reconciles refs with it, one transaction per
// source, so the index converges on what the live write paths produce: the latest version of
// every document, every ask question, every comment body, and every issue message (issue-less
// messages are never indexed), each read as LoadSource reads it. Each transaction first deletes
// the source's refs_owed row (migration 0120), so the drain does not re-index it again, and then
// reads the source, so a write that lands after the delete waits on it and stays owed. An edge
// that survives keeps its created_at and source_seq; one the rebuild introduces takes the source's
// own write time as created_at (Reindex) and has no source_seq, since no event introduced it, and
// no current excerpt: a document mention's excerpt is cut from the document's tree, which only the
// document service reads, so its reconciliation (docs.Service.RunReferenceExcerptReconciliation)
// stores it. An edge that survives keeps its payload, but StoredExcerpts hides it until the row has
// a current excerpt_version.
func RebuildAll(ctx context.Context, pool Pool, serverURL string) (Rebuild, error) {
	var report Rebuild
	sources := []struct {
		kind  string
		query string
		count *int
	}{
		{"artifact", `
			select a.id::text from artifacts a
			where exists (select 1 from artifact_versions v where v.artifact_id = a.id and v.markdown is not null)
			order by a.id`, &report.Documents},
		{"ask", `select id::text from asks order by id`, &report.Asks},
		{"comment", `select id::text from comments order by id`, &report.Comments},
		{"message", `select id::text from messages where issue_key is not null order by id`, &report.Messages},
	}
	for _, source := range sources {
		ids, err := loadSourceIDs(ctx, pool, source.query)
		if err != nil {
			return report, fmt.Errorf("load %s sources: %w", source.kind, err)
		}
		for _, id := range ids {
			exists, err := rebuildSource(ctx, pool, source.kind, id, serverURL)
			if err != nil {
				return report, fmt.Errorf("rebuild %s %s: %w", source.kind, id, err)
			}
			if exists {
				*source.count++
			}
		}
	}
	orphans, err := pool.Exec(ctx, `
		delete from refs r
		where not exists (
			select 1 from artifacts a where r.from_kind = 'artifact' and a.id::text = r.from_id
			union all
			select 1 from asks k where r.from_kind = 'ask' and k.id::text = r.from_id
			union all
			select 1 from comments c where r.from_kind = 'comment' and c.id::text = r.from_id
			union all
			select 1 from messages m where r.from_kind = 'message' and m.issue_key is not null and m.id::text = r.from_id
		)
	`)
	if err != nil {
		return report, fmt.Errorf("delete orphan references: %w", err)
	}
	report.Orphans = int(orphans.RowsAffected())
	if err := pool.QueryRow(ctx, `select count(*) from refs`).Scan(&report.Edges); err != nil {
		return report, fmt.Errorf("count references: %w", err)
	}
	return report, nil
}

func loadSourceIDs(ctx context.Context, pool Pool, query string) ([]string, error) {
	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// rebuildSource re-indexes one source in a transaction of its own and reports whether it still
// exists.
func rebuildSource(ctx context.Context, pool Pool, kind, id, serverURL string) (bool, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `delete from refs_owed where kind = $1 and id = $2`, kind, id); err != nil {
		return false, fmt.Errorf("clear its owed re-index: %w", err)
	}
	source, err := LoadSource(ctx, tx, kind, id)
	if err != nil {
		return false, err
	}
	if err := Reindex(ctx, tx, source, serverURL, NewResolver(ctx, tx, kind, id)); err != nil {
		return false, err
	}
	return source.Exists, tx.Commit(ctx)
}
