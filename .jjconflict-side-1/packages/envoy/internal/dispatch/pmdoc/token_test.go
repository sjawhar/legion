package pmdoc

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// tokenTestDocument is one markdown document a token test encodes, by name.
type tokenTestDocument struct {
	name, markdown string
}

// tokenTestDocuments is every corpus document and every fixture's markdown.
func tokenTestDocuments(t *testing.T) []tokenTestDocument {
	t.Helper()
	paths, err := filepath.Glob("testdata/corpus/*.md")
	if err != nil || len(paths) == 0 {
		t.Fatalf("corpus: %v (%d documents)", err, len(paths))
	}
	var documents []tokenTestDocument
	for _, path := range paths {
		markdown, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		documents = append(documents, tokenTestDocument{name: path, markdown: string(markdown)})
	}
	for _, fixture := range loadFixtures(t) {
		documents = append(documents, tokenTestDocument{name: "fixture " + fixture.Name, markdown: fixture.Markdown})
	}
	return documents
}

// markTokenTestTree gives tree a block id on every block, the marks a document read meets on every
// fifth text node (a comment, with its integer timestamp, and an authorship mark), and a block id
// on every third inline node that is not text, so the encoder meets marks and inline spans.
func markTokenTestTree(tree *Node) {
	EnsureBlockIDs(tree)
	texts, inlines := 0, 0
	Walk(tree, func(node *Node) bool {
		switch {
		case node.Type == "text":
			if texts%5 == 0 {
				node.Marks = append(node.Marks,
					Mark{Type: "proofComment", Attrs: Attrs{"id": fmt.Sprintf("comment-%d", texts), "by": "user:alice", "at": int64(1_700_000_000_000 + texts)}},
					Mark{Type: "proofAuthored", Attrs: Attrs{"by": "ai:<model> & co"}},
				)
			}
			texts++
		case isInlineNodeType(node.Type):
			if inlines%3 == 0 {
				if node.Attrs == nil {
					node.Attrs = Attrs{}
				}
				node.Attrs[BlockIDAttr] = fmt.Sprintf("inline-%d", inlines)
			}
			inlines++
		}
		return true
	})
}

// wantTokenJSON is what encoding/json makes of node's token representation built whole.
func wantTokenJSON(t *testing.T, node *Node) []byte {
	t.Helper()
	want, err := json.Marshal(tokenRepresentation(node))
	if err != nil {
		t.Fatalf("encode the representation: %v", err)
	}
	return want
}

// assertTokenSpans checks that the spans EncodeTokenJSON recorded into encoded are tree's block-id
// nodes in pre-order, each over its own representation.
func assertTokenSpans(t *testing.T, name string, tree *Node, encoded []byte, spans []TokenSpan) {
	t.Helper()
	var nodes []*Node
	Walk(tree, func(node *Node) bool {
		if id, _ := node.Attrs[BlockIDAttr].(string); id != "" {
			nodes = append(nodes, node)
		}
		return true
	})
	if len(spans) != len(nodes) {
		t.Fatalf("%s: %d spans, want one for each of %d block-id nodes", name, len(spans), len(nodes))
	}
	for index, node := range nodes {
		span := spans[index]
		if want := node.Attrs[BlockIDAttr].(string); span.ID != want {
			t.Fatalf("%s: span %d is block %q, want %q", name, index, span.ID, want)
		}
		if span.Start < 0 || span.End > len(encoded) || span.Start > span.End {
			t.Fatalf("%s: span %d (%q) is [%d, %d) of %d bytes", name, index, span.ID, span.Start, span.End, len(encoded))
		}
		if got, want := encoded[span.Start:span.End], wantTokenJSON(t, node); string(got) != string(want) {
			t.Fatalf("%s: span %d (%q) differs\n got: %s\nwant: %s", name, index, span.ID, got, want)
		}
	}
}

// largeTokenTestTree is one document standing for at least 600 KB of markdown, more than Parse
// reads: the corpus documents' blocks, frontmatter aside, joined over and over.
func largeTokenTestTree(t *testing.T, documents []tokenTestDocument) *Node {
	t.Helper()
	const size = 600 << 10
	type parsed struct {
		tree     *Node
		markdown int
	}
	var sources []parsed
	for _, document := range documents {
		if !strings.HasPrefix(document.name, "testdata/corpus/") {
			continue
		}
		tree, err := Parse(document.markdown)
		if err != nil {
			t.Fatalf("%s: parse: %v", document.name, err)
		}
		sources = append(sources, parsed{tree: tree, markdown: len(document.markdown)})
	}
	large := &Node{Type: "doc"}
	for stands := 0; stands < size; {
		for _, source := range sources {
			for _, child := range cloneNode(source.tree).Children {
				if child.Type != "frontmatter" {
					large.Children = append(large.Children, child)
				}
			}
			stands += source.markdown
		}
	}
	if err := large.Validate(); err != nil {
		t.Fatalf("the large document: %v", err)
	}
	return large
}

// A document read encodes the tree once and takes the document token from the whole encoding and
// each block token from its span, and callers hold those tokens across releases as edit
// preconditions: the whole encoding and every span are byte for byte what encoding/json makes of
// the representation built whole, for every document the corpus and the fixtures hold and for one
// far larger than Parse reads.
func TestEncodeTokenJSONIsWhatEncodingJSONMakesOfEveryDocument(t *testing.T) {
	documents := tokenTestDocuments(t)
	type tree struct {
		name string
		node *Node
	}
	var trees []tree
	for _, document := range documents {
		node, err := Parse(document.markdown)
		if err != nil {
			t.Fatalf("%s: parse: %v", document.name, err)
		}
		trees = append(trees, tree{name: document.name, node: node})
	}
	trees = append(trees, tree{name: "the large document", node: largeTokenTestTree(t, documents)})
	for _, tree := range trees {
		markTokenTestTree(tree.node)
		want := wantTokenJSON(t, tree.node)
		var spans []TokenSpan
		got, err := EncodeTokenJSON(tree.node, &spans)
		if err != nil {
			t.Fatalf("%s: %v", tree.name, err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s: encoding differs\n got: %s\nwant: %s", tree.name, got, want)
		}
		assertTokenSpans(t, tree.name, tree.node, got, spans)
	}
}

// tokenValueCase is one attribute a token encodes, named.
type tokenValueCase struct {
	name, key string
	value     any
}

type tokenTestStruct struct {
	A int
	B string
}

// tokenValueCases is every kind of value a token test encodes as an attribute.
func tokenValueCases() []tokenValueCase {
	var ascii strings.Builder
	for c := range 0x80 {
		ascii.WriteByte(byte(c))
	}
	cases := []tokenValueCase{
		{name: "HTML characters", key: "<k&ey>", value: "<a href=\"x\">&amp;</a>"},
		{name: "every ASCII byte", key: ascii.String(), value: ascii.String()},
		{name: "line and paragraph separators", key: "sep\u2028", value: "a\u2028b\u2029c"},
		{name: "a lone invalid byte", key: "bad\xff", value: "a\xffb"},
		{name: "a truncated 3-byte sequence", key: "cut\xe2\x82", value: "a\xe2\x82"},
		{name: "a replacement character", key: "k", value: "a\ufffdb"},
		{name: "a 4-byte rune", key: "\U0001F600", value: "smile \U0001F600"},
		{name: "1e20", key: "k", value: 1e20},
		{name: "1e21", key: "k", value: 1e21},
		{name: "1e-7", key: "k", value: 1e-7},
		{name: "1e-6", key: "k", value: 1e-6},
		{name: "0.1", key: "k", value: 0.1},
		{name: "123456789.125", key: "k", value: 123456789.125},
		{name: "negative zero", key: "k", value: math.Copysign(0, -1)},
		{name: "the largest int64", key: "k", value: int64(math.MaxInt64)},
		{name: "the largest int", key: "k", value: int(math.MaxInt)},
		{name: "a float32", key: "k", value: float32(0.1)},
		{name: "true", key: "k", value: true},
		{name: "nil inside an array", key: "k", value: []any{nil, "x", 1}},
		{name: "nil inside a nested map", key: "k", value: map[string]any{"gone": nil, "kept": int64(2), "nested": Attrs{"gone": nil}}},
		{name: "a nil nested map", key: "k", value: map[string]any(nil)},
		{name: "an empty Attrs", key: "k", value: Attrs{}},
		{name: "strings", key: "k", value: []string{"a", "<b>", "\xff"}},
		{name: "nil strings", key: "k", value: []string(nil)},
		{name: "a nil array", key: "k", value: []any(nil)},
		{name: "bytes", key: "k", value: []byte("bytes <&>")},
		{name: "a json.Number", key: "k", value: json.Number("12.50")},
		{name: "an invalid json.Number", key: "k", value: json.Number("abc")},
		{name: "an int32", key: "k", value: int32(-7)},
		{name: "a uint", key: "k", value: uint(7)},
		{name: "a struct", key: "k", value: tokenTestStruct{A: 1, B: "<b>"}},
		{name: "NaN", key: "k", value: math.NaN()},
		{name: "+Inf", key: "k", value: math.Inf(1)},
		{name: "a chan", key: "k", value: make(chan int)},
	}
	// crdt.Attributes is a named map the canonical form leaves to encoding/json, so a value inside
	// it keeps its own type.
	for _, base := range cases[:len(cases):len(cases)] {
		cases = append(cases, tokenValueCase{name: base.name + " inside crdt.Attributes inside an array", key: base.key, value: []any{crdt.Attributes{"v": base.value, "w": "<x>"}}})
	}
	return cases
}

// tokenValueTrees is a document holding attribute key: value on a node, and one holding it on a
// mark.
func tokenValueTrees(key string, value any) (onNode, onMark *Node) {
	onNode = &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Attrs: Attrs{key: value, BlockIDAttr: "p1"}, Children: []*Node{{Type: "text", Text: "x"}}}}}
	onMark = &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Attrs: Attrs{BlockIDAttr: "p1"}, Children: []*Node{{Type: "text", Text: "x", Marks: []Mark{
		{Type: "proofComment", Attrs: Attrs{"id": "c1", key: value}},
		{Type: "strong"},
	}}}}}}
	return onNode, onMark
}

// assertTokenJSONAsEncodingJSON checks that EncodeTokenJSON makes of tree what encoding/json makes
// of its representation: the same bytes, or an error of the same message and class.
func assertTokenJSONAsEncodingJSON(t *testing.T, name string, tree *Node) {
	t.Helper()
	if err := tree.Validate(); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var spans []TokenSpan
	got, gotErr := EncodeTokenJSON(tree, &spans)
	want, wantErr := json.Marshal(tokenRepresentation(tree))
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("%s: error %v, encoding/json's %v", name, gotErr, wantErr)
	}
	if wantErr != nil {
		if gotErr.Error() != wantErr.Error() || tokenErrorClass(gotErr) != tokenErrorClass(wantErr) {
			t.Fatalf("%s: error %T %q, encoding/json's %T %q", name, gotErr, gotErr, wantErr, wantErr)
		}
		if got != nil {
			t.Fatalf("%s: an error returned %q", name, got)
		}
		return
	}
	if string(got) != string(want) {
		t.Fatalf("%s: encoding differs\n got: %s\nwant: %s", name, got, want)
	}
	assertTokenSpans(t, name, tree, got, spans)
}

// tokenErrorClass names which of encoding/json's marshal errors err is.
func tokenErrorClass(err error) string {
	var value *json.UnsupportedValueError
	var typ *json.UnsupportedTypeError
	var marshaler *json.MarshalerError
	switch {
	case errors.As(err, &value):
		return "unsupported value"
	case errors.As(err, &typ):
		return "unsupported type"
	case errors.As(err, &marshaler):
		return "marshaler"
	default:
		return fmt.Sprintf("%T", err)
	}
}

// Every value an attribute can hold is encoded as encoding/json encodes it, byte for byte and
// error for error, on a node and on a mark alike: strings escaped as it escapes them, numbers in its
// shortest forms, the canonical form's conversions, and every other type left to it.
func TestEncodeTokenJSONEncodesEveryValueAsEncodingJSON(t *testing.T) {
	for _, test := range tokenValueCases() {
		onNode, onMark := tokenValueTrees(test.key, test.value)
		assertTokenJSONAsEncodingJSON(t, test.name+" on a node", onNode)
		assertTokenJSONAsEncodingJSON(t, test.name+" on a mark", onMark)
	}
}

// A node's marks are one token in whatever order they arrive.
func TestEncodeTokenJSONWritesMarksInTheOrderOfTheirEncodings(t *testing.T) {
	marks := []Mark{
		{Type: "proofComment", Attrs: Attrs{"id": "c2", "by": "user:bob", "at": int64(7)}},
		{Type: "strong"},
		{Type: "proofComment", Attrs: Attrs{"id": "c1", "by": "user:alice", "tags": []string{"<a>"}}},
	}
	reversed := []Mark{marks[2], marks[1], marks[0]}
	tree := func(marks []Mark) *Node {
		return &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: "x", Marks: marks}}}}}
	}
	forward, err := tree(marks).TokenJSON()
	if err != nil {
		t.Fatal(err)
	}
	backward, err := tree(reversed).TokenJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(forward) != string(backward) {
		t.Fatalf("marks in two orders give two tokens\n%s\n%s", forward, backward)
	}
	assertTokenJSONAsEncodingJSON(t, "three marks", tree(reversed))
}

// A typed block's server-owned attributes are the server's to change, so they are no part of its
// token.
func TestEncodeTokenJSONLeavesOutServerOwnedAttributes(t *testing.T) {
	open, err := Parse(":::ask{#decision urgency=\"high\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n")
	if err != nil {
		t.Fatal(err)
	}
	answered := cloneNode(open)
	answered.Children[0].Attrs["state"] = "answered"
	openToken, err := open.TokenJSON()
	if err != nil {
		t.Fatal(err)
	}
	answeredToken, err := answered.TokenJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(openToken) != string(answeredToken) || strings.Contains(string(openToken), "state") {
		t.Fatalf("the ask's state is part of its token\n  open: %s\nanswered: %s", openToken, answeredToken)
	}
	assertTokenJSONAsEncodingJSON(t, "an answered ask", answered)
}

// A node's attributes are encoded before its marks, so a node whose attributes and marks both
// fail reports its attributes' error, as encoding/json does.
func TestEncodeTokenJSONReportsTheAttributesErrorBeforeTheMarks(t *testing.T) {
	tree := &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{
		Type:  "text",
		Text:  "x",
		Attrs: Attrs{"weight": math.NaN()},
		Marks: []Mark{{Type: "proofComment", Attrs: Attrs{"id": "c1", "at": math.Inf(-1)}}, {Type: "strong", Attrs: Attrs{"c": make(chan int)}}},
	}}}}}
	_, err := tree.TokenJSON()
	var unsupported *json.UnsupportedValueError
	if !errors.As(err, &unsupported) || err.Error() != "json: unsupported value: NaN" {
		t.Fatalf("error = %T %v, want the attributes' NaN", err, err)
	}
	assertTokenJSONAsEncodingJSON(t, "attributes and marks that fail", tree)
}

// Whatever strings and number a small document holds, its token encoding is encoding/json's.
func FuzzTokenJSON(f *testing.F) {
	for _, test := range tokenValueCases() {
		switch value := test.value.(type) {
		case string:
			f.Add(value, test.key, value, test.key, value, 0.5)
		case float64:
			f.Add("text", test.key, "v", "by", "user:a", value)
		}
	}
	f.Fuzz(func(t *testing.T, text, key, value, markKey, markValue string, number float64) {
		tree := &Node{Type: "doc", Children: []*Node{
			{Type: "heading", Attrs: Attrs{"level": 2, BlockIDAttr: "h1"}, Children: []*Node{{Type: "text", Text: value}}},
			{Type: "paragraph", Attrs: Attrs{key: value, "number": number}, Children: []*Node{{
				Type:  "text",
				Text:  text,
				Marks: []Mark{{Type: "proofComment", Attrs: Attrs{"id": key, markKey: markValue}}, {Type: "proofAuthored", Attrs: Attrs{"by": markValue, "at": number}}},
			}}},
		}}
		if tree.Validate() != nil {
			t.Skip()
		}
		assertTokenJSONAsEncodingJSON(t, "a fuzzed document", tree)
	})
}
