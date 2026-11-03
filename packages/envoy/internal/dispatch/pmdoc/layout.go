package pmdoc

import "math"

// Layout is what one whole render (RenderWithLayout) records of the top level of the markdown it
// writes, valid only with that markdown. The render writes the document's top-level blocks
// B0 … Bn-1 as `B0 "\n\n" B1 … "\n\n" Bn-1 "\n"` (renderer.blocks); the layout holds, for each
// block, where its bytes end and what of it the rest of the document depends on (blockFlags), and
// whether the render is one that Splice can continue at all. It holds 5 bytes for each top-level
// block and a few words. A Layout is never modified once made: Splice answers a new one and
// leaves the one it was called on valid with the markdown it came with.
type Layout struct {
	// ends is each top-level block's end offset in the markdown, the separator after it excluded.
	ends []uint32
	// flags is each top-level block's blockFlags.
	flags []blockFlags
	// spanCells is how many empty cells the tables' spans added in all (tableGrid).
	spanCells int
	// incremental reports whether Splice may continue this render: see finish.
	incremental bool
}

// blockFlags is what a layout records of one top-level block.
type blockFlags uint8

const (
	// blockHoldsAsk: the block holds an ask block anywhere in it, itself included, as the tree is
	// read (the walk of WalkAskBlocks).
	blockHoldsAsk blockFlags = 1 << iota
	// blockHoldsFootnoteDefinition: the block holds a footnote definition anywhere in it, whose
	// label every block's rendering reads (footnoteLabelSet).
	blockHoldsFootnoteDefinition
	// blockChargesSpanCells: the block's tables' spans charged the span budget (tableGrid).
	blockChargesSpanCells
	// blockEmptyParagraph: the block is an empty paragraph as written (asWritten), which the
	// render writes as nothing and otherListMarkers skips.
	blockEmptyParagraph
	// blockBulletList and blockOrderedList: the block is a list of that kind, which
	// otherListMarkers compares with the block before it.
	blockBulletList
	blockOrderedList
	// blockOtherListMarker: the block is a list written with its kind's other marker
	// (otherListMarkers).
	blockOtherListMarker
)

// blockMarkerClass is the flags otherListMarkers reads of a block: whether it is an empty
// paragraph, a bullet list, an ordered list, or none of them. otherListMarkers over a container's
// blocks is a function of their classes alone.
const blockMarkerClass = blockEmptyParagraph | blockBulletList | blockOrderedList

// RenderWithLayout is RenderWithBlockOffsets that records the document's Layout as well, from the
// same render: the markdown, the offsets and the errors are RenderWithBlockOffsets'.
func RenderWithLayout(doc *Node) (string, []BlockOffset, *Layout, error) {
	return renderWithLayout(doc, true)
}

// RenderWithLayout is RenderWithLayout of d's tree without validating it again: the renderer's
// own refusals still run, and a panic in it is still an ErrPanic. The zero ValidDocument is
// RenderWithLayout's error for a nil tree.
func (d ValidDocument) RenderWithLayout() (string, []BlockOffset, *Layout, error) {
	return renderWithLayout(d.doc, false)
}

// renderWithLayout is RenderWithLayout, validating doc first only when validate says so.
func renderWithLayout(doc *Node, validate bool) (string, []BlockOffset, *Layout, error) {
	r, err := renderDocument(doc, maxSpanCells, validate, true)
	if err != nil {
		return "", nil, nil, err
	}
	return r.b.String(), r.blockOffsets, r.layout, nil
}

// Blocks is how many top-level blocks the layout's document holds.
func (l *Layout) Blocks() int {
	return len(l.ends)
}

// AskBlocks lists, in order, the index of each top-level block holding an ask block anywhere in it.
// The ask blocks of the document are the ask blocks of these blocks, since the document node itself
// is never an ask.
func (l *Layout) AskBlocks() []int {
	var asks []int
	for index, flags := range l.flags {
		if flags&blockHoldsAsk != 0 {
			asks = append(asks, index)
		}
	}
	return asks
}

// newLayout is a layout about to record blocks top-level blocks, incremental until a check fails.
func newLayout(blocks int) *Layout {
	return &Layout{ends: make([]uint32, 0, blocks), flags: make([]blockFlags, 0, blocks), incremental: true}
}

// onlyAnEmptyParagraphLayout is the layout of a document holding only an empty paragraph, which
// renders as nothing (render): one empty block, never incremental.
func onlyAnEmptyParagraphLayout() *Layout {
	return &Layout{ends: []uint32{0}, flags: []blockFlags{blockEmptyParagraph}}
}

// record records top-level block written, which r has just written with otherMarker, its span
// budget spans before it. A block after which r carries anything into the next block's rendering
// that a fresh renderer would not (carriesNothingPastABlock), or that ends past what an offset
// holds, leaves the layout not incremental.
func (l *Layout) record(r *renderer, written *Node, otherMarker bool, spans int) {
	flags := markerClass(written)
	if otherMarker {
		flags |= blockOtherListMarker
	}
	if r.spanBudget != spans {
		flags |= blockChargesSpanCells
	}
	end := r.b.Len()
	if uint64(end) > math.MaxUint32 || !r.carriesNothingPastABlock() {
		l.incremental = false
	}
	l.ends = append(l.ends, uint32(end))
	l.flags = append(l.flags, flags)
}

// finish completes the layout of a render of written, a document as written (asWritten), with
// spanCells for its tables' spans, r its last renderer, which first wrote a blank empty code block
// when blankEmptyCode says so. Splice may continue the render (incremental) only where every block
// of a whole render is written by the one rendering Splice reproduces block by block:
//   - at least two top-level blocks: a document holding only an empty paragraph renders as nothing
//     (render), and a single block is not worth splicing;
//   - the first block is not a rule, which renderBlocks rewrites `***` after reading the whole
//     markdown for front matter and the whole document for containers;
//   - no blank empty code block written, whose document render writes again bare where the whole
//     markdown holds a browser-only shape (holdsBrowserOnlyShape);
//   - the whole budget for spans, never spent: a table's cells depend on the budget a render has left
//     only where it reaches 0 (tableGrid takes the budget in min only to leave it 0), so while it
//     never does, every block writes what it writes with any budget that lasts;
//   - every record kept it incremental.
func (l *Layout) finish(written *Node, spanCells int, r *renderer, blankEmptyCode bool) {
	l.spanCells = spanCells - r.spanBudget
	l.incremental = l.incremental && len(written.Children) >= 2 && written.Children[0].Type != "hr" &&
		!blankEmptyCode && spanCells == maxSpanCells && r.spanBudget > 0
	for index, block := range written.Children {
		l.flags[index] |= holdsFlags(block)
	}
}

// markerClass is block's blockMarkerClass flags, block as written (asWritten).
func markerClass(block *Node) blockFlags {
	switch {
	case block.Type == "paragraph" && len(block.Children) == 0:
		return blockEmptyParagraph
	case block.Type == "bullet_list":
		return blockBulletList
	case block.Type == "ordered_list":
		return blockOrderedList
	}
	return 0
}

// askBlockType is the node type of an ask block, the one type WalkAskBlocks visits and
// blockHoldsAsk flags a block for.
const askBlockType = "ask"

// holdsBlocks reports whether a node's child can hold a block under it, and so whether a walk over
// the blocks of a document descends into it: text holds none. WalkAskBlocks and holdsFlags walk by
// this one rule, so the blocks a layout flags as holding an ask are exactly the blocks in which
// WalkAskBlocks finds one.
func holdsBlocks(child *Node) bool {
	return child.Type != "text"
}

// WalkAskBlocks calls visit with the id of each ask block node holds, in document order, wherever
// it stands: in a blockquote, a list item or another typed block, node itself included. The id is
// the block's BlockIDAttr attribute, "" for a block carrying none. It does not descend into text,
// which holds no block (holdsBlocks).
func WalkAskBlocks(node *Node, visit func(id string)) {
	if node == nil {
		return
	}
	if node.Type == askBlockType {
		id, _ := node.Attrs[BlockIDAttr].(string)
		visit(id)
	}
	for _, child := range node.Children {
		if holdsBlocks(child) {
			WalkAskBlocks(child, visit)
		}
	}
}

// holdsFlags reports whether block holds an ask block or a footnote definition, itself included.
// It walks as WalkAskBlocks walks (holdsBlocks), so the ask blocks of a block this flags are the
// ones that walk finds. asWritten keeps every node that is not inline, so the tree as read and as
// written answer alike.
func holdsFlags(block *Node) blockFlags {
	var flags blockFlags
	switch block.Type {
	case askBlockType:
		flags |= blockHoldsAsk
	case "footnote_definition":
		flags |= blockHoldsFootnoteDefinition
	}
	for _, child := range block.Children {
		if holdsBlocks(child) {
			flags |= holdsFlags(child)
		}
	}
	return flags
}

// carriesNothingPastABlock reports whether r, between two top-level blocks, carries nothing into the
// next block's rendering that a fresh renderer would not: no held line start (every inline run ends
// with endLine), no container frame, no pending `***` rule (itemBlocks sets it only for an hr it
// writes next, which clears it), no open code span. Splice renders a block with a fresh renderer, so
// a whole render where this fails anywhere is not incremental. What else a renderer carries is
// exact for another reason (Splice).
func (r *renderer) carriesNothingPastABlock() bool {
	return r.heldLineStart == nil && len(r.scopes) == 0 && !r.asteriskRule && r.inlineCodeFence == "" && !r.inlineCodePadded
}

// start is where top-level block index begins in the layout's markdown.
func (l *Layout) start(index int) int {
	if index == 0 {
		return 0
	}
	return int(l.ends[index-1]) + len("\n\n")
}

// Splice is the markdown and layout of the document after a change to some of its top-level blocks,
// from markdown, the rendering l was recorded with, rendering only the changed blocks: exactly what
// RenderWithLayout writes and records for the changed document, or ok false, telling the caller to
// render the document whole. l stays valid with markdown; the answered layout is valid with the
// answered markdown. With nothing touched it answers markdown and l themselves.
//
// touched is the index of every top-level block that changed, ascending; the caller guarantees that
// the document still holds as many top-level blocks, in the same order, and that no block outside
// touched changed. read answers the document's current top-level block at an index, as
// ReadBlockForRendering reads it: Splice reads each touched block, and each block l flags as holding
// a footnote definition, through it, and keeps none of them.
//
// Each touched block k is rendered in context: by a renderer whose buffer holds the new markdown up
// to the block, as a whole render's does there, so every read back of the bytes before it
// (endsWith, closesAfterPunctuation, wwwMayFollow, endLine's line and the line before it) reads what
// a whole render reads; with the document's footnote labels, read from the blocks that define them;
// with block k's other-list-marker flag; and with a span budget that lasts. Then come the separator
// and the old bytes after block k. Splice answers ok false unless every guard holds:
//   - G0 l is incremental (finish), and markdown is as long as l says;
//   - G1 touched is ascending, and each index is one of l's blocks;
//   - G2 the document holds at least two top-level blocks (finish);
//   - G3 the first block is not a rule, before or after (finish; checked again for block 0);
//   - G4 neither block k's old flags nor block k now hold a footnote definition: the label set every
//     block's escapes read is then the one the flagged blocks, none of them touched, define;
//   - G5 block k's marker class (blockMarkerClass) is what it was: otherListMarkers over the
//     document's blocks is then what it was, so block k takes its recorded other-list-marker flag
//     and every later block keeps its own;
//   - G6 block k's old rendering charged no span cells, and the budget the old total and the new
//     charges leave stays above 0 after it: no render of the new document then reaches the budget's
//     end, so every block's bytes are what they are with any budget that lasts (finish);
//   - G7 block k's rendering writes no blank empty code block, so the whole render of the new
//     document takes no second pass (render);
//   - G8 block k reads, is a block, validates as Validate validates it under the document (depth 1;
//     Validate checks nothing across blocks), and renders without error or panic, leaving the
//     renderer carrying nothing past it (carriesNothingPastABlock).
//
// Why the rest of what a renderer carries across blocks is exact:
//   - The bytes of a block after k do not depend on block k's bytes: every read back of the markdown
//     written stops at the line feed that ends the separator before the block reading (each inline
//     run's lines start no earlier than its block, and a line's start is found by scanning back to a
//     line feed), so it reads the separator's line feed whatever block k wrote.
//   - footnoteLineAt and footnoteLabel: a block compares footnoteLineAt with the start of a line it
//     writes, at or after its own start; a definition before the block began before its separator,
//     so the comparison fails whatever offset it carries, and block k holds no definition (G4). The
//     renderer here starts with none, which fails it too.
//   - labelBrackets is read only under an open link mark (writeInlineRun), and every inline run
//     starts with no mark open and sets it when it opens a link, so a value carried from an earlier
//     block is never read.
//   - asteriskRule, heldLineStart, scopes and the open code span: carriesNothingPastABlock, checked
//     after every block of the whole render (record) and after every block rendered here.
//   - bareEmptyCode and wroteBlankEmptyCode: false in an incremental render (finish) and in block k
//     (G7).
//   - blockOffsets do not shape the markdown; Splice answers none.
func (l *Layout) Splice(markdown string, touched []int, read func(index int) (*Node, error)) (string, *Layout, bool) {
	if len(touched) == 0 && l != nil {
		return markdown, l, true
	}
	spliced, err := l.splice(markdown, touched, read)
	if err != nil || spliced == nil {
		return "", nil, false
	}
	return spliced.markdown, spliced.layout, true
}

// splicedRendering is a rendering Splice answers.
type splicedRendering struct {
	markdown string
	layout   *Layout
}

// splice is Splice, answering nil where a guard fails and an ErrPanic where rendering panicked.
func (l *Layout) splice(markdown string, touched []int, read func(index int) (*Node, error)) (spliced *splicedRendering, err error) {
	defer recoverPanic(&spliced, &err, "splicing a document's blocks")
	if l == nil || !l.incremental || read == nil || len(l.ends) == 0 || len(markdown) != int(l.ends[len(l.ends)-1])+len("\n") {
		return nil, nil
	}
	for position, index := range touched {
		if index < 0 || index >= len(l.ends) || position > 0 && index <= touched[position-1] {
			return nil, nil
		}
		if l.flags[index]&(blockHoldsFootnoteDefinition|blockChargesSpanCells) != 0 {
			return nil, nil
		}
	}
	labels, ok := l.footnoteLabels(read)
	if !ok {
		return nil, nil
	}
	r := &renderer{footnoteLabels: labels, spanBudget: maxSpanCells - l.spanCells}
	r.b.Grow(len(markdown) + len(markdown)/16)
	next := &Layout{ends: make([]uint32, len(l.ends)), flags: make([]blockFlags, len(l.flags)), spanCells: l.spanCells, incremental: true}
	copy(next.flags, l.flags)
	copied := 0
	for _, index := range touched {
		r.b.WriteString(markdown[copied:l.start(index)])
		node, err := read(index)
		if err != nil || node == nil || !isBlockNodeType(node.Type) || validateNode(node, 1) != nil || index == 0 && node.Type == "hr" {
			return nil, nil
		}
		holds := holdsFlags(node)
		if holds&blockHoldsFootnoteDefinition != 0 {
			return nil, nil
		}
		written := asWritten(node)
		old := l.flags[index]
		if markerClass(written) != old&blockMarkerClass {
			return nil, nil
		}
		r.otherListMarker = old&blockOtherListMarker != 0
		spans := r.spanBudget
		r.block(written, "")
		if r.err != nil || r.wroteBlankEmptyCode || r.spanBudget <= 0 || !r.carriesNothingPastABlock() || uint64(r.b.Len()) > math.MaxUint32 {
			return nil, nil
		}
		flags := old&(blockMarkerClass|blockOtherListMarker) | holds
		if r.spanBudget != spans {
			flags |= blockChargesSpanCells
			next.spanCells += spans - r.spanBudget
		}
		next.flags[index] = flags
		next.ends[index] = uint32(r.b.Len())
		copied = int(l.ends[index])
	}
	r.b.WriteString(markdown[copied:])
	if uint64(r.b.Len()) > math.MaxUint32 {
		return nil, nil
	}
	shift, nextTouched := 0, 0
	for index, end := range l.ends {
		if nextTouched < len(touched) && touched[nextTouched] == index {
			shift = int(next.ends[index]) - int(end)
			nextTouched++
			continue
		}
		next.ends[index] = uint32(int(end) + shift)
	}
	return &splicedRendering{markdown: r.b.String(), layout: next}, nil
}

// footnoteLabels is the label set of the document l was recorded from, read through read from the
// blocks l flags as holding a footnote definition, as render reads it from the document as written
// (definedFootnoteLabels over asWritten).
func (l *Layout) footnoteLabels(read func(index int) (*Node, error)) (footnoteLabelSet, bool) {
	var defining []*Node
	for index, flags := range l.flags {
		if flags&blockHoldsFootnoteDefinition == 0 {
			continue
		}
		block, err := read(index)
		if err != nil || block == nil {
			return footnoteLabelSet{}, false
		}
		defining = append(defining, asWritten(block))
	}
	return definedFootnoteLabels(&Node{Type: "doc", Children: defining}), true
}
