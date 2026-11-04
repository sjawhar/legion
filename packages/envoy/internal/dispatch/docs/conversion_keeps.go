package docs

import (
	"context"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/decisionmigration"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Consume each source heading once, including dropped ask restatements. Titles are not
// identities: two choices of the same title have distinct blocks and distinct review receipts.
// Full canonical heading text is used rather than the block index's truncated excerpt.
func moveConvertedDecisionKeeps(ctx context.Context, tx pgx.Tx, source legacyDecisionDocument, entries []decisionmigration.Entry, markdown string, blocks []model.ArtifactBlock, version int) error {
	sort.SliceStable(blocks, func(i, j int) bool { return blocks[i].From < blocks[j].From })
	headings := map[string][]string{}
	for _, block := range blocks {
		if block.Type == "heading" {
			heading := strings.TrimSpace(markdown[block.From:block.To])
			headings[heading] = append(headings[heading], block.ID)
		}
	}
	for _, entry := range entries {
		if entry.HeadingLevel == 0 {
			continue
		}
		heading := strings.Repeat("#", entry.HeadingLevel) + " " + entry.Title
		ids := headings[heading]
		if len(ids) == 0 {
			continue
		}
		id := ids[0]
		headings[heading] = ids[1:]
		if entry.Drop {
			continue
		}
		if _, err := tx.Exec(ctx, `update decision_keeps set artifact_id=$2::uuid,block_id=$3,changed_version=$4 where artifact_id=$1::uuid and block_id=$5`, source.id, source.spec, entry.ID, version, id); err != nil {
			return err
		}
	}
	return nil
}
