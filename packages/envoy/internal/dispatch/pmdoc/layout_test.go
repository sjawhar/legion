package pmdoc

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/reearth/ygo/crdt"
)

// spliceTally counts what a differential run of Layout.Splice saw.
type spliceTally struct {
	documents, trials, spliced int
	// textTrials and textSpliced count the text inserts and deletes in a paragraph, a heading or a
	// list, the edit people make most, which must splice in the large majority.
	textTrials, textSpliced int
	byKind                  map[string][2]int
}

func newSpliceTally() *spliceTally { return &spliceTally{byKind: map[string][2]int{}} }

func (tally *spliceTally) count(kind string, text, spliced bool) {
	tally.trials++
	counts := tally.byKind[kind]
	counts[0]++
	if spliced {
		tally.spliced++
		counts[1]++
	}
	tally.byKind[kind] = counts
	if text {
		tally.textTrials++
		if spliced {
			tally.textSpliced++
		}
	}
}

func (tally *spliceTally) log(t *testing.T, name string) {
	t.Helper()
	kinds := make([]string, 0, len(tally.byKind))
	for kind := range tally.byKind {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	var each strings.Builder
	for _, kind := range kinds {
		counts := tally.byKind[kind]
		fmt.Fprintf(&each, " %s %d/%d;", kind, counts[1], counts[0])
	}
	t.Logf("%s: %d documents, %d trials, splice ok %d (%.1f%%); paragraph/heading/list text edits ok %d/%d (%.1f%%); by edit:%s",
		name, tally.documents, tally.trials, tally.spliced, percent(tally.spliced, tally.trials),
		tally.textSpliced, tally.textTrials, percent(tally.textSpliced, tally.textTrials), each.String())
}

func percent(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return 100 * float64(part) / float64(whole)
}

// assertSpliceIsWhole splices next's touched blocks into markdown, layout's rendering of an earlier
// document, and, when Splice answers, asserts that it answers what RenderWithLayout renders and
// records for next, and that layout is left as it was. It reports whether Splice answered.
func assertSpliceIsWhole(t *testing.T, name, markdown string, layout *Layout, next *Node, touched []int) bool {
	t.Helper()
	before := cloneLayout(layout)
	got, gotLayout, ok := layout.Splice(markdown, touched, func(index int) (*Node, error) { return next.Children[index], nil })
	if !reflect.DeepEqual(layout, before) {
		t.Fatalf("%s: Splice modified the layout it was called on", name)
	}
	if !ok {
		return false
	}
	want, _, wantLayout, err := RenderWithLayout(next)
	if err != nil {
		t.Errorf("%s: Splice of %v answered, but the whole render fails: %v", name, touched, err)
		return true
	}
	if got != want {
		at := 0
		for at < len(got) && at < len(want) && got[at] == want[at] {
			at++
		}
		t.Errorf("%s: Splice of %v differs from the whole render at byte %d:\n splice: %q\n  whole: %q",
			name, touched, at, excerpt(got, at), excerpt(want, at))
		return true
	}
	if !reflect.DeepEqual(gotLayout, wantLayout) || layoutSize(gotLayout) != layoutSize(wantLayout) {
		t.Errorf("%s: Splice of %v answered layout %+v (%d bytes), the whole render records %+v (%d bytes)",
			name, touched, gotLayout, layoutSize(gotLayout), wantLayout, layoutSize(wantLayout))
	}
	return true
}

func excerpt(text string, at int) string {
	return text[max(0, at-60):min(len(text), at+60)]
}

func cloneLayout(layout *Layout) *Layout {
	if layout == nil {
		return nil
	}
	clone := *layout
	clone.ends = slices.Clone(layout.ends)
	clone.flags = slices.Clone(layout.flags)
	return &clone
}

// replaceBlocks is doc with each block in with standing in for the block at its index.
func replaceBlocks(doc *Node, with map[int]*Node) *Node {
	children := slices.Clone(doc.Children)
	for index, block := range with {
		children[index] = block
	}
	return &Node{Type: "doc", Attrs: doc.Attrs, Children: children}
}

// mutationKinds are the edits mutate makes to a block.
var mutationKinds = []string{"insert", "delete", "empty", "mark", "attribute", "anchor", "break"}

// insertions are the texts mutate inserts: every character that opens or closes markdown syntax,
// and the runs that open blocks, references or links.
var insertions = []string{
	"*", "_", "[", "]", "#", "|", "`", "<", ">", "!", "\\", "~", "-", "+", "=", ":", "(", ")", "&", "@",
	"---", "[^x]", "[^fn-1]", "**", "__", "~~", "- ", "1. ", "> ", ":::", "```", "[ ] ", "&amp;",
	"www.example.com", "https://example.com/a", "<div>", " ", "word", "x", "\n", "# ", "| a |",
}

// insertionsFor is insertions with a reference to each footnote label doc defines, which text
// shaped like that reference must escape.
func insertionsFor(doc *Node) []string {
	texts := slices.Clone(insertions)
	for _, definition := range collect(doc, func(n *Node) bool { return n.Type == "footnote_definition" }) {
		if label, ok := definition.Attrs["label"].(string); ok {
			texts = append(texts, "[^"+label+"]", "[^"+strings.ToUpper(label)+"]")
		}
	}
	return texts
}

// mutate makes one edit of kind to block, a copy the caller owns, at a place rng picks, inserting
// one of texts where it inserts text.
func mutate(block *Node, kind string, rng *rand.Rand, texts []string) {
	switch kind {
	case "insert":
		textblocks := collect(block, func(n *Node) bool {
			return n.Type == "paragraph" || n.Type == "heading" || n.Type == "code_block"
		})
		if len(textblocks) == 0 {
			return
		}
		textblock := textblocks[rng.IntN(len(textblocks))]
		insertion := texts[rng.IntN(len(texts))]
		var runs []*Node
		for _, child := range textblock.Children {
			if child.Type == "text" {
				runs = append(runs, child)
			}
		}
		if len(runs) == 0 {
			textblock.Children = append(textblock.Children, &Node{Type: "text", Text: insertion})
			return
		}
		text := runs[rng.IntN(len(runs))]
		at := runeBoundary(text.Text, rng)
		text.Text = text.Text[:at] + insertion + text.Text[at:]
	case "delete":
		parents := collect(block, func(n *Node) bool {
			for _, child := range n.Children {
				if child.Type == "text" {
					return true
				}
			}
			return false
		})
		if len(parents) == 0 {
			return
		}
		parent := parents[rng.IntN(len(parents))]
		var at []int
		for index, child := range parent.Children {
			if child.Type == "text" {
				at = append(at, index)
			}
		}
		index := at[rng.IntN(len(at))]
		text := parent.Children[index]
		runes := []rune(text.Text)
		from := rng.IntN(len(runes) + 1)
		to := min(len(runes), from+1+rng.IntN(5))
		text.Text = string(runes[:from]) + string(runes[to:])
		if text.Text == "" {
			parent.Children = slices.Delete(parent.Children, index, index+1)
		}
	case "empty":
		paragraphs := collect(block, func(n *Node) bool { return n.Type == "paragraph" })
		if len(paragraphs) == 0 {
			return
		}
		paragraph := paragraphs[0]
		if rng.IntN(2) == 0 {
			paragraph = paragraphs[rng.IntN(len(paragraphs))]
		}
		if len(paragraph.Children) > 0 {
			paragraph.Children = nil
		} else {
			paragraph.Children = []*Node{{Type: "text", Text: texts[rng.IntN(len(texts))] + "text"}}
		}
	case "mark", "anchor":
		text := randomInlineText(block, rng)
		if text == nil {
			return
		}
		var mark Mark
		if kind == "anchor" {
			mark = Mark{Type: "proofComment", Attrs: Attrs{"id": fmt.Sprintf("comment-%d", rng.IntN(1000))}}
		} else {
			mark = []Mark{
				{Type: "strong"}, {Type: "emphasis"}, {Type: "strike_through"}, {Type: "inlineCode"},
				{Type: "link", Attrs: Attrs{"href": "https://example.com/x", "title": nil}},
			}[rng.IntN(5)]
		}
		toggleMarkOnPart(text, mark, rng)
	case "attribute":
		changeAttribute(block, rng, texts)
	case "break":
		textblocks := collect(block, func(n *Node) bool { return n.Type == "paragraph" || n.Type == "heading" })
		if len(textblocks) == 0 {
			return
		}
		textblock := textblocks[rng.IntN(len(textblocks))]
		at := rng.IntN(len(textblock.Children) + 1)
		textblock.Children = slices.Insert(textblock.Children, at, &Node{Type: "hardbreak", Attrs: Attrs{"isInline": rng.IntN(2) == 0}})
	}
}

// collect lists every node under root, root included, that keep accepts, in document order.
func collect(root *Node, keep func(*Node) bool) []*Node {
	var found []*Node
	Walk(root, func(n *Node) bool {
		if keep(n) {
			found = append(found, n)
		}
		return true
	})
	return found
}

func runeBoundary(text string, rng *rand.Rand) int {
	var boundaries []int
	for at := range text {
		boundaries = append(boundaries, at)
	}
	boundaries = append(boundaries, len(text))
	return boundaries[rng.IntN(len(boundaries))]
}

// randomInlineText is a text node of a paragraph or heading under block, split so that the
// node answered is a part of it rng picks, or nil.
func randomInlineText(block *Node, rng *rand.Rand) *[]*Node {
	textblocks := collect(block, func(n *Node) bool {
		if n.Type != "paragraph" && n.Type != "heading" {
			return false
		}
		for _, child := range n.Children {
			if child.Type == "text" && child.Text != "" {
				return true
			}
		}
		return false
	})
	if len(textblocks) == 0 {
		return nil
	}
	return &textblocks[rng.IntN(len(textblocks))].Children
}

// toggleMarkOnPart removes mark from a text node of children that carries it, or adds it to a
// part of one that does not, splitting it there.
func toggleMarkOnPart(children *[]*Node, mark Mark, rng *rand.Rand) {
	var texts []int
	for index, child := range *children {
		if child.Type == "text" && child.Text != "" {
			texts = append(texts, index)
		}
	}
	index := texts[rng.IntN(len(texts))]
	text := (*children)[index]
	for position, held := range text.Marks {
		if held.Type == mark.Type {
			text.Marks = slices.Delete(slices.Clone(text.Marks), position, position+1)
			return
		}
	}
	runes := []rune(text.Text)
	from := rng.IntN(len(runes))
	to := from + 1 + rng.IntN(len(runes)-from)
	marked := slices.Clone(text.Marks)
	marked = append(marked, mark)
	sortMarks(marked)
	parts := []*Node{}
	if from > 0 {
		parts = append(parts, &Node{Type: "text", Text: string(runes[:from]), Marks: text.Marks})
	}
	parts = append(parts, &Node{Type: "text", Text: string(runes[from:to]), Marks: marked})
	if to < len(runes) {
		parts = append(parts, &Node{Type: "text", Text: string(runes[to:]), Marks: text.Marks})
	}
	*children = slices.Concat((*children)[:index], parts, (*children)[index+1:])
}

// changeAttribute changes one attribute of a node under block that rng picks.
func changeAttribute(block *Node, rng *rand.Rand, texts []string) {
	nodes := collect(block, func(n *Node) bool {
		switch n.Type {
		case "heading", "code_block", "ordered_list", "list_item", "table_cell", "table_header", "callout", "ask", "bullet_list":
			return true
		}
		return false
	})
	if len(nodes) == 0 {
		return
	}
	node := nodes[rng.IntN(len(nodes))]
	switch node.Type {
	case "heading":
		node.Attrs = withAttr(node.Attrs, "level", float64(1+rng.IntN(6)))
	case "code_block":
		node.Attrs = withAttr(node.Attrs, "language", []any{nil, "go", "", "a`b"}[rng.IntN(4)])
	case "ordered_list":
		node.Attrs = withAttr(node.Attrs, "order", float64(rng.IntN(12)))
	case "bullet_list":
		node.Attrs = withAttr(node.Attrs, "spread", rng.IntN(2) == 0)
	case "list_item":
		if rng.IntN(2) == 0 {
			node.Attrs = withAttr(node.Attrs, "checked", []any{nil, true, false}[rng.IntN(3)])
		} else {
			node.Attrs = withAttr(node.Attrs, "spread", rng.IntN(2) == 0)
		}
	case "table_cell", "table_header":
		if rng.IntN(2) == 0 {
			node.Attrs = withAttr(node.Attrs, "alignment", []any{nil, "left", "center", "right"}[rng.IntN(4)])
		} else {
			node.Attrs = withAttr(node.Attrs, "colspan", float64(1+rng.IntN(3)))
		}
	case "callout":
		node.Attrs = withAttr(node.Attrs, "title", texts[rng.IntN(len(texts))]+" title")
	case "ask":
		node.Attrs = withAttr(node.Attrs, BlockIDAttr, fmt.Sprintf("ask-changed-%d", rng.IntN(1000)))
	}
}

// isTextEdit reports whether kind on block is a text insert or delete in a paragraph, a heading or a
// list: the edits that must splice in the large majority.
func isTextEdit(kind string, block *Node) bool {
	return (kind == "insert" || kind == "delete") && (block.Type == "paragraph" || block.Type == "heading" || isList(block))
}

// spliceDifferential mutates each top-level block of doc perBlock times, one block at a time, and
// then rounds times a few blocks at once, asserting every Splice that answers (assertSpliceIsWhole).
func spliceDifferential(t *testing.T, name string, doc *Node, perBlock, rounds int, seed uint64, tally *spliceTally) {
	t.Helper()
	markdown, _, layout, err := RenderWithLayout(doc)
	if err != nil {
		return
	}
	tally.documents++
	rng := rand.New(rand.NewPCG(seed, uint64(len(markdown))))
	texts := insertionsFor(doc)
	for index, block := range doc.Children {
		for trial := range perBlock {
			kind := mutationKinds[(index+trial)%len(mutationKinds)]
			changed := cloneNode(block)
			mutate(changed, kind, rng, texts)
			next := replaceBlocks(doc, map[int]*Node{index: changed})
			ok := assertSpliceIsWhole(t, fmt.Sprintf("%s block %d %s #%d", name, index, kind, trial), markdown, layout, next, []int{index})
			tally.count(kind, isTextEdit(kind, block), ok)
		}
	}
	for round := range rounds {
		changes := map[int]*Node{}
		for range 1 + rng.IntN(4) {
			index := rng.IntN(len(doc.Children))
			changed := cloneNode(doc.Children[index])
			mutate(changed, mutationKinds[rng.IntN(len(mutationKinds))], rng, texts)
			changes[index] = changed
		}
		touched := slices.Sorted(maps.Keys(changes))
		ok := assertSpliceIsWhole(t, fmt.Sprintf("%s round %d", name, round), markdown, layout, replaceBlocks(doc, changes), touched)
		tally.count("several", false, ok)
	}
}

// TestIncrementalRenderMatchesTheWholeRenderOnTheFixtures runs the splice differential over every
// document of testdata: each fixture's tree as the browser editor reads it, and each corpus file as
// Parse reads it.
func TestIncrementalRenderMatchesTheWholeRenderOnTheFixtures(t *testing.T) {
	tally := newSpliceTally()
	perBlock, rounds := 21, 30
	if raceEnabled || testing.Short() {
		perBlock, rounds = 7, 10
	}
	for index, fx := range loadFixtures(t) {
		doc, err := FromJSON(fx.PMJSON)
		if err != nil {
			t.Fatalf("%s: %v", fx.Name, err)
		}
		spliceDifferential(t, "fixture "+fx.Name, doc, perBlock, rounds, uint64(index), tally)
	}
	paths, err := filepath.Glob(filepath.Join("testdata", "corpus", "*.md"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("corpus: %d files, %v", len(paths), err)
	}
	for index, path := range paths {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := Parse(string(source))
		if err != nil {
			continue
		}
		spliceDifferential(t, "corpus "+filepath.Base(path), doc, perBlock, rounds, uint64(1000+index), tally)
	}
	tally.log(t, "testdata")
	assertMostTextEditsSplice(t, tally)
}

func assertMostTextEditsSplice(t *testing.T, tally *spliceTally) {
	t.Helper()
	if tally.textTrials == 0 || percent(tally.textSpliced, tally.textTrials) < 80 {
		t.Errorf("paragraph, heading and list text edits spliced %d of %d, want at least 80%%", tally.textSpliced, tally.textTrials)
	}
}

// generatedDocument is a document of at least minBytes of markdown holding every kind of block:
// headings, paragraphs with every mark, footnote references and definitions, nested bullet and
// ordered lists and task lists, tables with and without spans, quotes, code, callouts, asks, and
// rules, none first.
func generatedDocument(t *testing.T, minBytes int) *Node {
	t.Helper()
	var source strings.Builder
	words := strings.Fields("the observer renders one block per keystroke and splices it into the markdown it keeps " +
		"whenever the guards hold which they do for most edits people make while typing in a document")
	sentence := func(section, length int) string {
		var text strings.Builder
		for index := range length {
			if index > 0 {
				text.WriteByte(' ')
			}
			text.WriteString(words[(section*7+index)%len(words)])
		}
		return text.String()
	}
	// Each section writes 14 or 15 top-level blocks of about 1 KB on average, as LINUX-129's 390 KB
	// hold 422.
	for section := 0; source.Len() < minBytes; section++ {
		fmt.Fprintf(&source, "# Section %d with *emphasis* and `code`\n\n", section)
		fmt.Fprintf(&source, "%s, **%s**, _%s_, ~~%s~~, a [link](https://example.com/%d \"title\"), www.example.com/%d, a reference[^fn-%d], <span>html</span>, and %s.\n\n",
			sentence(section, 260), sentence(section+1, 6), sentence(section+2, 5), sentence(section+3, 4), section, section, section, sentence(section+4, 200))
		fmt.Fprintf(&source, "- %s\n  - %s\n    1. %s\n    2. %s\n- [ ] %s\n- [x] %s\n", sentence(section, 40), sentence(section+1, 30), sentence(section+2, 25), sentence(section+3, 25), sentence(section+4, 20), sentence(section+5, 20))
		for item := range 6 {
			fmt.Fprintf(&source, "- %s\n", sentence(section+item, 25))
		}
		fmt.Fprintf(&source, "\n* %s\n* %s\n* %s\n\n", sentence(section+6, 40), sentence(section+7, 40), sentence(section+8, 40))
		fmt.Fprintf(&source, "%d. %s\n%d. %s\n   - %s\n   - %s\n%d. %s\n\n", section+1, sentence(section, 40), section+2, sentence(section+1, 40), sentence(section+2, 20), sentence(section+3, 20), section+3, sentence(section+4, 40))
		source.WriteString("| Name | Value | Note |\n| --- | :---: | ---: |\n")
		for row := range 16 {
			fmt.Fprintf(&source, "| %s | %d | %s |\n", sentence(section+row, 3), row, sentence(section+row+1, 8))
		}
		source.WriteString("\n")
		fmt.Fprintf(&source, "> %s\n>\n> - %s\n> - %s\n\n", sentence(section, 100), sentence(section+1, 20), sentence(section+2, 20))
		fmt.Fprintf(&source, "```go\nfunc section%d() string {\n", section)
		for line := range 12 {
			fmt.Fprintf(&source, "\tvalue%d := %q\n", line, sentence(section+line, 8))
		}
		source.WriteString("\treturn value0\n}\n```\n\n")
		fmt.Fprintf(&source, ":::callout{#callout-%d kind=\"note\" title=\"Note %d\"}\n%s\n\n%s\n:::\n\n", section, section, sentence(section, 80), sentence(section+1, 60))
		fmt.Fprintf(&source, ":::ask{#ask-%d urgency=\"high\" multiple=\"false\" state=\"open\"}\n%s?\n\n- REST: %s\n- gRPC: %s\n:::\n\n", section, sentence(section, 30), sentence(section+1, 20), sentence(section+2, 20))
		if section%3 == 1 {
			source.WriteString("---\n\n")
		}
		fmt.Fprintf(&source, "## Details %d\n\n%s.\n\n", section, sentence(section+5, 200))
		fmt.Fprintf(&source, "[^fn-%d]: %s.\n\n", section, sentence(section+6, 30))
	}
	doc, err := Parse(source.String())
	if err != nil {
		t.Fatalf("parse the generated document: %v", err)
	}
	// Markdown carries no span: give every fourth table a cell spanning columns and one spanning rows,
	// as a table pasted from HTML holds.
	tables := 0
	for _, block := range doc.Children {
		if block.Type != "table" {
			continue
		}
		if tables%4 == 0 {
			block.Children[1].Children[0].Attrs = withAttr(block.Children[1].Children[0].Attrs, "colspan", float64(2))
			block.Children[2].Children[1].Attrs = withAttr(block.Children[2].Children[1].Attrs, "rowspan", float64(3))
		}
		tables++
	}
	return doc
}

func withAttr(attrs Attrs, name string, value any) Attrs {
	attrs = cloneAttrs(attrs)
	if attrs == nil {
		attrs = Attrs{}
	}
	attrs[name] = value
	return attrs
}

// TestIncrementalRenderMatchesTheWholeRenderOnALargeDocument runs the splice differential over a
// generated document of at least 390 KB holding every kind of block, every top-level block mutated.
func TestIncrementalRenderMatchesTheWholeRenderOnALargeDocument(t *testing.T) {
	doc := generatedDocument(t, 400<<10)
	markdown, _, layout, err := RenderWithLayout(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(markdown) < 390_000 || !layout.incremental {
		t.Fatalf("generated document: %d bytes, incremental %v", len(markdown), layout.incremental)
	}
	kinds := map[string]bool{}
	Walk(doc, func(node *Node) bool {
		kinds[node.Type] = true
		return true
	})
	for _, kind := range []string{"heading", "paragraph", "bullet_list", "ordered_list", "table", "blockquote", "code_block", "callout", "ask", "hr", "footnote_definition", "footnote_reference"} {
		if !kinds[kind] {
			t.Fatalf("generated document holds no %s", kind)
		}
	}
	started := time.Now()
	tally := newSpliceTally()
	stride, rounds := 1, 20
	if raceEnabled || testing.Short() {
		stride, rounds = 16, 4
	}
	rng := rand.New(rand.NewPCG(644, 390))
	texts := insertionsFor(doc)
	for index := 0; index < len(doc.Children); index += stride {
		kind := mutationKinds[rng.IntN(len(mutationKinds))]
		changed := cloneNode(doc.Children[index])
		mutate(changed, kind, rng, texts)
		next := replaceBlocks(doc, map[int]*Node{index: changed})
		ok := assertSpliceIsWhole(t, fmt.Sprintf("large block %d %s", index, kind), markdown, layout, next, []int{index})
		tally.count(kind, isTextEdit(kind, doc.Children[index]), ok)
	}
	for round := range rounds {
		changes := map[int]*Node{}
		for range 2 + rng.IntN(6) {
			index := rng.IntN(len(doc.Children))
			changed := cloneNode(doc.Children[index])
			mutate(changed, []string{"insert", "delete"}[rng.IntN(2)], rng, texts)
			changes[index] = changed
		}
		touched := slices.Sorted(maps.Keys(changes))
		ok := assertSpliceIsWhole(t, fmt.Sprintf("large round %d", round), markdown, layout, replaceBlocks(doc, changes), touched)
		tally.count("several", false, ok)
	}
	tally.documents = 1
	tally.log(t, fmt.Sprintf("large document (%d bytes, %d top-level blocks, layout %d bytes, %s)", len(markdown), layout.Blocks(), layoutSize(layout), time.Since(started).Round(time.Millisecond)))
	assertMostTextEditsSplice(t, tally)
}

// TestIncrementalRenderCorpus runs the splice differential over every .md file of the directory
// DISPATCH_RENDER_CORPUS names (real documents, never committed), skipped where it is unset.
func TestIncrementalRenderCorpus(t *testing.T) {
	dir := os.Getenv("DISPATCH_RENDER_CORPUS")
	if dir == "" {
		t.Skip("DISPATCH_RENDER_CORPUS is unset")
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("DISPATCH_RENDER_CORPUS %s: %d .md files, %v", dir, len(paths), err)
	}
	tally := newSpliceTally()
	unparsed := 0
	for index, path := range paths {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := Parse(string(source))
		if err != nil {
			unparsed++
			t.Logf("%s: Parse: %v", filepath.Base(path), err)
			continue
		}
		spliceDifferential(t, filepath.Base(path), doc, 1, 10, uint64(index), tally)
	}
	t.Logf("%d files, %d not parsed", len(paths), unparsed)
	tally.log(t, "DISPATCH_RENDER_CORPUS")
	assertMostTextEditsSplice(t, tally)
}

// TestIncrementalRenderReadsBlocksAsTheWholeRead reads each fixture's Yjs document whole
// (ReadForRendering) and block by block (ReadBlockForRendering), and splices every block of it read
// so into the whole rendering, unchanged.
func TestIncrementalRenderReadsBlocksAsTheWholeRead(t *testing.T) {
	spliced := 0
	for _, fx := range loadFixtures(t) {
		doc := crdt.New()
		if err := crdt.ApplyUpdateV1(doc, fx.YjsUpdateV1, nil); err != nil {
			t.Fatalf("%s: apply update: %v", fx.Name, err)
		}
		fragment := doc.GetXmlFragment("prosemirror")
		whole, err := ReadForRendering(fragment)
		if err != nil {
			t.Fatalf("%s: %v", fx.Name, err)
		}
		children := fragment.Children()
		read := func(index int) (*Node, error) {
			element, ok := children[index].(*crdt.YXmlElement)
			if !ok {
				return nil, fmt.Errorf("top-level child %d is %T", index, children[index])
			}
			return ReadBlockForRendering(element)
		}
		for index := range children {
			block, err := read(index)
			if err != nil {
				t.Fatalf("%s block %d: %v", fx.Name, index, err)
			}
			if !block.EqualWithBlockIDs(whole.Children[index]) {
				t.Fatalf("%s block %d: ReadBlockForRendering differs from ReadForRendering", fx.Name, index)
			}
		}
		markdown, _, layout, err := RenderWithLayout(whole)
		if err != nil {
			continue
		}
		touched := make([]int, len(children))
		for index := range touched {
			touched[index] = index
		}
		got, gotLayout, ok := layout.Splice(markdown, touched, read)
		if !ok {
			continue
		}
		spliced++
		if got != markdown || !reflect.DeepEqual(gotLayout, layout) {
			t.Errorf("%s: splicing every block unchanged answers %q, layout %+v; want %q, layout %+v", fx.Name, got, gotLayout, markdown, layout)
		}
	}
	if spliced == 0 {
		t.Fatal("no fixture spliced")
	}
	t.Logf("%d fixtures spliced every block", spliced)
}

func paragraph(text string) *Node {
	if text == "" {
		return &Node{Type: "paragraph"}
	}
	return &Node{Type: "paragraph", Children: []*Node{{Type: "text", Text: text}}}
}

func bulletList(spread bool, items ...*Node) *Node {
	return &Node{Type: "bullet_list", Attrs: Attrs{"spread": spread}, Children: items}
}

func listItem(spread bool, children ...*Node) *Node {
	return &Node{Type: "list_item", Attrs: Attrs{"checked": nil, "label": "•", "listType": "bullet", "spread": spread}, Children: children}
}

func footnoteDefinition(label string, children ...*Node) *Node {
	return &Node{Type: "footnote_definition", Attrs: Attrs{"label": label}, Children: children}
}

// spliceCase renders doc whole, then splices changes into it, asserting that Splice answers as
// want says and, where it answers, what the whole render of the changed document is.
func spliceCase(t *testing.T, name string, doc *Node, changes map[int]*Node, want bool) {
	t.Helper()
	markdown, _, layout, err := RenderWithLayout(doc)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	touched := slices.Sorted(maps.Keys(changes))
	if got := assertSpliceIsWhole(t, name, markdown, layout, replaceBlocks(doc, changes), touched); got != want {
		t.Errorf("%s: Splice answered %v, want %v", name, got, want)
	}
}

// TestIncrementalRenderGuards holds one case for each guard of Layout.Splice, and the cases next
// to them that splice.
func TestIncrementalRenderGuards(t *testing.T) {
	reference := &Node{Type: "paragraph", Children: []*Node{{Type: "text", Text: "See "}, {Type: "footnote_reference", Attrs: Attrs{"label": "a"}}}}
	t.Run("G4 a footnote definition in the block", func(t *testing.T) {
		doc := &Node{Type: "doc", Children: []*Node{paragraph("[^b] stays text"), reference, footnoteDefinition("a", paragraph("Note."))}}
		// Relabelled, the definition makes the first paragraph's `[^b]` a reference to escape.
		spliceCase(t, "relabel", doc, map[int]*Node{2: footnoteDefinition("b", paragraph("Note."))}, false)
		spliceCase(t, "definition text", doc, map[int]*Node{2: footnoteDefinition("a", paragraph("Note, longer."))}, false)
		spliceCase(t, "definition into a paragraph's place", doc, map[int]*Node{0: &Node{Type: "blockquote", Children: []*Node{footnoteDefinition("b", paragraph("x"))}}}, false)
		spliceCase(t, "the reference's paragraph", doc, map[int]*Node{1: &Node{Type: "paragraph", Children: []*Node{{Type: "text", Text: "See [^a] and "}, {Type: "footnote_reference", Attrs: Attrs{"label": "a"}}}}}, true)
	})
	t.Run("G3 a rule first", func(t *testing.T) {
		doc := &Node{Type: "doc", Children: []*Node{{Type: "hr"}, paragraph("text"), bulletList(false, listItem(false, paragraph("item")))}}
		spliceCase(t, "after a first rule", doc, map[int]*Node{1: paragraph("text, more")}, false)
		doc = &Node{Type: "doc", Children: []*Node{paragraph("first"), paragraph("text"), bulletList(false, listItem(false, paragraph("item")))}}
		spliceCase(t, "a rule becoming first", doc, map[int]*Node{0: {Type: "hr"}}, false)
		spliceCase(t, "a rule after the first block", doc, map[int]*Node{1: {Type: "hr"}}, true)
	})
	t.Run("G5 an empty paragraph between two lists", func(t *testing.T) {
		doc := &Node{Type: "doc", Children: []*Node{
			bulletList(false, listItem(false, paragraph("one"))), paragraph(""), bulletList(false, listItem(false, paragraph("two"))),
		}}
		markdown, _, _, err := RenderWithLayout(doc)
		if err != nil || !strings.Contains(markdown, "* two") {
			t.Fatalf("the second list should take the other marker: %q, %v", markdown, err)
		}
		// Written, the paragraph stands between the lists: the second takes `-` again.
		spliceCase(t, "the empty paragraph written", doc, map[int]*Node{1: paragraph("between")}, false)
		spliceCase(t, "text in the second list", doc, map[int]*Node{2: bulletList(false, listItem(false, paragraph("two, more")))}, true)
		spliceCase(t, "text in the first list", doc, map[int]*Node{0: bulletList(false, listItem(false, paragraph("one"), bulletList(false, listItem(false, paragraph("nested")))))}, true)
		spliceCase(t, "a list becoming another kind", doc, map[int]*Node{2: {Type: "ordered_list", Attrs: Attrs{"order": float64(1), "spread": false}, Children: []*Node{listItem(false, paragraph("two"))}}}, false)
		spliceCase(t, "a written paragraph emptied", &Node{Type: "doc", Children: []*Node{
			bulletList(false, listItem(false, paragraph("one"))), paragraph("between"), bulletList(false, listItem(false, paragraph("two"))),
		}}, map[int]*Node{1: paragraph("")}, false)
	})
	t.Run("G6 the span budget", func(t *testing.T) {
		row := func(kind string, cells ...*Node) *Node { return &Node{Type: kind, Children: cells} }
		cell := func(kind, text string, colspan, rowspan int) *Node {
			return &Node{Type: kind, Attrs: Attrs{"alignment": nil, "colspan": float64(colspan), "colwidth": nil, "rowspan": float64(rowspan)}, Children: []*Node{paragraph(text)}}
		}
		table := func(colspan, rowspan, rows int) *Node {
			children := []*Node{row("table_header_row", cell("table_header", "h", 1, 1))}
			children = append(children, row("table_row", cell("table_cell", "spans", colspan, rowspan)))
			for range rows - 1 {
				children = append(children, row("table_row", cell("table_cell", "c", 1, 1)))
			}
			return &Node{Type: "table", Children: children}
		}
		// colspan 1000 and rowspan 120 charge 999 + 119 x 1000 cells, past maxSpanCells.
		exhausted := &Node{Type: "doc", Children: []*Node{table(1000, 120, 120), paragraph("text")}}
		spliceCase(t, "a render that spent the budget", exhausted, map[int]*Node{1: paragraph("text, more")}, false)
		// 999 + 98 x 1000 = 98,999 cells charged: the second table's 2,999 more reach the end.
		nearly := &Node{Type: "doc", Children: []*Node{table(1000, 99, 99), paragraph("text"), table(1, 1, 3)}}
		spliceCase(t, "spans past what is left", nearly, map[int]*Node{2: table(1000, 3, 3)}, false)
		spliceCase(t, "spans within what is left", nearly, map[int]*Node{2: table(2, 2, 3)}, true)
		spliceCase(t, "a block that charged spans", nearly, map[int]*Node{0: table(1000, 99, 99)}, false)
		spliceCase(t, "beside a block that charged spans", nearly, map[int]*Node{1: paragraph("text, more")}, true)
	})
	t.Run("G7 a blank empty code block in a typed block in a spread list", func(t *testing.T) {
		emptyCode := &Node{Type: "code_block", Attrs: Attrs{"language": nil}}
		callout := func(children ...*Node) *Node {
			return &Node{Type: "callout", Attrs: Attrs{BlockIDAttr: "c1", "kind": "note", "title": ""}, Children: children}
		}
		blank := bulletList(false, listItem(true, paragraph("item"), callout(emptyCode)))
		doc := &Node{Type: "doc", Children: []*Node{paragraph("text"), blank}}
		markdown, _, layout, err := RenderWithLayout(doc)
		if err != nil || layout.incremental {
			t.Fatalf("a blank empty code block leaves the layout incremental: %q, %v", markdown, err)
		}
		spliceCase(t, "in a document holding one", doc, map[int]*Node{0: paragraph("text, more")}, false)
		doc = &Node{Type: "doc", Children: []*Node{paragraph("text"), bulletList(false, listItem(true, paragraph("item"), callout(paragraph("body"))))}}
		spliceCase(t, "written by the block", doc, map[int]*Node{1: blank}, false)
	})
	t.Run("G2 a single block", func(t *testing.T) {
		spliceCase(t, "one paragraph", &Node{Type: "doc", Children: []*Node{paragraph("text")}}, map[int]*Node{0: paragraph("text, more")}, false)
		spliceCase(t, "one empty paragraph", &Node{Type: "doc", Children: []*Node{paragraph("")}}, map[int]*Node{0: paragraph("text")}, false)
	})
	t.Run("G1 and G8 what Splice is given", func(t *testing.T) {
		doc := &Node{Type: "doc", Children: []*Node{paragraph("one"), paragraph("two"), paragraph("three")}}
		markdown, _, layout, err := RenderWithLayout(doc)
		if err != nil {
			t.Fatal(err)
		}
		read := func(index int) (*Node, error) { return doc.Children[index], nil }
		for _, touched := range [][]int{{3}, {-1}, {1, 1}, {2, 1}} {
			if _, _, ok := layout.Splice(markdown, touched, read); ok {
				t.Errorf("Splice of %v answered", touched)
			}
		}
		if _, _, ok := layout.Splice(markdown+"x", []int{1}, read); ok {
			t.Error("Splice of markdown the layout was not recorded with answered")
		}
		bad := map[string]*Node{
			"inline at the top level": {Type: "text", Text: "x"},
			"an unknown node":         {Type: "nonsense"},
			"an invalid list":         {Type: "bullet_list"},
			"a render error":          {Type: "code_block", Children: []*Node{{Type: "text", Text: "x", Marks: []Mark{{Type: "strong"}}}}},
		}
		for name, block := range bad {
			spliceCase(t, name, doc, map[int]*Node{1: block}, false)
		}
		if _, _, ok := layout.Splice(markdown, []int{1}, func(int) (*Node, error) { return nil, fmt.Errorf("no read") }); ok {
			t.Error("Splice answered where read failed")
		}
		if _, _, ok := layout.Splice(markdown, []int{1}, func(int) (*Node, error) { panic("read panicked") }); ok {
			t.Error("Splice answered where read panicked")
		}
		var none *Layout
		if _, _, ok := none.Splice(markdown, []int{1}, read); ok {
			t.Error("a nil layout answered")
		}
		if got, gotLayout, ok := layout.Splice(markdown, nil, read); !ok || got != markdown || gotLayout != layout {
			t.Error("Splice of nothing touched does not answer the rendering it was given")
		}
	})
}

// TestIncrementalRenderLayoutRecordsAsksAndSize checks what a layout tells its caller: the blocks
// holding an ask, wherever it stands, the block count and the bytes it holds.
func TestIncrementalRenderLayoutRecordsAsksAndSize(t *testing.T) {
	doc, err := Parse("# Title\n\n:::ask{#a1 urgency=\"high\" multiple=\"false\" state=\"open\"}\nWhich?\n\n- A: a\n- B: b\n:::\n\nText.\n\n> :::ask{#a2 urgency=\"high\" multiple=\"false\" state=\"open\"}\n> Which?\n>\n> - A: a\n> - B: b\n> :::\n")
	if err != nil {
		t.Fatal(err)
	}
	markdown, offsets, layout, err := RenderWithLayout(doc)
	if err != nil {
		t.Fatal(err)
	}
	wantMarkdown, wantOffsets, err := RenderWithBlockOffsets(doc)
	if err != nil || markdown != wantMarkdown || !reflect.DeepEqual(offsets, wantOffsets) {
		t.Fatalf("RenderWithLayout differs from RenderWithBlockOffsets: %v", err)
	}
	if got := layout.AskBlocks(); !slices.Equal(got, []int{1, 3}) {
		t.Errorf("AskBlocks() = %v, want [1 3]", got)
	}
	// The blocks the layout flags are the blocks WalkAskBlocks finds an ask in: the agreement the
	// incremental path's ask ids rest on (holdsBlocks).
	var walked []int
	for index, block := range doc.Children {
		holdsAsk := false
		WalkAskBlocks(block, func(string) { holdsAsk = true })
		if holdsAsk {
			walked = append(walked, index)
		}
	}
	if got := layout.AskBlocks(); !slices.Equal(got, walked) {
		t.Errorf("AskBlocks() = %v, WalkAskBlocks finds an ask in blocks %v", got, walked)
	}
	if layout.Blocks() != 4 {
		t.Errorf("Blocks() = %d, want 4", layout.Blocks())
	}
	for index, end := range layout.ends {
		if start := layout.start(index); markdown[int(end):int(end)+1] != "\n" || index > 0 && markdown[start-2:start] != "\n\n" {
			t.Errorf("block %d ends at %d in %q", index, end, markdown)
		}
	}
	if size := layoutSize(layout); size < 4*5 || size > 4*5+64 {
		t.Errorf("layoutSize = %d, want 5 bytes per block and the layout's own fields", size)
	}
}

// layoutSize is how many bytes l holds: its own fields and its two slices' arrays.
func layoutSize(l *Layout) int {
	return int(unsafe.Sizeof(*l)) + cap(l.ends)*int(unsafe.Sizeof(uint32(0))) + cap(l.flags)
}
