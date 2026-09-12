package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// AnchorBlockBackfill reports how many legacy anchor rows gained their stable block identity.
type AnchorBlockBackfill struct {
	Asks     int
	Comments int
	Skipped  int
}

// BackfillAnchorBlocks resolves legacy quote anchors that still have one matching passage.
// Rows with a missing or ambiguous quote stay unchanged, so the command is idempotent and never
// guesses a block for an old row.
func (s *Service) BackfillAnchorBlocks(ctx context.Context) (AnchorBlockBackfill, error) {
	var result AnchorBlockBackfill
	for _, target := range []struct {
		table string
		count *int
	}{
		{table: "asks", count: &result.Asks},
		{table: "comments", count: &result.Comments},
	} {
		updated, skipped, err := s.backfillAnchorBlocks(ctx, target.table)
		if err != nil {
			return AnchorBlockBackfill{}, err
		}
		*target.count = updated
		result.Skipped += skipped
	}
	return result, nil
}

func (s *Service) backfillAnchorBlocks(ctx context.Context, table string) (int, int, error) {
	rows, err := s.store.Pool.Query(ctx, fmt.Sprintf(`
		select id::text, anchor
		from %s
		where anchor is not null and nullif(anchor->>'block_id', '') is null
		order by id
	`, table))
	if err != nil {
		return 0, 0, fmt.Errorf("list %s anchor blocks: %w", table, err)
	}
	defer rows.Close()

	type row struct {
		id     string
		anchor model.Anchor
	}
	var candidates []row
	for rows.Next() {
		var candidate row
		var encoded []byte
		if err := rows.Scan(&candidate.id, &encoded); err != nil {
			return 0, 0, fmt.Errorf("scan %s anchor block: %w", table, err)
		}
		if err := json.Unmarshal(encoded, &candidate.anchor); err != nil {
			return 0, 0, fmt.Errorf("decode %s anchor block: %w", table, err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate %s anchor blocks: %w", table, err)
	}

	updated := 0
	skipped := 0
	for _, candidate := range candidates {
		blockID, err := s.BlockForQuote(ctx, candidate.anchor.ArtifactID, candidate.anchor.Quote)
		if errors.Is(err, pmdoc.ErrTargetNotFound) {
			skipped++
			continue
		}
		var ambiguous *pmdoc.ErrTargetAmbiguous
		if errors.As(err, &ambiguous) {
			skipped++
			continue
		}
		if err != nil {
			return 0, 0, fmt.Errorf("resolve %s anchor %s: %w", table, candidate.id, err)
		}
		if blockID == "" {
			skipped++
			continue
		}
		command, err := s.store.Pool.Exec(ctx, fmt.Sprintf(`
			update %s
			set anchor = jsonb_set(anchor, '{block_id}', to_jsonb($2::text), true)
			where id = $1 and nullif(anchor->>'block_id', '') is null
		`, table), candidate.id, blockID)
		if err != nil {
			return 0, 0, fmt.Errorf("update %s anchor %s: %w", table, candidate.id, err)
		}
		updated += int(command.RowsAffected())
	}
	return updated, skipped, nil
}
