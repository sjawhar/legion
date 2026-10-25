package pmdoc

// The pmdoc differential (gen/differential.sh) reads one corpus at two revisions. Each revision
// reads every document, renders what it read and what the browser editor's engine read, and reads
// each rendering back; the engine reads the same renderings; then the head judges the base against
// itself. Both tests skip unless the driver sets their inputs. The driver copies this file into the
// base revision, so it uses only what every revision since the write-path read-back check has.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// diffDump is one revision's record of one document.
type diffDump struct {
	ID string `json:"id"`
	// Tree is what Parse reads the document as, block ids dropped; Parse is its refusal.
	Tree  json.RawMessage `json:"tree,omitempty"`
	Parse string          `json:"parse,omitempty"`
	// Write is ParseForWrite's refusal: what an upload of the document is answered.
	Write string `json:"write,omitempty"`
	// Render is Tree's rendering, the markdown the document is stored as, and ReadsBack whether
	// Parse reads it back as Tree.
	Render    *string `json:"render"`
	RenderErr string  `json:"render_err,omitempty"`
	ReadsBack bool    `json:"reads_back"`
	// FixedPoint is whether Render is the document's markdown byte for byte: reading the
	// document and writing it back gives back what was read.
	FixedPoint bool `json:"fixed_point"`
	// EngineRender is the rendering of the engine's reading of the document, the tree a browser
	// holds, and EngineReadsBack whether Parse reads it back as that tree.
	EngineRender    *string `json:"engine_render"`
	EngineRenderErr string  `json:"engine_render_err,omitempty"`
	EngineReadsBack bool    `json:"engine_reads_back"`
	// Panics names each step that panicked, recovered or not.
	Panics []string `json:"panics,omitempty"`
}

// diffReading is the engine's reading of one markdown field: {pm} or {err}; nil where the field
// was null.
type diffReading struct {
	PM  json.RawMessage `json:"pm"`
	Err string          `json:"err"`
}

// diffLines reads a JSONL file one object at a time, in order.
type diffLines struct {
	name    string
	scanner *bufio.Scanner
	file    *os.File
}

func openDiffLines(t *testing.T, variable string) *diffLines {
	t.Helper()
	path := os.Getenv(variable)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("%s: %v", variable, err)
	}
	t.Cleanup(func() { file.Close() })
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<30)
	return &diffLines{name: path, scanner: scanner, file: file}
}

func (lines *diffLines) next(t *testing.T, into any) bool {
	t.Helper()
	for lines.scanner.Scan() {
		if strings.TrimSpace(lines.scanner.Text()) == "" {
			continue
		}
		if err := json.Unmarshal(lines.scanner.Bytes(), into); err != nil {
			t.Fatalf("%s: %v", lines.name, err)
		}
		return true
	}
	if err := lines.scanner.Err(); err != nil {
		t.Fatalf("%s: %v", lines.name, err)
	}
	return false
}

// diffStep runs one step, recording a panic, recovered into ErrPanic or not, under name.
func diffStep(dump *diffDump, name string, step func() error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: %v", ErrPanic, recovered)
		}
		if err != nil && errors.Is(err, ErrPanic) {
			dump.Panics = append(dump.Panics, name)
		}
	}()
	return step()
}

// diffTree decodes the engine's ProseMirror JSON as Go holds the document where the two differ only
// in what the browser editor does not store: a mark on a hard break or an image, which the engine's
// reader keeps and the editor's store (y-prosemirror) drops, as Go's schema does by holding marks on
// text alone. A link around one whole image is the image's link attribute, which both store; a link
// the engine holds as a mark on an image (one holding more than the bare image, or no address) is
// one such mark, which the editor shows until the document is reloaded (LEGION-365).
// TestDifferentialCompare counts both kinds apart. Each short table body row is padded to its
// header's width as Parse pads it, the documented canonicalisation. No revision is judged by these.
func diffTree(reading *diffReading) *Node {
	doc, _ := diffTreeLinkingImages(reading)
	return doc
}

// diffImageLinks is how the engine's reading of a document links its images. whole: an image
// carries a non-empty link attribute, a link around one whole image, which the editor stores.
// marked: an image carries a link mark, a shape whose link the editor drops as before (LEGION-365).
type diffImageLinks struct{ whole, marked bool }

// diffTreeLinkingImages is diffTree, also reporting how the engine's reading links its images.
func diffTreeLinkingImages(reading *diffReading) (*Node, diffImageLinks) {
	if reading == nil || reading.Err != "" || reading.PM == nil {
		return nil, diffImageLinks{}
	}
	var generic any
	if err := json.Unmarshal(reading.PM, &generic); err != nil {
		return nil, diffImageLinks{}
	}
	links := dropUnstoredMarks(generic)
	raw, err := json.Marshal(generic)
	if err != nil {
		return nil, links
	}
	doc, err := FromJSON(raw)
	if err != nil {
		return nil, links
	}
	padShortRows(doc)
	return doc, links
}

// dropUnstoredMarks removes, in decoded ProseMirror JSON, every mark on a hard break or an image (a
// mark on an image is never stored), and reports whether an image carried a non-empty link
// attribute and, apart, whether one carried a link mark it removed.
func dropUnstoredMarks(value any) (links diffImageLinks) {
	node, ok := value.(map[string]any)
	if !ok {
		return links
	}
	switch node["type"] {
	case "hardbreak":
		delete(node, "marks")
	case "image":
		attrs, _ := node["attrs"].(map[string]any)
		if link, _ := attrs["link"].(string); link != "" {
			links.whole = true
		}
		marks, _ := node["marks"].([]any)
		for _, mark := range marks {
			if m, ok := mark.(map[string]any); ok && m["type"] == "link" {
				links.marked = true
			}
		}
		delete(node, "marks")
	}
	children, _ := node["content"].([]any)
	for _, child := range children {
		inner := dropUnstoredMarks(child)
		links.whole = links.whole || inner.whole
		links.marked = links.marked || inner.marked
	}
	return links
}

// padShortRows gives every short body row of every table under node the cells Parse pads it with,
// each taking its header cell's alignment. A row holding no cell, which both parsers read a table
// with no body row as holding, stays empty.
func padShortRows(node *Node) {
	if node.Type != "table" || len(node.Children) == 0 {
		for _, child := range node.Children {
			padShortRows(child)
		}
		return
	}
	header := node.Children[0]
	for _, row := range node.Children[1:] {
		for column := len(row.Children); len(row.Children) > 0 && column < len(header.Children); column++ {
			row.Children = append(row.Children, &Node{
				Type:     "table_cell",
				Attrs:    Attrs{"alignment": header.Children[column].Attrs["alignment"], "colspan": 1, "colwidth": nil, "rowspan": 1},
				Children: []*Node{{Type: "paragraph"}},
			})
		}
	}
}

func withoutBlockIDs(doc *Node) *Node {
	out := cloneNode(doc)
	Walk(out, func(node *Node) bool {
		delete(node.Attrs, BlockIDAttr)
		return true
	})
	return out
}

func TestDifferentialDump(t *testing.T) {
	if os.Getenv("PMDOC_DIFF_OUT") == "" {
		t.Skip("PMDOC_DIFF_OUT unset; gen/differential.sh runs this")
	}
	sources := openDiffLines(t, "PMDOC_DIFF_CORPUS")
	engine := openDiffLines(t, "PMDOC_DIFF_ENGINE")
	out, err := os.Create(os.Getenv("PMDOC_DIFF_OUT"))
	if err != nil {
		t.Fatal(err)
	}
	// A typed block's id is written in its markdown, so each step mints ids from a counter
	// restarted for it, and two runs of one revision write the same bytes.
	t.Cleanup(func() { SetBlockIDGenerator(nil) })
	defer out.Close()
	writer := bufio.NewWriterSize(out, 1<<20)
	defer writer.Flush()
	for {
		var source struct{ ID, MD string }
		if !sources.next(t, &source) {
			break
		}
		var read struct {
			ID string       `json:"id"`
			MD *diffReading `json:"md"`
		}
		if !engine.next(t, &read) || read.ID != source.ID {
			t.Fatalf("the engine's readings do not follow the corpus at %q", source.ID)
		}
		dump := diffDump{ID: source.ID}
		var tree *Node
		SetBlockIDGenerator(counterBlockIDs())
		if err := diffStep(&dump, "parse", func() (err error) { tree, err = Parse(source.MD); return err }); err != nil {
			dump.Parse = err.Error()
		}
		SetBlockIDGenerator(counterBlockIDs())
		if err := diffStep(&dump, "write", func() error { _, err := ParseForWrite(source.MD, nil); return err }); err != nil {
			dump.Write = err.Error()
		}
		if tree != nil {
			dump.Tree, _ = withoutBlockIDs(tree).JSON()
			dump.Render, dump.RenderErr, dump.ReadsBack = diffRender(&dump, "render", tree)
			dump.FixedPoint = dump.Render != nil && *dump.Render == source.MD
		}
		if engineTree := diffTree(read.MD); engineTree != nil {
			dump.EngineRender, dump.EngineRenderErr, dump.EngineReadsBack = diffRender(&dump, "engine render", engineTree)
		}
		line, err := json.Marshal(dump)
		if err != nil {
			t.Fatal(err)
		}
		writer.Write(append(line, '\n'))
	}
}

// diffRender renders tree and reports whether Parse reads the rendering back as tree.
func diffRender(dump *diffDump, name string, tree *Node) (*string, string, bool) {
	var markdown string
	if err := diffStep(dump, name, func() (err error) { markdown, err = Render(tree); return err }); err != nil {
		return nil, err.Error(), false
	}
	var back *Node
	if err := diffStep(dump, name+" read back", func() (err error) { back, err = Parse(markdown); return err }); err != nil {
		return &markdown, "", false
	}
	return &markdown, "", StripAnchorMarks(tree).Equal(back)
}

// diffSide is what one revision does with one document.
type diffSide struct {
	dump diffDump
	// tree is what it reads the document as, nil where it refuses it.
	tree *Node
	// stores: the write is taken and the tree renders, with nothing panicking.
	stores bool
	// correct: it stores the document as the engine reads it, in markdown both readers read back
	// as that tree.
	correct bool
	// renders: the engine's tree renders into markdown both readers read back as that tree.
	renders bool
	// engineOnly: it stores a tree Go reads back as written and the engine reads otherwise.
	engineOnly bool
	// Why correct fails: the tree differs from the engine's reading, or the engine reads the
	// rendering back otherwise (Go's read-back is in dump).
	ReadsAsEngine, EngineReadsBack bool
}

// diffRenderings is the engine's reading of one revision's two renderings of a document.
type diffRenderings struct {
	ID           string       `json:"id"`
	Render       *diffReading `json:"render"`
	EngineRender *diffReading `json:"engine_render"`
}

func judge(dump diffDump, source *Node, reading diffRenderings) diffSide {
	side := diffSide{dump: dump}
	if dump.Tree != nil {
		side.tree, _ = FromJSON(dump.Tree)
	}
	tree := side.tree
	side.stores = dump.Write == "" && tree != nil && dump.Render != nil && len(dump.Panics) == 0
	engineBack := diffTree(reading.Render)
	side.EngineReadsBack = tree != nil && engineBack != nil && StripAnchorMarks(tree).Equal(engineBack)
	side.ReadsAsEngine = tree != nil && source != nil && tree.Equal(source)
	side.correct = side.stores && side.ReadsAsEngine && dump.ReadsBack && side.EngineReadsBack
	side.engineOnly = side.stores && dump.ReadsBack && !side.EngineReadsBack
	engineRenderBack := diffTree(reading.EngineRender)
	side.renders = source != nil && dump.EngineRender != nil && dump.EngineReadsBack && engineRenderBack != nil && source.Equal(engineRenderBack) && len(dump.Panics) == 0
	return side
}

// differs reports whether two renderings differ, one missing counting as a difference.
func differs(base, head *string) bool {
	return base == nil || head == nil || *base != *head
}

func TestDifferentialCompare(t *testing.T) {
	if os.Getenv("PMDOC_DIFF_BASE") == "" {
		t.Skip("PMDOC_DIFF_BASE unset; gen/differential.sh runs this")
	}
	engine := openDiffLines(t, "PMDOC_DIFF_ENGINE")
	dumps := [2]*diffLines{openDiffLines(t, "PMDOC_DIFF_BASE"), openDiffLines(t, "PMDOC_DIFF_HEAD")}
	readings := [2]*diffLines{openDiffLines(t, "PMDOC_DIFF_BASE_ENGINE"), openDiffLines(t, "PMDOC_DIFF_HEAD_ENGINE")}
	report, err := os.Create(os.Getenv("PMDOC_DIFF_REPORT"))
	if err != nil {
		t.Fatal(err)
	}
	defer report.Close()
	writer := bufio.NewWriter(report)
	defer writer.Flush()

	counts := map[string]int{}
	flag := func(class string, sides [2]diffSide) {
		counts[class]++
		line, _ := json.Marshal(map[string]any{
			"class": class, "id": sides[1].dump.ID,
			"base": sides[0].dump, "head": sides[1].dump,
			"base_reads_as_engine": sides[0].ReadsAsEngine, "base_engine_reads_back": sides[0].EngineReadsBack,
			"head_reads_as_engine": sides[1].ReadsAsEngine, "head_engine_reads_back": sides[1].EngineReadsBack,
		})
		writer.Write(append(line, '\n'))
	}
	documents := 0
	for {
		var read struct {
			ID string       `json:"id"`
			MD *diffReading `json:"md"`
		}
		if !engine.next(t, &read) {
			break
		}
		documents++
		source, links := diffTreeLinkingImages(read.MD)
		if source == nil {
			counts["engine cannot read"]++
		}
		if links.whole {
			counts["engine links a whole image"]++
		}
		if links.marked {
			counts["engine holds an image's link as a mark (LEGION-365)"]++
		}
		var sides [2]diffSide
		for index := range sides {
			var dump diffDump
			var reading diffRenderings
			if !dumps[index].next(t, &dump) || !readings[index].next(t, &reading) || dump.ID != read.ID || reading.ID != read.ID {
				t.Fatalf("the runs do not follow the corpus at %q", read.ID)
			}
			sides[index] = judge(dump, source, reading)
		}
		base, head := sides[0], sides[1]
		if base.stores && !head.stores {
			flag("refused write", sides)
		}
		// A link around one whole image is stored as the image's link attributes, as the editor
		// stores it; head keeps it when it stores the document as the engine reads it.
		if links.whole {
			if head.correct {
				counts["linked image kept"]++
			} else {
				flag("lost whole-image link", sides)
			}
		}
		// A link the engine holds as a mark on an image is stored without it at both revisions, as
		// the editor stores it (LEGION-365); these are the documents that loss reaches. Where it is
		// the document's only kind of linked image, head stores the document as base does.
		if links.marked && base.stores {
			counts["base stores an image under a link mark without the link (LEGION-365)"]++
		}
		if links.marked && head.stores {
			flag("head stores an image under a link mark without the link (LEGION-365)", sides)
		}
		if links.marked && !links.whole && base.stores &&
			(head.tree == nil || !base.tree.Equal(head.tree) || differs(base.dump.Render, head.dump.Render)) {
			flag("changed linked image", sides)
		}
		for index, name := range []string{"base", "head"} {
			side := sides[index]
			if side.correct {
				counts[name+" stores correctly"]++
			}
			if side.renders {
				counts[name+" renders the engine's tree correctly"]++
			}
			if side.engineOnly {
				counts[name+" engine-only read-back"]++
			}
			if len(side.dump.Panics) > 0 {
				counts[name+" panics"]++
			}
		}
		switch {
		case base.correct && !head.correct:
			flag("regression", sides)
		case !base.stores && head.stores && !head.correct:
			flag("misread", sides)
		case !base.correct && head.correct:
			flag("fixed", sides)
		}
		// A fixed point base kept of a document it read otherwise than the editor is a misread kept
		// stable; it is counted apart, and only one of a document base read as the editor gates.
		switch lost := base.dump.FixedPoint && !head.dump.FixedPoint; {
		case lost && base.ReadsAsEngine:
			flag("lost fixed point", sides)
		case lost:
			flag("lost fixed point of a misread", sides)
		}
		if base.correct && differs(base.dump.Render, head.dump.Render) {
			flag("churn", sides)
		}
		if base.renders && !head.renders {
			flag("engine-tree regression", sides)
		}
		if base.renders && differs(base.dump.EngineRender, head.dump.EngineRender) {
			flag("engine-tree churn", sides)
		}
		if !base.renders && head.renders {
			flag("engine-tree fixed", sides)
		}
		if len(head.dump.Panics) > 0 {
			flag("head panic", sides)
		}
		if !base.engineOnly && head.engineOnly {
			flag("new engine-only read-back", sides)
		}
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "documents: %d (the engine cannot read %d)\n", documents, counts["engine cannot read"])
	for _, line := range []struct{ label, key string }{
		{"regressions (base stores correctly, head does not)", "regression"},
		{"misreads (base refuses, head stores what reads back otherwise)", "misread"},
		{"churn (base stores correctly, head's bytes differ)", "churn"},
		{"engine-tree regressions (base renders the engine's tree correctly, head does not)", "engine-tree regression"},
		{"engine-tree churn (base renders the engine's tree correctly, head's bytes differ)", "engine-tree churn"},
		{"head panics (recovered or not)", "head panic"},
		{"lost fixed points (base reads the markdown as the engine and writes it back as read, head does not)", "lost fixed point"},
		{"head refuses a write the base takes", "refused write"},
		{"head loses a whole-image link (the engine links a whole image, head does not store it correctly)", "lost whole-image link"},
		{"head changes a linked image the base stored (a link mark on an image only; tree or bytes differ)", "changed linked image"},
	} {
		fmt.Fprintf(&summary, "GATE %-86s %d\n", line.label, counts[line.key])
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&summary, "  %s: %d\n", key, counts[key])
	}
	fmt.Print(summary.String())
	for _, gate := range []string{"regression", "misread", "churn", "engine-tree regression", "engine-tree churn", "head panic", "lost fixed point", "refused write", "lost whole-image link", "changed linked image"} {
		if counts[gate] > 0 {
			t.Errorf("%d %s; see %s", counts[gate], gate, os.Getenv("PMDOC_DIFF_REPORT"))
		}
	}
}
