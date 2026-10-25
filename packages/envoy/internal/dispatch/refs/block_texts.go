package refs

import (
	"context"
	"fmt"
)

// BlockVersion is what the block index recorded of one block at one version of its document
// (IndexBlocks writes document_block_texts and document_indexed_versions).
type BlockVersion struct {
	// Exists is whether the document has the version.
	Exists bool
	// Indexed is whether a block-aware build indexed the version, so its blocks' texts are on
	// record; Earliest is the document's earliest indexed version, or 0 when none is.
	Indexed  bool
	Earliest int
	// Text is the block's whole text at the version: the latest text recorded at or before it.
	// It is nil when the version does not hold the block.
	Text *string
	// RemovedVersion is the latest version that recorded the block deleted, or 0.
	RemovedVersion int
}

// BlockTextAt reads block blockID of document artifactID as version records it. The text is
// read only from a version the index recorded: a version an older build wrote and no
// block-aware build indexed is not Indexed, and its Text is nil.
func BlockTextAt(ctx context.Context, q Queryer, artifactID, blockID string, version int) (BlockVersion, error) {
	var read BlockVersion
	var earliest, removed *int
	if err := q.QueryRow(ctx, `
		select exists (select 1 from artifact_versions where artifact_id = $1 and number = $3),
		       exists (select 1 from document_indexed_versions where artifact_id = $1 and version = $3),
		       (select min(version) from document_indexed_versions where artifact_id = $1),
		       (select text from document_block_texts where artifact_id = $1 and block_id = $2 and version <= $3
		        order by version desc limit 1),
		       (select max(version) from document_block_texts where artifact_id = $1 and block_id = $2 and text is null)
	`, artifactID, blockID, version).Scan(&read.Exists, &read.Indexed, &earliest, &read.Text, &removed); err != nil {
		return BlockVersion{}, fmt.Errorf("read block %q of %s at version %d: %w", blockID, artifactID, version, err)
	}
	if !read.Indexed {
		read.Text = nil
	}
	if earliest != nil {
		read.Earliest = *earliest
	}
	if removed != nil {
		read.RemovedVersion = *removed
	}
	return read, nil
}

// CurrentBlockText is the whole text of block blockID of document artifactID as its latest
// recorded version holds it, or "" when that version deleted it or none recorded it.
func CurrentBlockText(ctx context.Context, q Queryer, artifactID, blockID string) (string, error) {
	var current *string
	if err := q.QueryRow(ctx, `
		select (select text from document_block_texts where artifact_id = $1 and block_id = $2
		        order by version desc limit 1)
	`, artifactID, blockID).Scan(&current); err != nil {
		return "", fmt.Errorf("read the text of block %q of %s: %w", blockID, artifactID, err)
	}
	if current == nil {
		return "", nil
	}
	return *current, nil
}
