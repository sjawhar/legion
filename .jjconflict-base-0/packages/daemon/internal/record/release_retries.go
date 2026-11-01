package record

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (s *Postgres) ReleaseRetry(ctx context.Context, tx pgx.Tx, issue string) (*ReleaseRetry, error) {
	var retry ReleaseRetry
	err := tx.QueryRow(ctx, `select issue, dispatch_seq, attempts, next_at, exhausted from child_release_retries where issue = $1`, issue).Scan(
		&retry.Issue, &retry.Sequence, &retry.Attempts, &retry.NextAt, &retry.Exhausted,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read child release retry of %s: %w", issue, err)
	}
	return &retry, nil
}

func (s *Postgres) PutReleaseRetry(ctx context.Context, tx pgx.Tx, retry ReleaseRetry) error {
	if retry.Issue == "" || retry.Sequence <= 0 || retry.Attempts <= 0 || retry.NextAt.IsZero() {
		return errors.New("child release retry requires issue, sequence, attempts and next at")
	}
	_, err := tx.Exec(ctx, `insert into child_release_retries (issue, dispatch_seq, attempts, next_at, exhausted)
		values ($1, $2, $3, $4, $5)
		on conflict (issue) do update set dispatch_seq = excluded.dispatch_seq, attempts = excluded.attempts, next_at = excluded.next_at, exhausted = excluded.exhausted`,
		retry.Issue, retry.Sequence, retry.Attempts, retry.NextAt, retry.Exhausted)
	if err != nil {
		return fmt.Errorf("put child release retry of %s: %w", retry.Issue, err)
	}
	return nil
}

func (s *Postgres) DeleteReleaseRetry(ctx context.Context, tx pgx.Tx, issue string) error {
	if _, err := tx.Exec(ctx, `delete from child_release_retries where issue = $1`, issue); err != nil {
		return fmt.Errorf("delete child release retry of %s: %w", issue, err)
	}
	return nil
}
