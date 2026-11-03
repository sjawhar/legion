package pmdoc

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// asWritten shares the input's nodes wherever it writes them as they are, and must give exactly
// what the pipeline it replaced gave: StripAnchorMarks, then each textblock's line breaks written
// (oracleWriteLineBreaksIn), then each U+0000 written as U+FFFD (oracleWriteNulsAsReplacement).
// The two oracle functions are that pipeline's, kept here as they were.

func oracleAsWritten(doc *Node) *Node {
	out := StripAnchorMarks(doc)
	oracleWriteLineBreaksIn(out)
	oracleWriteNulsAsReplacement(out)
	return out
}

func oracleWriteNulsAsReplacement(node *Node) {
	node.Text = nulAsReplacement(node.Text)
	if !IsTypedBlock(node.Type) {
		writeNulsInAttrs(node.Attrs)
	} else if id, ok := node.Attrs[BlockIDAttr].(string); ok && strings.IndexByte(id, 0) >= 0 {
		node.Attrs[BlockIDAttr] = nulAsReplacement(id)
	}
	for _, mark := range node.Marks {
		writeNulsInAttrs(mark.Attrs)
	}
	for _, child := range node.Children {
		oracleWriteNulsAsReplacement(child)
	}
}

func oracleWriteLineBreaksIn(node *Node) {
	switch node.Type {
	case "paragraph", "heading":
		node.Children = writtenLineBreaks(node.Children)
	case "code_block":
	default:
		for _, child := range node.Children {
			oracleWriteLineBreaksIn(child)
		}
	}
}

func writtenCommentMark(id string) Mark {
	return Mark{Type: "proofComment", Attrs: Attrs{"id": id, "by": "user:alice"}}
}

func writtenAskMark(id string) Mark {
	return Mark{Type: "dispatchAsk", Attrs: Attrs{"id": id, "by": "user:alice"}}
}

// writtenCases are documents holding every shape asWritten writes otherwise than it reads, and
// shapes it writes as they are beside them.
func writtenCases() map[string]*Node {
	text := func(value string, marks ...Mark) *Node { return &Node{Type: "text", Text: value, Marks: marks} }
	paragraph := func(children ...*Node) *Node { return &Node{Type: "paragraph", Children: children} }
	doc := func(children ...*Node) *Node { return &Node{Type: "doc", Children: children} }
	link := func(href string) Mark { return Mark{Type: "link", Attrs: Attrs{"href": href}} }
	strong, emphasis, code := Mark{Type: "strong"}, Mark{Type: "emphasis"}, Mark{Type: "inlineCode"}
	comment, ask := writtenCommentMark("c1"), writtenAskMark("a1")
	inlineBreak := func() *Node { return &Node{Type: "hardbreak", Attrs: Attrs{"isInline": true}} }
	item := func(children ...*Node) *Node {
		return &Node{Type: "list_item", Attrs: Attrs{"spread": false}, Children: children}
	}
	list := func(items ...*Node) *Node {
		return &Node{Type: "bullet_list", Attrs: Attrs{"spread": false}, Children: items}
	}
	callout := func(id string, children ...*Node) *Node {
		return &Node{Type: "callout", Attrs: Attrs{BlockIDAttr: id, "kind": "note", "title": ""}, Children: children}
	}
	nested := func() *Node {
		inner := list(
			item(paragraph(text("first "), text("it", comment), text("em")), callout("inner", paragraph(text("a\nb"), inlineBreak(), text("c")))),
			item(paragraph(text("second")), &Node{Type: "blockquote", Children: []*Node{paragraph(text("q"), text("uote", ask))}}),
		)
		return &Node{Type: "blockquote", Children: []*Node{
			paragraph(text("outer")),
			list(item(paragraph(text("top")), inner)),
			callout("outer", paragraph(text("in "), text("callout", comment)), list(item(paragraph(text("x\x00"), text("y", comment))))),
		}}
	}
	return map[string]*Node{
		"plain":                  doc(paragraph(text("plain text")), &Node{Type: "code_block", Children: []*Node{text("x := 1")}}),
		"anchor-only marks":      doc(paragraph(text("all of it", comment)), paragraph(text("a", ask, comment), text("b", comment))),
		"an anchor in a word":    doc(paragraph(text("Use snake_"), text("ca", comment), text("se here."))),
		"an anchor under strong": doc(paragraph(text("bo", strong), text("ld", strong, comment), text(" end"))),
		"equal runs":             doc(paragraph(text("a", strong), text("b", strong), text("c"), text("d"))),
		"empty text":             doc(paragraph(text("a", strong), text("", emphasis), text("b", strong)), paragraph(text(""))),
		"text with attributes":   doc(paragraph(&Node{Type: "text", Text: "x", Attrs: Attrs{"note": "y"}}, text("z"))),
		"empty attributes":       doc(paragraph(&Node{Type: "text", Text: "x", Attrs: Attrs{}, Marks: []Mark{}}), &Node{Type: "hr", Attrs: Attrs{}}),
		"hard breaks": doc(paragraph(text("a"), inlineBreak(), text("b"),
			&Node{Type: "hardbreak", Attrs: Attrs{"isInline": false}}, text("c"), &Node{Type: "hardbreak"}, text("d"))),
		"line feed before text":       doc(paragraph(text("one\ntwo"), text(" three", strong))),
		"line feed before whitespace": doc(paragraph(text("one\n  ")), paragraph(text("one\n"), text("  "))),
		"line feed in a heading":      doc(&Node{Type: "heading", Attrs: Attrs{"level": 2}, Children: []*Node{text("a\nb"), text("c", comment)}}),
		"line feed in inline code":    doc(paragraph(text("a\nb", code), text("c"))),
		"nul in text":                 doc(paragraph(text("a\x00b"), text("c\x00", strong))),
		"nul in a node attribute": doc(paragraph(&Node{Type: "image", Attrs: Attrs{"src": "https://example.com/a\x00b", "alt": "a\x00b", "title": "t"}}),
			&Node{Type: "code_block", Attrs: Attrs{"language": "g\x00o"}, Children: []*Node{text("a\x00b")}}),
		"nul in an href":          doc(paragraph(text("label", link("https://example.com/a\x00b"), strong))),
		"nul in a typed id":       doc(callout("c\x00d", paragraph(text("inside")))),
		"nul in a typed title":    doc(&Node{Type: "callout", Attrs: Attrs{BlockIDAttr: "c1", "kind": "note", "title": "t\x00t"}, Children: []*Node{paragraph(text("inside"))}}),
		"nul and replacement":     doc(paragraph(text("a", link("u\x00")), text("b", link("u\uFFFD")), text("c", link("u\uFFFD")), text("d", link("u\x00")))),
		"nul hrefs alike":         doc(paragraph(text("a", link("u\x00")), text("b", link("u\x00"), comment))),
		"split code block":        doc(&Node{Type: "code_block", Attrs: Attrs{"language": "go"}, Children: []*Node{text("x :"), text("= 1", comment), text("\ny\n")}}),
		"split frontmatter":       doc(&Node{Type: "frontmatter", Children: []*Node{text("a: "), text("b")}}, paragraph(text("x"))),
		"split table cell":        doc(writtenTable(text("he"), text("ad", comment))),
		"untouched beside one":    doc(paragraph(text("kept")), list(item(paragraph(text("kept")))), paragraph(text("ch"), text("anged", comment))),
		"nested":                  doc(nested()),
		"deeply nested nul":       doc(&Node{Type: "blockquote", Children: []*Node{list(item(paragraph(text("x")), callout("id", paragraph(text("a\x00"), text("b", ask)))))}}),
		"only an empty paragraph": doc(paragraph()),
	}
}

func writtenTable(header ...*Node) *Node {
	cell := func(kind string, children ...*Node) *Node {
		return &Node{Type: kind, Attrs: Attrs{"alignment": nil, "colspan": 1, "colwidth": nil, "rowspan": 1}, Children: []*Node{{Type: "paragraph", Children: children}}}
	}
	return &Node{Type: "table", Children: []*Node{
		{Type: "table_header_row", Children: []*Node{cell("table_header", header...)}},
		{Type: "table_row", Children: []*Node{cell("table_cell", &Node{Type: "text", Text: "body"})}},
	}}
}

// writtenCorpus is the markdown of every corpus document, concatenated and repeated until it
// holds at least size bytes.
func writtenCorpus(t *testing.T, size int) string {
	t.Helper()
	paths, err := filepath.Glob("testdata/corpus/*.md")
	if err != nil || len(paths) == 0 {
		t.Fatalf("corpus: %v (%d documents)", err, len(paths))
	}
	var corpus strings.Builder
	for _, path := range paths {
		markdown, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		corpus.Write(markdown)
		corpus.WriteString("\n\n")
	}
	once := corpus.String()
	return strings.Repeat(once, (size+len(once)-1)/len(once))
}

var writtenRealSize = struct {
	sync.Mutex
	docs map[int]*Node
}{docs: map[int]*Node{}}

// writtenRealSizeDoc is the corpus repeated to size bytes (writtenCorpus), parsed, with anchors,
// line feeds, hard breaks and U+0000s put in at fixed strides (addWrittenBranches). Tests share it
// and only read it. It is parsed as a rendering is read back (ParseRendering): the corpus is dense
// with syntax, and repeated past about 100 KB it makes more elements than Parse takes from one
// write (MaxDocumentElements).
func writtenRealSizeDoc(t *testing.T, size int) *Node {
	t.Helper()
	writtenRealSize.Lock()
	defer writtenRealSize.Unlock()
	if doc, ok := writtenRealSize.docs[size]; ok {
		return doc
	}
	SetBlockIDGenerator(counterBlockIDs())
	defer SetBlockIDGenerator(nil)
	doc, err := ParseRendering(writtenCorpus(t, size))
	if err != nil {
		t.Fatalf("parse the corpus repeated to %d bytes: %v", size, err)
	}
	addWrittenBranches(doc)
	if err := doc.Validate(); err != nil {
		t.Fatalf("the corpus repeated to %d bytes with anchors: %v", size, err)
	}
	writtenRealSize.docs[size] = doc
	return doc
}

// addWrittenBranches splits text runs inside their words with anchor marks, and puts in line
// feeds, hard breaks with and without isInline and U+0000s, each at its own fixed stride over the
// document's text runs.
func addWrittenBranches(doc *Node) {
	count := 0
	middle := func(value string, part int) int {
		at := len(value) * part / 3
		for at < len(value) && !utf8.RuneStart(value[at]) {
			at++
		}
		return at
	}
	anchored := func(marks []Mark, anchor Mark) []Mark {
		out := append(slices.Clone(marks), anchor)
		sortMarks(out)
		return out
	}
	var visit func(node *Node)
	visit = func(node *Node) {
		textblock := node.Type == "paragraph" || node.Type == "heading"
		if !textblock && node.Type != "code_block" {
			for _, child := range node.Children {
				visit(child)
			}
			return
		}
		children := make([]*Node, 0, len(node.Children))
		for _, child := range node.Children {
			if child.Type != "text" || len(child.Text) < 6 {
				children = append(children, child)
				continue
			}
			count++
			value := child.Text
			if textblock && count%7 == 0 {
				at := middle(value, 1)
				value = value[:at] + "\n" + value[at:]
			}
			if textblock && count%11 == 0 {
				value += "\n "
			}
			if count%17 == 0 {
				at := middle(value, 2)
				value = value[:at] + "\x00" + value[at:]
			}
			anchor := writtenCommentMark(fmt.Sprintf("c%d", count))
			if textblock && count%2 == 0 {
				anchor = writtenAskMark(fmt.Sprintf("a%d", count))
			}
			first, second := middle(value, 1), middle(value, 2)
			switch {
			case count%5 == 0 && 0 < first && first < second && second < len(value):
				children = append(children,
					&Node{Type: "text", Text: value[:first], Marks: child.Marks},
					&Node{Type: "text", Text: value[first:second], Marks: anchored(child.Marks, anchor)},
					&Node{Type: "text", Text: value[second:], Marks: child.Marks})
			case count%3 == 0 && 0 < first && first < len(value):
				children = append(children,
					&Node{Type: "text", Text: value[:first], Marks: child.Marks},
					&Node{Type: "text", Text: value[first:], Marks: anchored(child.Marks, anchor)})
			default:
				children = append(children, &Node{Type: "text", Text: value, Marks: child.Marks})
			}
			if textblock && count%9 == 0 {
				children = append(children, &Node{Type: "hardbreak", Attrs: Attrs{"isInline": true}}, &Node{Type: "text", Text: "after"})
			}
			if textblock && count%13 == 0 {
				children = append(children, &Node{Type: "hardbreak", Attrs: Attrs{"isInline": false}}, &Node{Type: "text", Text: "next"})
			}
		}
		node.Children = children
	}
	visit(doc)
}

func writtenJSON(t *testing.T, name string, tree *Node) []byte {
	t.Helper()
	encoded, err := tree.JSON()
	if err != nil {
		t.Fatalf("%s: JSON: %v", name, err)
	}
	return encoded
}

// firstDifference is where got and want first differ, with a little of each from there.
func firstDifference(got, want []byte) string {
	at := 0
	for at < len(got) && at < len(want) && got[at] == want[at] {
		at++
	}
	from := max(at-80, 0)
	return fmt.Sprintf("at byte %d:\n got  %q\n want %q", at, got[from:min(at+80, len(got))], want[from:min(at+80, len(want))])
}

func assertWrittenAsOracle(t *testing.T, name string, doc *Node) {
	t.Helper()
	want := oracleAsWritten(doc)
	got := asWritten(doc)
	if gotJSON, wantJSON := writtenJSON(t, name+" asWritten", got), writtenJSON(t, name+" oracle", want); !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("%s: asWritten differs from the old pipeline %s", name, firstDifference(gotJSON, wantJSON))
	}
	gotMarkdown, gotOffsets, gotErr := RenderWithBlockOffsets(doc)
	wantMarkdown, wantOffsets, wantErr := RenderWithBlockOffsets(want)
	if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
		t.Fatalf("%s: RenderWithBlockOffsets error %v, the old pipeline's %v", name, gotErr, wantErr)
	}
	if gotMarkdown != wantMarkdown {
		t.Fatalf("%s: RenderWithBlockOffsets markdown differs %s", name, firstDifference([]byte(gotMarkdown), []byte(wantMarkdown)))
	}
	if !reflect.DeepEqual(gotOffsets, wantOffsets) {
		t.Fatalf("%s: RenderWithBlockOffsets offsets differ:\n got  %v\n want %v", name, gotOffsets, wantOffsets)
	}
}

func TestAsWrittenIsTheOldPipelineOnEveryBranch(t *testing.T) {
	for name, doc := range writtenCases() {
		t.Run(name, func(t *testing.T) {
			assertWrittenAsOracle(t, name, doc)
		})
	}
}

func TestAsWrittenIsTheOldPipelineOnRealSizeDocuments(t *testing.T) {
	for _, size := range []int{150_000, 660_000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			doc := writtenRealSizeDoc(t, size)
			if _, _, err := RenderWithBlockOffsets(doc); err != nil {
				t.Fatalf("the corpus repeated to %d bytes does not render: %v", size, err)
			}
			assertWrittenAsOracle(t, fmt.Sprintf("corpus to %d bytes", size), doc)
		})
	}
}

// asWritten copies only what it writes otherwise: a document it writes as it is is the document
// itself, written with no allocation, and beside a changed run every untouched block is shared.
func TestAsWrittenSharesWhatItWritesAsItIs(t *testing.T) {
	cases := writtenCases()
	plain := cases["plain"]
	if got := asWritten(plain); got != plain {
		t.Fatal("asWritten copied a document it writes as it is")
	}
	if allocations := testing.AllocsPerRun(10, func() { asWritten(plain) }); allocations != 0 {
		t.Fatalf("asWritten allocated %v times for a document it writes as it is", allocations)
	}
	beside := cases["untouched beside one"]
	got := asWritten(beside)
	if got == beside || got.Children[0] != beside.Children[0] || got.Children[1] != beside.Children[1] || got.Children[2] == beside.Children[2] {
		t.Fatal("asWritten did not share exactly the blocks it writes as they are")
	}
}

// Nothing that renders a document or reads one back writes it: asWritten shares the input's nodes
// with the tree every renderer and read-back reads. The largest document is compared once, after
// every step, as its encoding costs as much as a read-back; the others after each step, which
// names the one that wrote.
func TestRenderAndReadBackLeaveTheirInputUntouched(t *testing.T) {
	check := func(t *testing.T, name string, tree *Node, eachStep bool) {
		t.Helper()
		before := writtenJSON(t, name, tree)
		variant := cloneNode(tree)
		variant.Children = append(variant.Children, &Node{Type: "paragraph", Children: []*Node{{Type: "text", Text: "a variant"}}})
		variantBefore := writtenJSON(t, name+" variant", variant)
		untouched := func(step string) {
			t.Helper()
			if after := writtenJSON(t, name, tree); !bytes.Equal(after, before) {
				t.Fatalf("%s: %s wrote its input %s", name, step, firstDifference(after, before))
			}
			if after := writtenJSON(t, name+" variant", variant); !bytes.Equal(after, variantBefore) {
				t.Fatalf("%s: %s wrote the variant %s", name, step, firstDifference(after, variantBefore))
			}
		}
		for _, step := range []struct {
			name string
			run  func()
		}{
			{"asWritten", func() { asWritten(tree) }},
			{"Render", func() { _, _ = Render(tree) }},
			{"RenderWithBlockOffsets", func() { _, _, _ = RenderWithBlockOffsets(tree) }},
			{"ReadBack", func() { _, _ = ReadBack(tree) }},
			{"NewMisread", func() { _, _ = NewMisread(tree, variant) }},
			{"NewMisread of the variant", func() { _, _ = NewMisread(variant, tree) }},
			{"RefuseMisreadDocument", func() { _ = RefuseMisreadDocument(tree) }},
		} {
			step.run()
			if eachStep {
				untouched(step.name)
			}
		}
		if !eachStep {
			untouched("one of asWritten, Render, RenderWithBlockOffsets, ReadBack, NewMisread and RefuseMisreadDocument")
		}
	}
	for name, doc := range writtenCases() {
		t.Run(name, func(t *testing.T) { check(t, name, doc, true) })
	}
	for _, size := range []int{150_000, 660_000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			check(t, fmt.Sprintf("corpus to %d bytes", size), writtenRealSizeDoc(t, size), size < 660_000)
		})
	}
}

// writtenMarks filters a bare URL's link out of the marks it writes in place (withoutMark), which
// is safe only because visibleMarks gives it a slice of its own: the node's marks stay as they are.
func TestWrittenMarksDoesNotAliasABareURLsMarks(t *testing.T) {
	const url = "https://example.com"
	node := &Node{Type: "text", Text: url, Marks: []Mark{{Type: "link", Attrs: Attrs{"href": url}}, {Type: "strong"}}}
	want := slices.Clone(node.Marks)
	written := writtenMarks(node, false, false)
	if len(written) != 1 || written[0].Type != "strong" {
		t.Fatalf("writtenMarks = %v, want the bare URL's link left out", written)
	}
	if !reflect.DeepEqual(node.Marks, want) {
		t.Fatalf("writtenMarks left the node's marks %v, want %v", node.Marks, want)
	}
}
