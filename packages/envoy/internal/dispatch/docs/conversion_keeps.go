package docs

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/decisionmigration"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Retain each original receipt on the history document as the shadow check's independent
// baseline, and carry the same words, person and time to the converted node.
func copyConvertedDecisionKeeps(ctx context.Context, tx pgx.Tx, source legacyDecisionDocument, entries []decisionmigration.Entry, markdown string, blocks []model.ArtifactBlock, version int) error {
	headings := decisionmigration.SectionHeadings(entries, markdown, blocks)
	for _, entry := range entries {
		if entry.HeadingLevel == 0 {
			continue
		}
		id := headings[entry.ID]
		if id == "" {
			continue
		}
		if entry.Drop {
			continue
		}
		if _, err := tx.Exec(ctx, `insert into decision_keeps(artifact_id,block_id,text_sha256,changed_version,kept_by,kept_at)
 select $2::uuid,$3,text_sha256,$4,kept_by,kept_at from decision_keeps where artifact_id=$1::uuid and block_id=$5`, source.id, source.spec, entry.ID, version, id); err != nil {
			return err
		}
	}
	return nil
}
