package record

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func (s *Postgres) Handover(ctx context.Context, tx pgx.Tx, issue string) (*Handover, error) {
	var handover Handover
	err := tx.QueryRow(ctx, `select issue, tree, linger_generation, cancelled from handovers where issue = $1`, issue).Scan(
		&handover.Issue, &handover.Tree, &handover.Generation, &handover.Cancelled,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read hand-over of %s: %w", issue, err)
	}
	return &handover, nil
}

func (s *Postgres) PutHandover(ctx context.Context, tx pgx.Tx, handover Handover) error {
	if handover.Issue == "" || handover.Tree == "" || handover.Generation == 0 {
		return errors.New("hand-over requires issue, tree and linger generation")
	}
	if handover.Generation > maxInt64 {
		return fmt.Errorf("put hand-over of %s: generation %d does not fit a bigint", handover.Issue, handover.Generation)
	}
	_, err := tx.Exec(ctx, `insert into handovers (issue, tree, linger_generation, cancelled) values ($1, $2, $3, false)
		on conflict (issue) do update set tree = excluded.tree, linger_generation = excluded.linger_generation, cancelled = false`,
		handover.Issue, handover.Tree, int64(handover.Generation))
	if err != nil {
		return fmt.Errorf("put hand-over of %s: %w", handover.Issue, err)
	}
	return nil
}

func (s *Postgres) DeleteHandover(ctx context.Context, tx pgx.Tx, issue string) error {
	if _, err := tx.Exec(ctx, `delete from handovers where issue = $1`, issue); err != nil {
		return fmt.Errorf("delete hand-over of %s: %w", issue, err)
	}
	return nil
}

func (s *Postgres) DeleteHandoverAt(ctx context.Context, tx pgx.Tx, issue, tree string, generation uint64) error {
	if generation > maxInt64 {
		return fmt.Errorf("delete hand-over of %s: generation %d does not fit a bigint", issue, generation)
	}
	if _, err := tx.Exec(ctx, `delete from handovers where issue = $1 and tree = $2 and linger_generation = $3`,
		issue, tree, int64(generation)); err != nil {
		return fmt.Errorf("delete hand-over of %s: %w", issue, err)
	}
	return nil
}

func (s *Postgres) CancelTreeHandovers(ctx context.Context, tx pgx.Tx, tree string, generation uint64) error {
	if generation > maxInt64 {
		return fmt.Errorf("cancel hand-overs of %s: generation %d does not fit a bigint", tree, generation)
	}
	if _, err := tx.Exec(ctx, `update handovers set cancelled = true where tree = $1 and linger_generation = $2`, tree, int64(generation)); err != nil {
		return fmt.Errorf("cancel hand-overs of %s: %w", tree, err)
	}
	return nil
}

func (s *Postgres) CarriedRoot(ctx context.Context, tx pgx.Tx, issue string) (uint64, error) {
	var generation int64
	err := tx.QueryRow(ctx, `select generation from carried_roots where issue = $1`, issue).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read carried root %s: %w", issue, err)
	}
	return uint64(generation), nil
}

func (s *Postgres) PutCarriedRoot(ctx context.Context, tx pgx.Tx, issue string, generation uint64) error {
	if generation == 0 || generation > maxInt64 {
		return fmt.Errorf("put carried root %s: generation %d is out of range", issue, generation)
	}
	if _, err := tx.Exec(ctx, `insert into carried_roots (issue, generation) values ($1, $2)
		on conflict (issue) do update set generation = excluded.generation`, issue, int64(generation)); err != nil {
		return fmt.Errorf("put carried root %s: %w", issue, err)
	}
	return nil
}
