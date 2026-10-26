package record

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (s *Postgres) PutDispatchIssue(ctx context.Context, tx pgx.Tx, row DispatchIssueRow) error {
	// A zero sequence is no observation of Dispatch's at all: it is a fact the daemon minted from
	// its own record, and the insert of a brand-new key has no conflict for the fence below to
	// catch, so it would land as the row of record and then refuse every real observation that
	// Dispatch has already made. Such a fact writes nothing, and the next event, the periodic turn
	// or the next boot writes the row Dispatch means.
	if row.Seq == 0 {
		return nil
	}
	blockers := row.Blockers
	if blockers == nil {
		blockers = []string{}
	}
	_, err := tx.Exec(ctx, `insert into dispatch_issues (key, project, status, blockers, dispatch_seq)
		values ($1, $2, $3, $4, $5)
		on conflict (key) do update set project = excluded.project, status = excluded.status,
			blockers = excluded.blockers, dispatch_seq = excluded.dispatch_seq
		where dispatch_issues.dispatch_seq < excluded.dispatch_seq`,
		row.Key, row.Project, row.Status, blockers, row.Seq)
	if err != nil {
		return fmt.Errorf("put the Dispatch mirror of %s: %w", row.Key, err)
	}
	return nil
}

func (s *Postgres) DispatchIssues(ctx context.Context, tx pgx.Tx, project string) ([]DispatchIssueRow, error) {
	rows, err := tx.Query(ctx, `select key, project, status, blockers, dispatch_seq from dispatch_issues
		where project = $1 order by key`, project)
	if err != nil {
		return nil, fmt.Errorf("list the Dispatch mirror of %s: %w", project, err)
	}
	defer rows.Close()
	var mirrored []DispatchIssueRow
	for rows.Next() {
		var row DispatchIssueRow
		if err := rows.Scan(&row.Key, &row.Project, &row.Status, &row.Blockers, &row.Seq); err != nil {
			return nil, fmt.Errorf("scan the Dispatch mirror of %s: %w", project, err)
		}
		mirrored = append(mirrored, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the Dispatch mirror of %s: %w", project, err)
	}
	return mirrored, nil
}
