package refs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

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
// messages are never indexed). An edge that survives keeps its created_at and source_seq; one
// the rebuild introduces has no source_seq, since no event introduced it.
func RebuildAll(ctx context.Context, pool *pgxpool.Pool, serverURL string) (Rebuild, error) {
	var report Rebuild
	sources := []struct {
		kind  string
		query string
		count *int
	}{
		{"artifact", `
			select a.id::text, v.markdown
			from artifacts a
			join lateral (
				select markdown from artifact_versions
				where artifact_id = a.id and markdown is not null
				order by number desc limit 1
			) v on true
			order by a.id`, &report.Documents},
		{"ask", `select id::text, question from asks order by id`, &report.Asks},
		{"comment", `select id::text, body from comments order by id`, &report.Comments},
		{"message", `select id::text, body from messages where issue_key is not null order by id`, &report.Messages},
	}
	for _, source := range sources {
		rows, err := loadSources(ctx, pool, source.query)
		if err != nil {
			return report, fmt.Errorf("load %s sources: %w", source.kind, err)
		}
		for _, row := range rows {
			if err := replaceInTx(ctx, pool, source.kind, row[0], row[1], serverURL); err != nil {
				return report, fmt.Errorf("rebuild %s %s: %w", source.kind, row[0], err)
			}
			*source.count++
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

func loadSources(ctx context.Context, pool *pgxpool.Pool, query string) ([][2]string, error) {
	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sources := [][2]string{}
	for rows.Next() {
		var source [2]string
		if err := rows.Scan(&source[0], &source[1]); err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	return sources, rows.Err()
}

func replaceInTx(ctx context.Context, pool *pgxpool.Pool, fromKind, fromID, body, serverURL string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := replace(ctx, tx, fromKind, fromID, body, serverURL); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
