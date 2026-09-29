package pmdoc

import (
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"sort"
)

// skip names what a comparison leaves aside: the attributes it names, or all of them, and with
// text a textblock's inline content.
type skip struct {
	names map[string]bool
	all   bool
	text  bool
}

// shapeOnly compares blocks alone, their kinds and how they nest.
var shapeOnly = skip{all: true, text: true}

func (s skip) has(name string) bool { return s.all || s.names[name] }

// A document's markdown reads back as written when Parse gives back its blocks, their attributes and
// their text, as the markdown writes them (asWritten). Block ids, a typed block's server-owned
// attributes and anchor marks are not written, so they are not compared, and neither is an empty
// paragraph the renderer does not write (writtenChildren).

// ReadBack is what doc's markdown reads back as.
func ReadBack(doc *Node) (*Node, error) {
	return readBack(doc, maxSpanCells)
}

// readBack is what doc's markdown, its tables' spans adding at most spanCells cells (render),
// reads back as.
func readBack(doc *Node, spanCells int) (*Node, error) {
	r, err := render(doc, spanCells)
	if err != nil {
		return nil, err
	}
	return Parse(r.b.String())
}

// NewMisread names how after, which a write made from before, reads back otherwise where before
// did not, or is "" when it does not. Each block that reads back otherwise is found as far down as
// its markdown still pairs with what reads back (misreads), and one that before already read back
// otherwise the same way, under the same block id, is not the write's: a textblock whose text
// differs in the same characters, an attribute with the same values, or a block that pairs with
// nothing. So a write beside a stale misread, or inside the block that holds one, is judged by what
// it changed, while a new misread in that block is still named. An attribute before read back with
// another value anywhere (a table cell's alignment) is not compared. A before whose markdown the
// parser refuses (a browser edit can leave one) gives nothing to judge against, and is "" whether
// or not after parses: the checks that read each changed block alone still refuse one the write
// leaves unreadable. Only a refusal (ErrSchema) reading either back is a verdict; any other error,
// a panic (ErrPanic) among them, is this package's bug, not a misread, and is the error.
func NewMisread(before, after *Node) (string, error) {
	return newMisread(before, after, maxSpanCells)
}

// newMisread is NewMisread reading before and after back with their tables' spans adding at most
// spanCells cells each.
func newMisread(before, after *Node, spanCells int) (string, error) {
	backAfter, err := readBack(after, spanCells)
	if err != nil && !errors.Is(err, ErrSchema) {
		return "", err
	}
	written := asWritten(after)
	if err == nil && readDifference(written, backAfter, skip{}) == "" {
		return "", nil
	}
	backBefore, beforeErr := readBack(before, spanCells)
	if beforeErr != nil && !errors.Is(beforeErr, ErrSchema) {
		return "", beforeErr
	}
	if beforeErr != nil {
		return "", nil
	}
	if err != nil {
		return err.Error(), nil
	}
	previous := asWritten(before)
	drift := skip{names: attributeDrift(previous, backBefore)}
	type sameMisread struct{ id, differs string }
	known := map[sameMisread]bool{}
	for _, stale := range misreads(previous, backBefore, drift) {
		known[sameMisread{stale.id, stale.differs}] = true
	}
	for _, found := range misreads(written, backAfter, drift) {
		if found.id == "" || !known[sameMisread{found.id, found.differs}] {
			return found.reason, nil
		}
	}
	return "", nil
}

// documentMisread names how doc, a whole document a write makes, reads back otherwise, or is ""
// when it does not: every block that reads back otherwise is the write's, the first named as far
// down as its markdown still pairs (misreads), and a doc whose markdown the parser refuses is named
// by that refusal. Only a refusal (ErrSchema) is a verdict; any other error is the error.
func documentMisread(doc *Node) (string, error) {
	back, err := ReadBack(doc)
	if err != nil {
		if errors.Is(err, ErrSchema) {
			return err.Error(), nil
		}
		return "", err
	}
	written := asWritten(doc)
	difference := readDifference(written, back, skip{})
	if difference == "" {
		return "", nil
	}
	if found := misreads(written, back, skip{}); len(found) > 0 {
		return found[0].reason, nil
	}
	return difference, nil
}

// RefuseMisreadDocument refuses (ErrSchema) a whole document a write makes, a spec, an upload or a
// version, that reads back otherwise (documentMisread), and RefuseMisreadWrite a write into a
// document, an insert, whose result reads back otherwise where before did not (NewMisread). What
// is stored is the rendering, which the next read would give back as another document or refuse.
// The rules that read a shape refuse what they know first, so a refusal here names a shape none of
// them reads, and is logged. Any other error reading it back, a panic (ErrPanic) among them, is
// the error.
func RefuseMisreadDocument(doc *Node) error {
	return refuseMisread(documentMisread(doc))
}

// RefuseMisreadWrite refuses a write into a document; see RefuseMisreadDocument. It reads both
// documents back as renderSpanless writes them. An insert checks its write with it once for each
// operation of a batch, and a batch holds as many as a request carries, where a budget of span
// cells for each read-back would let one batch spend two for every operation. An insert writes
// markdown, which carries no span, between document-level blocks, so it changes no table and every
// span reads back the same before and after it either way; spanless, a read-back costs no more
// than the cells the tables hold.
func RefuseMisreadWrite(before, after *Node) error {
	return refuseMisread(newMisread(before, after, 0))
}

func refuseMisread(misread string, err error) error {
	if err != nil {
		return err
	}
	if misread == "" {
		return nil
	}
	slog.Warn("pmdoc: refused a write whose markdown reads back otherwise", "misread", misread)
	return fmt.Errorf("%w: the markdown the document would be stored as reads back otherwise (%s)", ErrSchema, misread)
}

// misread is a block that reads back otherwise: its id, what differs (the same for the same
// misread whatever else changed around it), and how a reader is told.
type misread struct {
	id, differs, reason string
}

// misreads lists the blocks under want, a block as written, that read back otherwise in got, in
// document order, each as far down as the two still pair: children pair one to one when both hold
// as many, and otherwise as alignChildren pairs them, a child it cannot pair naming itself.
func misreads(want, got *Node, ignore skip) []misread {
	var found []misread
	collectMisreads(want, got, ignore, &found)
	return found
}

func collectMisreads(want, got *Node, ignore skip, found *[]misread) {
	add := func(id, differs, reason string) {
		*found = append(*found, misread{id: id, differs: differs, reason: reason})
	}
	own := selfDifference(want, got, ignore)
	switch {
	case own.kind != "":
		add(blockIDOf(want), want.Type+" as "+got.Type, own.kind)
		return
	case own.attribute != "":
		add(blockIDOf(want), want.Type+" "+own.attribute, own.attribute)
	}
	if isTextblock(want.Type) {
		if own.text != "" {
			add(blockIDOf(want), want.Type+" text "+own.textDiffers, own.text)
		}
		return
	}
	written := writtenChildren(want)
	pairs, unpaired := pairChildren(want.Type, written, got.Children, ignore)
	for _, pair := range pairs {
		collectMisreads(written[pair[0]], got.Children[pair[1]], ignore, found)
	}
	indexes := make([]int, 0, len(unpaired))
	for index := range unpaired {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		child := written[index]
		add(blockIDOf(child), child.Type+" unpaired", unpaired[index])
	}
}

// pairChildren pairs written, a parent's children as written, with back's: one to one when both
// hold as many, so a pair that reads back otherwise is looked into, and otherwise as alignChildren
// pairs them, with the children it cannot pair.
func pairChildren(parent string, written, back []*Node, ignore skip) ([][2]int, map[int]string) {
	if len(written) != len(back) {
		return alignChildren(parent, written, back, ignore)
	}
	pairs := make([][2]int, len(written))
	for index := range written {
		pairs[index] = [2]int{index, index}
	}
	return pairs, nil
}

// ownDifference is how a block differs from its read-back apart from its child blocks: its kind,
// its first differing attribute, and a textblock's text, each "" where they agree.
type ownDifference struct {
	kind, attribute, text, textDiffers string
}

func selfDifference(want, got *Node, ignore skip) ownDifference {
	if want.Type != got.Type {
		return ownDifference{kind: blockName(want.Type) + " reads back as " + blockName(got.Type)}
	}
	own := ownDifference{attribute: attributeReason(want, got, ignore)}
	if isTextblock(want.Type) && !ignore.text && !inlineEqual(want.Children, got.Children) {
		wanted, read := textContent(want), textContent(got)
		own.text = fmt.Sprintf("%s reads back holding %q, not %q", blockName(want.Type), read, wanted)
		own.textDiffers = differingSpan(wanted, read)
	}
	return own
}

// differingSpan is what wanted and read hold between the text they start and end with alike.
func differingSpan(wanted, read string) string {
	w, r := []rune(wanted), []rune(read)
	prefix := 0
	for prefix < len(w) && prefix < len(r) && w[prefix] == r[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(w)-prefix && suffix < len(r)-prefix && w[len(w)-1-suffix] == r[len(r)-1-suffix] {
		suffix++
	}
	return fmt.Sprintf("%q as %q", string(w[prefix:len(w)-suffix]), string(r[prefix:len(r)-suffix]))
}

// AgreeWithReadBack is doc with its document-level blocks first to last, which a write changed,
// holding what their markdown reads back as where the write left a shape the markdown cannot carry
// but reads back unambiguously, touching only what the write wrote. With halves, an empty
// paragraph without a block id that the renderer does not write goes, as Splice leaves the empty
// halves of the textblock a block replacement lands in. Each list and list item takes the spread
// its markdown reads back with, paired as far down as misreads pairs blocks, so one in a list that
// already reads back otherwise elsewhere takes it too, and so does an item the write left alone
// in a list it made loose, since a list's looseness is its whole markdown's. The exception is one
// before holds unchanged under its id whose spread already read back otherwise there (or when
// before does not read back): that spread is not the write's, and it is kept. Blocks outside
// first to last are doc's own.
func AgreeWithReadBack(before, doc *Node, first, last int, halves bool) *Node {
	out := &Node{Type: doc.Type, Attrs: doc.Attrs, Children: slices.Clone(doc.Children)}
	for index := first; index <= last; index++ {
		out.Children[index] = cloneNode(doc.Children[index])
	}
	if halves {
		kept := make([]*Node, 0, len(out.Children))
		written := writtenChildren(out)
		dropped := 0
		for index, child := range out.Children {
			changed := index >= first && index <= last
			if changed {
				dropUnwrittenHalves(child)
			}
			if changed && !slices.Contains(written, child) && blockIDOf(child) == "" {
				dropped++
				continue
			}
			kept = append(kept, child)
		}
		out.Children = kept
		last -= dropped
	}
	back, err := ReadBack(out)
	if err != nil {
		return out
	}
	held := map[string]*Node{}
	Walk(before, func(node *Node) bool {
		if id := blockIDOf(node); id != "" {
			held[id] = node
		}
		return true
	})
	stale, known := staleSpreads(before)
	spreadDifferences(out, back, first, last, func(node, read *Node) {
		id := blockIDOf(node)
		if previous, ok := held[id]; ok && previous.Equal(node) && (!known || stale[id]) {
			return
		}
		if node.Attrs == nil {
			node.Attrs = Attrs{}
		}
		node.Attrs["spread"] = read.Attrs["spread"]
	})
	return out
}

// staleSpreads is the block ids of before's lists and list items whose spread its markdown already
// reads back otherwise, and false when before does not read back.
func staleSpreads(before *Node) (map[string]bool, bool) {
	back, err := ReadBack(before)
	if err != nil {
		return nil, false
	}
	stale := map[string]bool{}
	spreadDifferences(before, back, 0, len(before.Children)-1, func(node, _ *Node) {
		stale[blockIDOf(node)] = true
	})
	return stale, true
}

// spreadDifferences pairs doc's document-level blocks first to last with back, doc's read-back, as
// misreads does, and calls visit with each list and list item under them and the block it pairs
// with when the two hold different spreads.
func spreadDifferences(doc, back *Node, first, last int, visit func(node, read *Node)) {
	pairs, _ := pairChildren("doc", writtenChildren(StripAnchorMarks(doc)), back.Children, spreadAside)
	writtenIndex := writtenIndexes(doc)
	for _, pair := range pairs {
		if index := writtenIndex[pair[0]]; index >= first && index <= last {
			blockSpreadDifferences(doc.Children[index], back.Children[pair[1]], visit)
		}
	}
}

// spreadAside compares blocks leaving their spread aside.
var spreadAside = skip{names: map[string]bool{"spread": true}}

func dropUnwrittenHalves(node *Node) {
	if isTextblock(node.Type) {
		return
	}
	written := writtenChildren(node)
	kept := node.Children[:0]
	for _, child := range node.Children {
		if !slices.Contains(written, child) && blockIDOf(child) == "" {
			continue
		}
		dropUnwrittenHalves(child)
		kept = append(kept, child)
	}
	node.Children = kept
}

// blockSpreadDifferences is spreadDifferences for node, which pairs with back, and the blocks it
// holds, visiting node before them.
func blockSpreadDifferences(node, back *Node, visit func(node, read *Node)) {
	if node.Type != back.Type || isTextblock(node.Type) {
		return
	}
	if (node.Type == "bullet_list" || node.Type == "ordered_list" || node.Type == "list_item") && node.Attrs["spread"] != back.Attrs["spread"] {
		visit(node, back)
	}
	written := writtenChildren(node)
	pairs, _ := pairChildren(node.Type, written, back.Children, spreadAside)
	for _, pair := range pairs {
		blockSpreadDifferences(written[pair[0]], back.Children[pair[1]], visit)
	}
}

func blockIDOf(node *Node) string {
	id, _ := node.Attrs[BlockIDAttr].(string)
	return id
}

// writtenChildren is node's children as its markdown writes them: an empty paragraph is not
// written, except where the parser reads one as the browser editor does (emptyParagraphFirst), as
// its container's only child and ahead of a list item's first block when that is not a paragraph.
func writtenChildren(node *Node) []*Node {
	only := len(node.Children) == 1
	written := make([]*Node, 0, len(node.Children))
	for index, child := range node.Children {
		readFirst := node.Type == "list_item" && index == 0 && len(node.Children) > 1 && node.Children[1].Type != "paragraph"
		if only || readFirst || child.Type != "paragraph" || len(child.Children) != 0 {
			written = append(written, child)
		}
	}
	return written
}

// writtenIndexes maps each of doc's written document-level blocks to its index in doc.
func writtenIndexes(doc *Node) []int {
	written := writtenChildren(doc)
	indexes := make([]int, 0, len(written))
	for index, child := range doc.Children {
		if slices.Contains(written, child) {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

// resyncWindow bounds how far alignBlocks looks past a block that reads back otherwise for the next
// pair that reads back as written.
const resyncWindow = 8

// alignBlocks pairs doc's written document-level blocks, anchor marks stripped, with back's, what
// its markdown reads back as (alignChildren), naming each block that reads back otherwise by its
// index in doc.
func alignBlocks(doc, back *Node, ignore skip) ([][2]int, map[int]string) {
	indexes := writtenIndexes(doc)
	pairs, unpaired := alignChildren(doc.Type, writtenChildren(doc), back.Children, ignore)
	misread := make(map[int]string, len(unpaired))
	for index, reason := range unpaired {
		misread[indexes[index]] = reason
	}
	return pairs, misread
}

// alignChildren pairs written blocks, a parent's children, with back's in order: each pair reads
// back as written
// (what ignore names aside), and each written block that does not is named by its index with how
// it reads back. Past a block that reads back otherwise the pairing resumes at the nearest pair
// that reads back as written, so two lists that meet are both named and the blocks after them pair
// again.
func alignChildren(parent string, written, back []*Node, ignore skip) ([][2]int, map[int]string) {
	var pairs [][2]int
	misread := map[int]string{}
	i, j := 0, 0
	for i < len(written) && j < len(back) {
		reason := readDifference(written[i], back[j], ignore)
		if reason == "" {
			pairs = append(pairs, [2]int{i, j})
			i, j = i+1, j+1
			continue
		}
		di, dj, found := resync(written, back, i, j, ignore)
		if !found {
			break
		}
		if di == 0 {
			// back holds blocks the document does not: the block before wrote them.
			misread[max(i-1, 0)] = reason
		}
		for k := i; k < i+di; k++ {
			misread[k] = reason
		}
		i, j = i+di, j+dj
	}
	for k := i; k < len(written); k++ {
		reason := blockName(written[k].Type) + " reads back as nothing"
		if j < len(back) {
			reason = readDifference(written[k], back[j], ignore)
		}
		misread[k] = reason
	}
	if i == len(written) && j < len(back) && len(written) > 0 {
		misread[len(written)-1] = endOf(parent) + " reads back as " + blockName(back[j].Type)
	}
	return pairs, misread
}

// resync finds the nearest blocks past written[i] and back[j] that read back as written, nearest by
// how many blocks it skips on both sides.
func resync(written, back []*Node, i, j int, ignore skip) (int, int, bool) {
	for distance := 1; distance <= 2*resyncWindow; distance++ {
		for di := range distance + 1 {
			dj := distance - di
			if di > resyncWindow || dj > resyncWindow || i+di >= len(written) || j+dj >= len(back) {
				continue
			}
			if readDifference(written[i+di], back[j+dj], ignore) == "" {
				return di, dj, true
			}
		}
	}
	return 0, 0, false
}

// attributeDrift is the attributes doc's blocks read back with other values where they read back
// holding the same blocks and text - but a task item's checked, which is its own item's: a task
// item emptied of its text is written as a plain empty item and reads back with checked none, and
// NewMisread tells a stale one from a new one by its block id, so the attribute drifting would hide
// every other task item emptied.
func attributeDrift(doc, back *Node) map[string]bool {
	drift := map[string]bool{}
	pairs, _ := alignBlocks(doc, back, skip{all: true})
	written := writtenChildren(doc)
	for _, pair := range pairs {
		collectDrift(written[pair[0]], back.Children[pair[1]], drift)
	}
	delete(drift, "checked")
	return drift
}

func collectDrift(node, back *Node, drift map[string]bool) {
	for {
		key, _, _, differs := attributeDifference(node, back, skip{names: drift})
		if !differs {
			break
		}
		drift[key] = true
	}
	children := back.Children
	if isTextblock(node.Type) {
		return
	}
	for index, child := range writtenChildren(node) {
		if index < len(children) {
			collectDrift(child, children[index], drift)
		}
	}
}

// readDifference names how got, what a block's markdown reads back as, differs from want, the
// block as written: its kind, its blocks, its text or an attribute; "" when it reads back as
// written. Attributes ignore names are not compared.
func readDifference(want, got *Node, ignore skip) string {
	own := selfDifference(want, got, ignore)
	if own.kind != "" {
		return own.kind
	}
	// A list's own attributes follow its items, which name a list the next one joins; any other
	// block's come first, which names a task item that reads back plain rather than its text.
	list := want.Type == "bullet_list" || want.Type == "ordered_list"
	if !list && own.attribute != "" {
		return own.attribute
	}
	if isTextblock(want.Type) {
		return own.text
	}
	written := writtenChildren(want)
	for index := range max(len(written), len(got.Children)) {
		switch {
		case index >= len(written):
			if list && got.Children[index].Type == "list_item" {
				// A list of the same kind right after it continues it.
				return blockName(want.Type) + " beside another of its kind reads back as one list"
			}
			return endOf(want.Type) + " reads back as " + blockName(got.Children[index].Type)
		case index >= len(got.Children):
			return blockName(written[index].Type) + " reads back as nothing"
		}
		if reason := readDifference(written[index], got.Children[index], ignore); reason != "" {
			return reason
		}
	}
	return own.attribute
}

// attributeReason names the first attribute want and got hold with different values, or is "".
func attributeReason(want, got *Node, ignore skip) string {
	key, wanted, read, differs := attributeDifference(want, got, ignore)
	switch {
	case !differs:
		return ""
	case want.Type == "list_item" && key == "checked" && read == nil:
		return "a task item reads back as a plain list item"
	default:
		return fmt.Sprintf("%s reads back with %s %v, not %v", blockName(want.Type), key, orNone(read), orNone(wanted))
	}
}

func orNone(value any) any {
	if value == nil {
		return "none"
	}
	return value
}

// attributeDifference is the first attribute, by name, that want and got hold with different
// values, block ids, server-owned attributes (tokenAttrs) and those ignore names aside.
func attributeDifference(want, got *Node, ignore skip) (string, any, any, bool) {
	if ignore.all {
		return "", nil, nil, false
	}
	wanted, read := writtenAttrs(want), tokenAttrs(got.Type, got.Attrs)
	keys := make([]string, 0, len(wanted)+len(read))
	for key := range wanted {
		keys = append(keys, key)
	}
	for key := range read {
		if _, ok := wanted[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !ignore.has(key) && !reflect.DeepEqual(wanted[key], read[key]) {
			return key, wanted[key], read[key], true
		}
	}
	return "", nil, nil, false
}

// writtenAttrs is the attributes node's markdown writes: tokenAttrs, with a table column that has
// no alignment - null, or the "none" this parser once stored - written unaligned (tableAlignment),
// which reads back with none.
func writtenAttrs(node *Node) Attrs {
	attrs := tokenAttrs(node.Type, node.Attrs)
	if node.Type == "table_header" || node.Type == "table_cell" {
		switch attrs["alignment"] {
		case "left", "center", "right":
		default:
			delete(attrs, "alignment")
		}
	}
	return attrs
}

// inlineEqual compares a textblock's inline content: each node's kind, text, attributes and marks,
// the marks in any order.
func inlineEqual(want, got []*Node) bool {
	if len(want) != len(got) {
		return false
	}
	for index := range want {
		w, g := want[index], got[index]
		if w.Type != g.Type || w.Text != g.Text || !reflect.DeepEqual(tokenAttrs(w.Type, w.Attrs), tokenAttrs(g.Type, g.Attrs)) || !marksEqual(sortedMarks(w.Marks), sortedMarks(g.Marks)) || !inlineEqual(w.Children, g.Children) {
			return false
		}
	}
	return true
}

func sortedMarks(marks []Mark) []Mark {
	sorted := slices.Clone(marks)
	sortMarks(sorted)
	return sorted
}
