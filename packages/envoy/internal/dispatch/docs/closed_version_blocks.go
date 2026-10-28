package docs

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pmezard/go-difflib/difflib"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// indexedBlock is a block row of a document's index: its id, type and text's sha256 (hex).
type indexedBlock struct {
	id, typ, sum string
}

// indexedBlocks is each block row the index holds for document artifactID, by id, read in the
// caller's transaction, which holds the document's owner row.
func indexedBlocks(ctx context.Context, tx pgx.Tx, artifactID string) ([]indexedBlock, error) {
	rows, err := tx.Query(ctx, `
		select block_id, type, text_sha256 from document_blocks where artifact_id = $1 order by block_id
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read the indexed blocks of %s: %w", artifactID, err)
	}
	indexed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (indexedBlock, error) {
		var block indexedBlock
		err := row.Scan(&block.id, &block.typ, &block.sum)
		return block, err
	})
	if err != nil {
		return nil, fmt.Errorf("read the indexed blocks of %s: %w", artifactID, err)
	}
	return indexed, nil
}

// closedVersionBlocks is the rendering of the version whose stored markdown is stored, with each
// block of it that a row of the document's index (indexed, by id) or a block of the live document
// - live, with liveBlocks (TextWithBlocks) - accounts for, under that row's or live block's id. A
// block of the version that neither accounts for, and a live block that matches no block of it,
// are left out, so each block indexed at the version is text that version holds.
//
// The passes run in order, each matching only blocks the passes before it left unmatched: typed
// blocks by id (matchTypedBlocks); the rows, first aligned with the live blocks that have one
// (alignRows), then claimed by type and text (claimRows); then the live text, aligned within each
// gap between two rows aligned in the first step (alignGaps); last, the blocks of each text left
// on both sides, paired in order where both hold equally many (pairLeftovers). A live block that
// repeats an earlier block's id - a paste keeps its source's id until a settlement stamps a new
// one - takes part in no pass: refs.IndexBlocks indexes only the first holder of an id, so a
// repeated holder's id would name another block's row.
//
// A row whose text the version does not hold - indexed at an older version whose text of that
// block this version changed - and whose block's live text the version does not hold either
// matches no block, and is still recorded deleted at this version, although this version holds
// the block.
func closedVersionBlocks(stored, live string, liveBlocks []model.ArtifactBlock, indexed []indexedBlock) (string, []model.ArtifactBlock, error) {
	tree, err := pmdoc.ParseRendering(stored)
	if err != nil {
		return "", nil, err
	}
	markdown, offsets, err := documentMarkdownWithOffsets(tree)
	if err != nil {
		return "", nil, err
	}
	match := newVersionMatch(markdown, offsets, liveBlocks, indexed)
	match.readLive(live, match.matchTypedBlocks())
	anchors := match.alignRows()
	match.claimRows(indexed)
	match.alignGaps(anchors)
	match.pairLeftovers()
	return markdown, match.blocks(), nil
}

// versionMatch is one run of closedVersionBlocks: the version's blocks (offsets, into markdown),
// the live blocks and the index's rows, and the ids the passes have given the version's blocks so
// far.
type versionMatch struct {
	markdown   string
	offsets    []pmdoc.BlockOffset
	liveBlocks []model.ArtifactBlock
	// firstHolder is, by id, the index in liveBlocks of the first block holding it.
	firstHolder map[string]int
	// rows is the index's rows by id.
	rows map[string]indexedBlock
	// ids is the id each block of the version has taken, empty while it has none, and used each id
	// a block of the version has taken.
	ids  []string
	used map[string]bool
	// sums is each block of the version's type and text sha256.
	sums []string
	// versionUntyped is the index in offsets of each untyped block of the version, in document
	// order, and versionKeys its type and text.
	versionUntyped []int
	versionKeys    []string
	// liveUntyped is the index in liveBlocks of each untyped live block that is the first holder of
	// its id, in document order, and liveKeys its type and text as the version would hold it
	// (versionText).
	liveUntyped []int
	liveKeys    []string
}

// newVersionMatch lists the version's blocks and the live blocks' first holders, with no block
// matched yet.
func newVersionMatch(markdown string, offsets []pmdoc.BlockOffset, liveBlocks []model.ArtifactBlock, indexed []indexedBlock) *versionMatch {
	match := &versionMatch{
		markdown:    markdown,
		offsets:     offsets,
		liveBlocks:  liveBlocks,
		firstHolder: make(map[string]int, len(liveBlocks)),
		rows:        make(map[string]indexedBlock, len(indexed)),
		ids:         make([]string, len(offsets)),
		used:        make(map[string]bool, len(offsets)),
		sums:        make([]string, len(offsets)),
	}
	for index, block := range liveBlocks {
		if _, held := match.firstHolder[block.ID]; !held {
			match.firstHolder[block.ID] = index
		}
	}
	for _, row := range indexed {
		match.rows[row.id] = row
	}
	for index, offset := range offsets {
		body := markdown[offset.From:offset.To]
		match.sums[index] = offset.Type + "\x00" + refs.BlockTextSum(body)
		if !pmdoc.IsTypedBlock(offset.Type) {
			match.versionUntyped = append(match.versionUntyped, index)
			match.versionKeys = append(match.versionKeys, offset.Type+"\x00"+body)
		}
	}
	return match
}

// take gives the version's block at index the id, and reports whether it did. It is the one place
// a block of the version takes an id, so that each block takes at most one id and each id names at
// most one block: it refuses a block that has one and an id a block has taken. refs.IndexBlocks
// keeps only the first holder of a repeated id and records every other block it held deleted, so a
// second taker would record a block deleted at a version that holds it.
func (m *versionMatch) take(index int, id string) bool {
	if m.ids[index] != "" || m.used[id] {
		return false
	}
	m.ids[index], m.used[id] = id, true
	return true
}

// matchTypedBlocks gives each typed block of the version the id of the live block of its id and
// type, and returns the version's text of each typed block it matched, by id. The version's
// markdown names the id of a typed block alone; its parse mints the others, which no reader knows,
// so those ids are never indexed. A typed block's live text may differ from the version's in what
// the close wrote into it.
func (m *versionMatch) matchTypedBlocks() map[string]string {
	typedTexts := map[string]string{}
	for index, offset := range m.offsets {
		if !pmdoc.IsTypedBlock(offset.Type) {
			continue
		}
		if at, held := m.firstHolder[offset.ID]; held && m.liveBlocks[at].Type == offset.Type && m.take(index, offset.ID) {
			typedTexts[offset.ID] = m.markdown[offset.From:offset.To]
		}
	}
	return typedTexts
}

// readLive lists the untyped live blocks that are the first holders of their ids, each keyed by
// its type and its text in live with each typed block inside it read as the version's text of that
// block (typedTexts), so a block holding an ask whose state the close wrote still matches.
func (m *versionMatch) readLive(live string, typedTexts map[string]string) {
	for index, block := range m.liveBlocks {
		if pmdoc.IsTypedBlock(block.Type) || m.firstHolder[block.ID] != index {
			continue
		}
		m.liveUntyped = append(m.liveUntyped, index)
		m.liveKeys = append(m.liveKeys, block.Type+"\x00"+versionText(live, m.liveBlocks, index, typedTexts))
	}
}

// alignRows is the first step by which every row of the index that holds the text of a block of
// the version claims that block before any live text is compared. A row was indexed from a
// version's text of its block under that block's id, so where the version holds the row's type
// and text (the sha256 of markdown[From:To], refs.BlockTextSum), the block keeps its row, its
// changed version and its text under its own id rather than being recorded deleted at a version
// that holds it, and a live block an edit typed with the same text takes none of it.
//
// The version's untyped blocks are aligned in document order with the live untyped blocks that
// have a row, keyed by that row's type and text sha256 against each version block's, by difflib's
// matching blocks with no junk heuristic: unlike a greedy first match, a block repeated further
// on, or a block an edit moved, cannot pull the alignment past the unchanged run that follows it,
// and the longest runs of unchanged blocks, which an edit leaves around itself, are kept whole, so
// each of several identical blocks keeps its own row. It never gives an id a block has taken. It
// returns each match, as indexes into versionUntyped and liveUntyped, in document order.
func (m *versionMatch) alignRows() [][2]int {
	versionSums := make([]string, len(m.versionUntyped))
	for at, index := range m.versionUntyped {
		versionSums[at] = m.sums[index]
	}
	var liveSums []string
	var liveIndexed []int
	for at, index := range m.liveUntyped {
		if row, held := m.rows[m.liveBlocks[index].ID]; held {
			liveSums = append(liveSums, row.typ+"\x00"+row.sum)
			liveIndexed = append(liveIndexed, at)
		}
	}
	var anchors [][2]int
	for _, match := range difflib.NewMatcherWithJunk(versionSums, liveSums, false, nil).GetMatchingBlocks() {
		for step := range match.Size {
			anchor := [2]int{match.A + step, liveIndexed[match.B+step]}
			if !m.take(m.versionUntyped[anchor[0]], m.liveBlocks[m.liveUntyped[anchor[1]]].ID) {
				continue
			}
			anchors = append(anchors, anchor)
		}
	}
	return anchors
}

// claimRows is the second step of the rows' claim: each block of the version still unmatched
// takes the row of its type and text whose id no other block took, a typed block only the row of
// its own id, an untyped one the first such row whose id is a live block's, in live document
// order, else the first of the others by id. A block an edit moved across a first-step match keeps
// its row so, and a block an unsettled edit deleted keeps its row under its own id, which no live
// block holds. It reorders indexed.
func (m *versionMatch) claimRows(indexed []indexedBlock) {
	rank := func(id string) int {
		if at, held := m.firstHolder[id]; held {
			return at
		}
		return len(m.liveBlocks)
	}
	slices.SortStableFunc(indexed, func(a, b indexedBlock) int { return cmp.Compare(rank(a.id), rank(b.id)) })
	holders := make(map[string][]string, len(indexed))
	for _, row := range indexed {
		key := row.typ + "\x00" + row.sum
		holders[key] = append(holders[key], row.id)
	}
	for index, offset := range m.offsets {
		if m.ids[index] != "" {
			continue
		}
		for _, id := range holders[m.sums[index]] {
			if (!pmdoc.IsTypedBlock(offset.Type) || id == offset.ID) && m.take(index, id) {
				break
			}
		}
	}
}

// alignGaps compares live text, between the version's untyped blocks no row claimed and the live
// untyped blocks whose id no row claimed for a block of the version, keyed by type and text as the
// version would hold it (readLive). They are aligned by difflib as in alignRows, within each gap
// between two consecutive first-step matches (anchors), so no match crosses one. Those matches are
// the only bounds that keep document order on both sides: difflib finds a match in every span that
// holds an equal pair, so a block of the version that claimRows gave a live block's row is one an
// edit moved across a first-step match, and a block it gave the row of an id no live block holds
// has no place in the live document to cross. The alignment matches every untyped block of a
// document none of whose blocks the index holds yet, and a block whose row holds an older
// version's text that this version changed. A version is compared and indexed as it renders
// today, as the live text is.
//
// A text either side still holds more than once takes no part: with no row to name its copies, a
// copy an edit typed would take an original's id and history, so its copies are left to
// pairLeftovers.
func (m *versionMatch) alignGaps(anchors [][2]int) {
	versionLeft, liveLeft := m.leftovers()
	repeated := func(key string) bool { return len(versionLeft[key]) > 1 || len(liveLeft[key]) > 1 }
	var gapVersion, gapLive []string
	var gapVersionAt, gapLiveAt []int
	fromVersion, fromLive := 0, 0
	for _, anchor := range append(anchors, [2]int{len(m.versionUntyped), len(m.liveUntyped)}) {
		gapVersion, gapVersionAt = gapVersion[:0], gapVersionAt[:0]
		for at := fromVersion; at < anchor[0]; at++ {
			if m.ids[m.versionUntyped[at]] == "" && !repeated(m.versionKeys[at]) {
				gapVersion, gapVersionAt = append(gapVersion, m.versionKeys[at]), append(gapVersionAt, m.versionUntyped[at])
			}
		}
		gapLive, gapLiveAt = gapLive[:0], gapLiveAt[:0]
		for at := fromLive; at < anchor[1]; at++ {
			if !m.used[m.liveBlocks[m.liveUntyped[at]].ID] && !repeated(m.liveKeys[at]) {
				gapLive, gapLiveAt = append(gapLive, m.liveKeys[at]), append(gapLiveAt, m.liveUntyped[at])
			}
		}
		if len(gapVersion) > 0 && len(gapLive) > 0 {
			for _, match := range difflib.NewMatcherWithJunk(gapVersion, gapLive, false, nil).GetMatchingBlocks() {
				for step := range match.Size {
					m.take(gapVersionAt[match.A+step], m.liveBlocks[gapLiveAt[match.B+step]].ID)
				}
			}
		}
		fromVersion, fromLive = anchor[0]+1, anchor[1]+1
	}
}

// leftovers groups, by type and text, the version's untyped blocks that have no id and the live
// untyped blocks whose id no block has taken, each in document order, as indexes into offsets and
// into liveBlocks.
func (m *versionMatch) leftovers() (versionLeft, liveLeft map[string][]int) {
	versionLeft, liveLeft = map[string][]int{}, map[string][]int{}
	for at, index := range m.versionUntyped {
		if m.ids[index] == "" {
			versionLeft[m.versionKeys[at]] = append(versionLeft[m.versionKeys[at]], index)
		}
	}
	for at, index := range m.liveUntyped {
		if !m.used[m.liveBlocks[index].ID] {
			liveLeft[m.liveKeys[at]] = append(liveLeft[m.liveKeys[at]], index)
		}
	}
	return versionLeft, liveLeft
}

// pairLeftovers pairs, for each type and text, the version's untyped blocks and the live untyped
// blocks still unmatched that hold it, in order - the first with the first, and so on - where both
// sides hold equally many, and never gives an id a block has taken. A text held more than once
// says which copy is which only so: otherwise a copy an edit typed would take an original's id and
// history, so no copy is matched until a settlement versions them (an original left unmatched is
// dated to the version the reopen's settlement writes). A text held once on each side names its
// block: one an edit moved across others, in a document with no row to name it, matches no block
// within its gap.
func (m *versionMatch) pairLeftovers() {
	versionLeft, liveLeft := m.leftovers()
	for key, left := range versionLeft {
		holding := liveLeft[key]
		if len(holding) != len(left) {
			continue
		}
		for step, index := range left {
			m.take(index, m.liveBlocks[holding[step]].ID)
		}
	}
}

// blocks is the version's blocks the passes gave an id, in document order, each under it.
func (m *versionMatch) blocks() []model.ArtifactBlock {
	blocks := make([]model.ArtifactBlock, 0, len(m.offsets))
	for index, offset := range m.offsets {
		if m.ids[index] != "" {
			blocks = append(blocks, model.ArtifactBlock{ID: m.ids[index], Type: offset.Type, From: offset.From, To: offset.To})
		}
	}
	return blocks
}

// versionText is the text of blocks[index] in live, with each typed block inside it that the
// version holds (typedTexts, by id) read as the version's text of that block. blocks are in
// document order, each parent before its children, so the blocks inside one follow it up to the
// first that starts at or past its end.
func versionText(live string, blocks []model.ArtifactBlock, index int, typedTexts map[string]string) string {
	outer := blocks[index]
	var text strings.Builder
	at := outer.From
	for _, inner := range blocks[index+1:] {
		if inner.From >= outer.To {
			break
		}
		held, typed := typedTexts[inner.ID]
		if inner.From < at || !typed || !pmdoc.IsTypedBlock(inner.Type) {
			continue
		}
		text.WriteString(live[at:inner.From])
		text.WriteString(held)
		at = inner.To
	}
	if at == outer.From {
		return live[outer.From:outer.To]
	}
	text.WriteString(live[at:outer.To])
	return text.String()
}
