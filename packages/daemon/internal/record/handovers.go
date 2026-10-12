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

func (s *Postgres) HandedOverRoot(ctx context.Context, tx pgx.Tx, issue string) (*HandedOverRoot, error) {
	root := HandedOverRoot{Issue: issue}
	var generation int64
	err := tx.QueryRow(ctx, `select generation, gate_carried from handed_over_roots where issue = $1`, issue).Scan(&generation, &root.GateCarried)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read handed-over root %s: %w", issue, err)
	}
	root.Generation = uint64(generation)
	return &root, nil
}

func (s *Postgres) PutHandedOverRoot(ctx context.Context, tx pgx.Tx, root HandedOverRoot) error {
	if root.Generation == 0 || root.Generation > maxInt64 {
		return fmt.Errorf("put handed-over root %s: generation %d is out of range", root.Issue, root.Generation)
	}
	if _, err := tx.Exec(ctx, `insert into handed_over_roots (issue, generation, gate_carried) values ($1, $2, $3)
		on conflict (issue) do update set generation = excluded.generation, gate_carried = excluded.gate_carried`,
		root.Issue, int64(root.Generation), root.GateCarried); err != nil {
		return fmt.Errorf("put handed-over root %s: %w", root.Issue, err)
	}
	return nil
}
