package pmdoc

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// A typed block that an earlier build wrote before one of its attributes existed does not hold
// that attribute. It reads with the attribute's default, as the browser editor reads it, and the
// rendering writes that default. A tree built without the attribute validates and renders the same.
func TestATypedBlockMissingADefaultedAttributeReadsAsItsDefault(t *testing.T) {
	const want = ":::callout{#callout-1 kind=\"note\" title=\"\"}\nBody.\n:::\n"
	tree, err := Parse(":::callout{#callout-1 kind=\"warning\" title=\"Read this\"}\nBody.\n:::\n")
	if err != nil {
		t.Fatalf("parse callout: %v", err)
	}
	doc := crdt.New()
	fragment := doc.GetXmlFragment("prosemirror")
	if err := doc.TransactE(func(txn *crdt.Transaction) error {
		return Update(txn, fragment, tree)
	}); err != nil {
		t.Fatalf("write callout to Yjs: %v", err)
	}
	if err := doc.TransactE(func(txn *crdt.Transaction) error {
		callout := fragment.Children()[0].(*crdt.YXmlElement)
		callout.DeleteAttribute(txn, "kind")
		callout.DeleteAttribute(txn, "title")
		return nil
	}); err != nil {
		t.Fatalf("remove the callout's attributes: %v", err)
	}

	read, err := Read(fragment)
	if err != nil {
		t.Fatalf("read a callout without kind and title: %v", err)
	}
	if got := read.Children[0].Attrs; got["kind"] != "note" || got["title"] != "" {
		t.Fatalf("callout attributes = %#v, want kind note and an empty title", got)
	}
	if rendered, err := Render(read); err != nil || rendered != want {
		t.Fatalf("render the read callout = %q, %v; want %q", rendered, err, want)
	}

	built := &Node{Type: "doc", Children: []*Node{{
		Type:     "callout",
		Attrs:    Attrs{BlockIDAttr: "callout-1"},
		Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: "Body."}}}},
	}}}
	if err := built.Validate(); err != nil {
		t.Fatalf("validate a callout without kind and title: %v", err)
	}
	if rendered, err := Render(built); err != nil || rendered != want {
		t.Fatalf("render a callout without kind and title = %q, %v; want %q", rendered, err, want)
	}
}

// The manifest names every node and mark of the Proof schema, and this package's node and mark
// sets are the manifest's.
func TestTheManifestNamesEveryNodeAndMarkThisPackageKnows(t *testing.T) {
	wantNodes := []string{
		"blockquote", "bullet_list", "code_block", "doc", "footnote_definition", "footnote_reference",
		"frontmatter", "hardbreak", "heading", "hr", "html", "image", "list_item", "ordered_list",
		"paragraph", "table", "table_cell", "table_header", "table_header_row", "table_row", "text",
	}
	wantMarks := []string{
		"dispatchAsk", "emphasis", "inlineCode", "link", "proofApproved", "proofAuthored",
		"proofComment", "proofFlagged", "proofSuggestion", "strike_through", "strong",
	}
	var manifest struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
		Marks []struct {
			Name string `json:"name"`
		} `json:"marks"`
	}
	if err := json.Unmarshal(SchemaJSON(), &manifest); err != nil {
		t.Fatalf("decode the manifest: %v", err)
	}
	var nodes, marks []string
	for _, node := range manifest.Nodes {
		nodes = append(nodes, node.Name)
	}
	for _, mark := range manifest.Marks {
		marks = append(marks, mark.Name)
	}
	slices.Sort(nodes)
	slices.Sort(marks)
	if !slices.Equal(nodes, wantNodes) {
		t.Errorf("manifest nodes = %v, want %v", nodes, wantNodes)
	}
	if !slices.Equal(marks, wantMarks) {
		t.Errorf("manifest marks = %v, want %v", marks, wantMarks)
	}
	if got := slices.Sorted(maps.Keys(active.nodes)); !slices.Equal(got, wantNodes) {
		t.Errorf("node types = %v, want %v", got, wantNodes)
	}
	if got := slices.Sorted(maps.Keys(active.marks)); !slices.Equal(got, wantMarks) {
		t.Errorf("mark types = %v, want %v", got, wantMarks)
	}
}

// A newer build creates an element this build reads but may not create yet. This build reads a
// document holding it, from the live tree or from stored markdown, and edits around it. No write
// of this build creates one: markdown written in whole or in part, a retype, or a tree written
// over a document that does not hold the element. Each refusal tells the writer what to do. A tree
// written over an empty document as content a stored version already holds (a rebuild's seed)
// creates nothing, so it is written.
func TestAnElementThisBuildReadsButCannotCreateIsReadAndNeverCreated(t *testing.T) {
	const markdown = ":::future{#future-1}\nBody.\n:::\n"
	const instruction = "Remove it from your markdown, or write it again after the next release"
	refusesFuture := func(t *testing.T, what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), `typed block "future"`) || !strings.Contains(err.Error(), instruction) {
			t.Fatalf("%s: error = %v, want ErrSchema naming typed block \"future\" and saying %q", what, err, instruction)
		}
	}

	restoreCreatable, err := UseStagedBlockForTest("future", 2)
	if err != nil {
		t.Fatalf("stage the future block as creatable: %v", err)
	}
	t.Cleanup(restoreCreatable)
	if got := SchemaCreatable(); got != 2 {
		t.Fatalf("SchemaCreatable() where future is creatable = %d, want 2", got)
	}
	written, err := ParseForWrite(markdown, nil)
	if err != nil {
		t.Fatalf("a build that creates future writes it: %v", err)
	}
	doc := crdt.New()
	fragment := doc.GetXmlFragment("prosemirror")
	if err := doc.TransactE(func(txn *crdt.Transaction) error {
		return Update(txn, fragment, written)
	}); err != nil {
		t.Fatalf("a build that creates future stores it: %v", err)
	}

	restoreUncreatable, err := UseStagedBlockForTest("future", 3)
	if err != nil {
		t.Fatalf("stage the future block as not creatable: %v", err)
	}
	t.Cleanup(restoreUncreatable)
	if got, want := SchemaVersion(), 2; got != want {
		t.Fatalf("SchemaVersion() = %d, want %d", got, want)
	}
	if got := SchemaCreatable(); got != 1 {
		t.Fatalf("SchemaCreatable() where future is not creatable = %d, want 1", got)
	}
	versions, declared := Element{Kind: TypedBlockElement, Name: "future"}.Versions()
	if !declared || versions != (ElementVersions{Since: 2, CreatableSince: 3}) {
		t.Fatalf("future versions = %+v, %t; want since 2, creatable_since 3", versions, declared)
	}
	read, err := Read(fragment)
	if err != nil {
		t.Fatalf("read a stored tree holding future: %v", err)
	}
	if rendered, err := Render(read); err != nil || rendered != markdown {
		t.Fatalf("render a stored tree holding future = %q, %v; want %q", rendered, err, markdown)
	}
	if _, err := Parse(markdown); err != nil {
		t.Fatalf("read stored markdown holding future: %v", err)
	}
	if _, err := ParseForWrite(markdown+"\nMore.\n", read); err != nil {
		t.Fatalf("rewrite a document that holds future already: %v", err)
	}

	_, err = ParseForWrite(markdown, nil)
	refusesFuture(t, "write markdown holding future", err)
	_, err = ParseFragment(markdown, false, NewWriteBudget())
	refusesFuture(t, "write a fragment holding future", err)
	plain, err := Parse("Question?\n")
	if err != nil {
		t.Fatalf("parse a paragraph: %v", err)
	}
	_, err = RetypeBlock(plain, plain.Children[0].Attrs[BlockIDAttr].(string), "future", nil)
	refusesFuture(t, "retype a paragraph as future", err)
	empty := crdt.New()
	emptyFragment := empty.GetXmlFragment("prosemirror")
	err = empty.TransactE(func(txn *crdt.Transaction) error {
		return Update(txn, emptyFragment, read)
	})
	refusesFuture(t, "write a tree holding future over a document without it", err)
	reseeded := crdt.New()
	reseededFragment := reseeded.GetXmlFragment("prosemirror")
	if err := reseeded.TransactE(func(txn *crdt.Transaction) error {
		return UpdateHolding(txn, reseededFragment, read, read)
	}); err != nil {
		t.Fatalf("seed an empty document with a stored tree holding future: %v", err)
	}
	if seeded, err := Read(reseededFragment); err != nil {
		t.Fatalf("read the seeded document: %v", err)
	} else if rendered, err := Render(seeded); err != nil || rendered != markdown {
		t.Fatalf("the seeded document renders %q (%v), want the stored %q", rendered, err, markdown)
	}
}

// DocumentSince is the highest since among the declared elements a document holds: the schema
// version a client needs to represent all of it. A document of version 1 elements is 1, one that
// also holds an element read from version 2 is 2, and no document is 0. An element the manifest
// does not declare has no since, and counts for nothing.
func TestDocumentSinceIsTheHighestSinceTheDocumentHolds(t *testing.T) {
	restore, err := UseStagedBlockForTest("future", 2)
	if err != nil {
		t.Fatalf("stage the future block as creatable: %v", err)
	}
	t.Cleanup(restore)
	plain, err := Parse("Question?\n")
	if err != nil {
		t.Fatalf("parse a paragraph: %v", err)
	}
	future, err := Parse("Question?\n\n:::future{#future-1}\nBody.\n:::\n")
	if err != nil {
		t.Fatalf("parse a document holding future: %v", err)
	}
	unknown := &Node{Type: "doc", Children: []*Node{{Type: "paragraph"}, {Type: "unknown_node"}}}
	for _, c := range []struct {
		name string
		doc  *Node
		want int
	}{
		{name: "no document", doc: nil, want: 0},
		{name: "a paragraph", doc: plain, want: 1},
		{name: "a paragraph and a future block", doc: future, want: 2},
		{name: "a paragraph and an undeclared node", doc: unknown, want: 1},
	} {
		if got := DocumentSince(c.doc); got != c.want {
			t.Errorf("DocumentSince(%s) = %d, want %d", c.name, got, c.want)
		}
	}
}
