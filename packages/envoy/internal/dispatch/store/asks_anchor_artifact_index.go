package store

import (
	"context"
	"fmt"
)

// anchorArtifactIDCanonicalForm matches the lower-case hyphenated uuid every server-written anchor
// carries in artifact_id. It is the guard of asks_anchor_artifact_id's index expression (migration
// 0137) and of the asks read that reaches it (docs.asksAnchorArtifactIDExpression); the three
// copies must stay identical, or this count stops describing the rows the index leaves out.
const anchorArtifactIDCanonicalForm = `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`

// AsksOutsideAnchorArtifactIndex counts the anchored asks that asks_anchor_artifact_id (migration
// 0137) leaves out: those whose anchor names an artifact_id that is not a lower-case document id,
// so the index expression's regex guard answers null for them rather than casting. Settlement's
// sweep reads asks through that index and keeps the text comparison beside it, so it records no
// mark for these rows and their document's marks are swept once their TTL passes. An ask whose
// anchor carries no artifact_id at all is not one of them: it names no document to begin with.
// The boot logs the count so that rows an earlier release stored, which no migration and no
// census refuses, are still visible to whoever reads the logs.
func (s *Store) AsksOutsideAnchorArtifactIndex(ctx context.Context) (int64, error) {
	var count int64
	if err := s.Pool.QueryRow(ctx, `
		select count(*)
		from asks
		where anchor is not null
			and anchor->>'artifact_id' is not null
			and anchor->>'artifact_id' !~ '`+anchorArtifactIDCanonicalForm+`'
	`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count anchored asks outside asks_anchor_artifact_id: %w", err)
	}
	return count, nil
}
