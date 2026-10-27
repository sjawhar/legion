package record

import (
	"context"
	"errors"
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
	return s.PutDispatchClaim(ctx, tx, row.Key, row.ClaimedBy, row.ClaimSeq)
}

func (s *Postgres) PutDispatchClaim(ctx context.Context, tx pgx.Tx, key, holder string, seq int64) error {
	if seq <= 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `insert into dispatch_claims (key, claimed_by, claim_seq) values ($1, $2, $3)
		on conflict (key) do update set claimed_by = excluded.claimed_by, claim_seq = excluded.claim_seq
		where dispatch_claims.claim_seq < excluded.claim_seq`,
		key, holder, seq); err != nil {
		return fmt.Errorf("put the Dispatch claim of %s: %w", key, err)
	}
	return nil
}

func (s *Postgres) DispatchClaim(ctx context.Context, tx pgx.Tx, key string) (string, error) {
	var holder string
	err := tx.QueryRow(ctx, `select claimed_by from dispatch_claims where key = $1`, key).Scan(&holder)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the Dispatch claim of %s: %w", key, err)
	}
	return holder, nil
}

func (s *Postgres) DispatchIssues(ctx context.Context, tx pgx.Tx, project string) ([]DispatchIssueRow, error) {
	rows, err := tx.Query(ctx, `select i.key, i.project, i.status, i.blockers, i.dispatch_seq, coalesce(c.claimed_by, ''), coalesce(c.claim_seq, 0)
		from dispatch_issues i left join dispatch_claims c on c.key = i.key
		where i.project = $1 order by i.key`, project)
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
