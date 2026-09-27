package pmdoc

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
)

// skip names the attributes a comparison leaves aside, or all of them.
type skip struct {
	names map[string]bool
	all   bool
}

func (s skip) has(name string) bool { return s.all || s.names[name] }

// A document's markdown reads back as written when Parse gives back its blocks, their attributes and
// their text. Block ids, a typed block's server-owned attributes and anchor marks are not written,
// so they are not compared, and neither is an empty paragraph the renderer does not write
// (writtenChildren).

// ReadBack is what doc's markdown reads back as.
func ReadBack(doc *Node) (*Node, error) {
	markdown, err := Render(doc)
	if err != nil {
		return nil, err
	}
	return Parse(markdown)
}

// NewMisread names how after, which a write made from before by turning before's document-level
// blocks first to last into after's blocks first to lastAfter, reads back otherwise where before
// read back as written, or is "" when it does not. A block the write changed is judged whatever
// before held there; an unchanged one only when before read it back as written, and an attribute
// before already read back with another value (a table cell's alignment) is not the write's. A
// before whose markdown the parser refuses (a browser edit can leave one) gives nothing to judge
// against, and is "" whether or not after parses: the checks that read each changed block alone
// still refuse one the write leaves unreadable.
func NewMisread(before, after *Node, first, last, lastAfter int) string {
	backAfter, err := ReadBack(after)
	if err != nil {
		if _, beforeErr := ReadBack(before); beforeErr != nil {
			return ""
		}
		return err.Error()
	}
	written := StripAnchorMarks(after)
	if _, misread := alignBlocks(written, backAfter, skip{}); len(misread) == 0 {
		return ""
	}
	backBefore, err := ReadBack(before)
	if err != nil {
		return ""
	}
	previous := StripAnchorMarks(before)
	drift := skip{names: attributeDrift(previous, backBefore)}
	_, misreadBefore := alignBlocks(previous, backBefore, drift)
	_, misread := alignBlocks(written, backAfter, drift)
	indexes := make([]int, 0, len(misread))
	for index := range misread {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		if index >= first && index <= lastAfter {
			return misread[index]
		}
		old := index
		if index > lastAfter {
			old = index - (lastAfter - last)
		}
		if _, already := misreadBefore[old]; !already {
			return misread[index]
		}
	}
	return ""
}

// AgreeWithReadBack is doc with its document-level blocks first to last, which a write changed,
// holding what their markdown reads back as where the write left a shape the markdown cannot carry
// but reads back unambiguously. With halves, an empty paragraph without a block id that the
// renderer does not write goes, as Splice leaves the empty halves of the textblock a block
// replacement lands in. Each list's and list item's
// spread is the one its markdown reads back with. Blocks outside first to last are doc's own.
func AgreeWithReadBack(doc *Node, first, last int, halves bool) *Node {
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
	pairs, _ := alignBlocks(StripAnchorMarks(out), back, skip{names: map[string]bool{"spread": true}})
	writtenIndex := writtenIndexes(out)
	for _, pair := range pairs {
		if index := writtenIndex[pair[0]]; index >= first && index <= last {
			adoptSpread(out.Children[index], back.Children[pair[1]])
		}
	}
	return out
}

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

// adoptSpread gives node's lists and list items, which AgreeWithReadBack cloned, the spread their
// read-back holds, where the two hold the same blocks.
func adoptSpread(node, back *Node) {
	if (node.Type == "bullet_list" || node.Type == "ordered_list" || node.Type == "list_item") && node.Attrs["spread"] != back.Attrs["spread"] {
		if node.Attrs == nil {
			node.Attrs = Attrs{}
		}
		node.Attrs["spread"] = back.Attrs["spread"]
	}
	if isTextblock(node.Type) {
		return
	}
	written := writtenChildren(node)
	for index, child := range written {
		if index < len(back.Children) {
			adoptSpread(child, back.Children[index])
		}
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
// its markdown reads back as, in order: each pair reads back as written (attributes named in ignore
// aside), and each block that does not is named by its index in doc with how it reads back. Past a
// block that reads back otherwise the pairing resumes at the nearest pair that reads back as
// written, so two lists that meet are both named and the blocks after them pair again.
func alignBlocks(doc, back *Node, ignore skip) ([][2]int, map[int]string) {
	written := writtenChildren(doc)
	indexes := writtenIndexes(doc)
	var pairs [][2]int
	misread := map[int]string{}
	i, j := 0, 0
	for i < len(written) && j < len(back.Children) {
		reason := readDifference(written[i], back.Children[j], ignore)
		if reason == "" {
			pairs = append(pairs, [2]int{i, j})
			i, j = i+1, j+1
			continue
		}
		di, dj, found := resync(written, back.Children, i, j, ignore)
		if !found {
			break
		}
		if di == 0 {
			// back holds blocks the document does not: the block before wrote them.
			misread[indexes[max(i-1, 0)]] = reason
		}
		for k := i; k < i+di; k++ {
			misread[indexes[k]] = reason
		}
		i, j = i+di, j+dj
	}
	for k := i; k < len(written); k++ {
		reason := blockName(written[k].Type) + " reads back as nothing"
		if j < len(back.Children) {
			reason = readDifference(written[k], back.Children[j], ignore)
		}
		misread[indexes[k]] = reason
	}
	if i == len(written) && j < len(back.Children) && len(written) > 0 {
		misread[indexes[len(written)-1]] = endOf("doc") + " reads back as " + blockName(back.Children[j].Type)
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
// holding the same blocks and text.
func attributeDrift(doc, back *Node) map[string]bool {
	drift := map[string]bool{}
	pairs, _ := alignBlocks(doc, back, skip{all: true})
	written := writtenChildren(doc)
	for _, pair := range pairs {
		collectDrift(written[pair[0]], back.Children[pair[1]], drift)
	}
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
	if want.Type != got.Type {
		return blockName(want.Type) + " reads back as " + blockName(got.Type)
	}
	// A list's own attributes follow its items, which name a list the next one joins; any other
	// block's come first, which names a task item that reads back plain rather than its text.
	list := want.Type == "bullet_list" || want.Type == "ordered_list"
	if !list {
		if reason := attributeReason(want, got, ignore); reason != "" {
			return reason
		}
	}
	if isTextblock(want.Type) {
		if !inlineEqual(want.Children, got.Children) {
			return fmt.Sprintf("%s reads back holding %q, not %q", blockName(want.Type), textContent(got), textContent(want))
		}
		return ""
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
	if list {
		return attributeReason(want, got, ignore)
	}
	return ""
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
// no alignment written left (tableAlignment).
func writtenAttrs(node *Node) Attrs {
	attrs := tokenAttrs(node.Type, node.Attrs)
	if node.Type == "table_header" || node.Type == "table_cell" {
		if alignment, _ := attrs["alignment"].(string); alignment != "center" && alignment != "right" {
			attrs["alignment"] = "left"
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
