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
	// Two fences in one row: the status, project and blockers move only for an observation newer
	// than dispatch_seq, and the claim only for one carrying a claim newer than claim_seq (a row
	// carrying none, claim_seq 0, never passes it). Each column is chosen by its own fence, and
	// the update runs when either passes.
	_, err := tx.Exec(ctx, `insert into dispatch_issues (key, project, status, blockers, dispatch_seq, claimed_by, claim_seq)
		values ($1, $2, $3, $4, $5, $6, $7)
		on conflict (key) do update set
			project = case when dispatch_issues.dispatch_seq < excluded.dispatch_seq then excluded.project else dispatch_issues.project end,
			status = case when dispatch_issues.dispatch_seq < excluded.dispatch_seq then excluded.status else dispatch_issues.status end,
			blockers = case when dispatch_issues.dispatch_seq < excluded.dispatch_seq then excluded.blockers else dispatch_issues.blockers end,
			dispatch_seq = greatest(dispatch_issues.dispatch_seq, excluded.dispatch_seq),
			claimed_by = case when dispatch_issues.claim_seq < excluded.claim_seq then excluded.claimed_by else dispatch_issues.claimed_by end,
			claim_seq = greatest(dispatch_issues.claim_seq, excluded.claim_seq)
		where dispatch_issues.dispatch_seq < excluded.dispatch_seq or dispatch_issues.claim_seq < excluded.claim_seq`,
		row.Key, row.Project, row.Status, blockers, row.Seq, row.ClaimedBy, row.ClaimSeq)
	if err != nil {
		return fmt.Errorf("put the Dispatch mirror of %s: %w", row.Key, err)
	}
	return nil
}

func (s *Postgres) PutDispatchClaim(ctx context.Context, tx pgx.Tx, key, holder string, seq int64) error {
	if seq <= 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `update dispatch_issues set claimed_by = $2, claim_seq = $3 where key = $1 and claim_seq < $3`,
		key, holder, seq); err != nil {
		return fmt.Errorf("put the Dispatch claim of %s: %w", key, err)
	}
	return nil
}

func (s *Postgres) DispatchIssues(ctx context.Context, tx pgx.Tx, project string) ([]DispatchIssueRow, error) {
	rows, err := tx.Query(ctx, `select key, project, status, blockers, dispatch_seq, claimed_by, claim_seq from dispatch_issues
		where project = $1 order by key`, project)
	if err != nil {
		return nil, fmt.Errorf("list the Dispatch mirror of %s: %w", project, err)
	}
	defer rows.Close()
	var mirrored []DispatchIssueRow
	for rows.Next() {
		var row DispatchIssueRow
		if err := rows.Scan(&row.Key, &row.Project, &row.Status, &row.Blockers, &row.Seq, &row.ClaimedBy, &row.ClaimSeq); err != nil {
			return nil, fmt.Errorf("scan the Dispatch mirror of %s: %w", project, err)
		}
		mirrored = append(mirrored, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the Dispatch mirror of %s: %w", project, err)
	}
	return mirrored, nil
}
