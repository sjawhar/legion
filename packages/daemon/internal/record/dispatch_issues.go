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

// DispatchIssueSeq is the sequence of the latest Dispatch event of key the mirror holds, and whether
// it holds one: every issue event the daemon applies writes its row first (blockers.Mirror).
func (s *Postgres) DispatchIssueSeq(ctx context.Context, tx pgx.Tx, key string) (int64, bool, error) {
	var seq int64
	err := tx.QueryRow(ctx, `select dispatch_seq from dispatch_issues where key = $1`, key).Scan(&seq)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("read the Dispatch mirror sequence of %s: %w", key, err)
	}
	return seq, true, nil
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
	mirrored, err := queryDispatchIssues(ctx, tx, `select i.key, i.project, i.status, i.blockers, i.dispatch_seq, coalesce(c.claimed_by, ''), coalesce(c.claim_seq, 0)
		from dispatch_issues i left join dispatch_claims c on c.key = i.key
		where i.project = $1 order by i.key`, project)
	if err != nil {
		return nil, fmt.Errorf("list the Dispatch mirror of %s: %w", project, err)
	}
	return mirrored, nil
}

// DispatchIssuesOf reads the keys a judgement names and every key their rows wait on in one
// statement: the set is built first, from the keys and the blockers of their rows, and the rows are
// then read by primary key, so the read never scans the project's mirror.
func (s *Postgres) DispatchIssuesOf(ctx context.Context, tx pgx.Tx, project string, keys []string) ([]DispatchIssueRow, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	mirrored, err := queryDispatchIssues(ctx, tx, `select i.key, i.project, i.status, i.blockers, i.dispatch_seq, coalesce(c.claimed_by, ''), coalesce(c.claim_seq, 0)
		from dispatch_issues i left join dispatch_claims c on c.key = i.key
		where i.project = $1 and i.key = any(array(
			select unnest($2::text[])
			union select unnest(judged.blockers) from dispatch_issues judged where judged.project = $1 and judged.key = any($2)))
		order by i.key`, project, keys)
	if err != nil {
		return nil, fmt.Errorf("read the Dispatch mirror of %v in %s: %w", keys, project, err)
	}
	return mirrored, nil
}

// queryDispatchIssues is every mirrored row query selects with args, in its order.
func queryDispatchIssues(ctx context.Context, tx pgx.Tx, query string, args ...any) ([]DispatchIssueRow, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var mirrored []DispatchIssueRow
	for rows.Next() {
		var row DispatchIssueRow
		if err := rows.Scan(&row.Key, &row.Project, &row.Status, &row.Blockers, &row.Seq, &row.ClaimedBy, &row.ClaimSeq); err != nil {
			return nil, err
		}
		mirrored = append(mirrored, row)
	}
	return mirrored, rows.Err()
}
