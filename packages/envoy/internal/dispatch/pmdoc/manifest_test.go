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
	if got := slices.Sorted(maps.Keys(nodeTypes)); !slices.Equal(got, wantNodes) {
		t.Errorf("node types = %v, want %v", got, wantNodes)
	}
	if got := slices.Sorted(maps.Keys(markTypes)); !slices.Equal(got, wantMarks) {
		t.Errorf("mark types = %v, want %v", got, wantMarks)
	}
}

// useFutureBlock installs, until the test ends, the embedded manifest with one more typed block,
// "future", which builds read from schema version 2 and create from schema version creatableSince.
func useFutureBlock(t *testing.T, creatableSince int) {
	t.Helper()
	var manifest map[string]any
	if err := json.Unmarshal(SchemaJSON(), &manifest); err != nil {
		t.Fatalf("decode the manifest: %v", err)
	}
	manifest["types"] = append(manifest["types"].([]any), map[string]any{
		"name": "future", "content": "block+", "render": "host", "attributes": map[string]any{},
		"since": 2, "creatable_since": creatableSince,
	})
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode the manifest: %v", err)
	}
	restore, err := UseManifestForTest(encoded)
	if err != nil {
		t.Fatalf("install the manifest: %v", err)
	}
	t.Cleanup(restore)
}

// A newer build creates an element this build reads but may not create yet. This build reads a
// document holding it, from the live tree or from stored markdown, and edits around it. No write
// of this build creates one: markdown written in whole or in part, a retype, or a tree written
// over a document that does not hold the element.
func TestAnElementThisBuildReadsButCannotCreateIsReadAndNeverCreated(t *testing.T) {
	const markdown = ":::future{#future-1}\nBody.\n:::\n"
	refusesFuture := func(t *testing.T, what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), `typed block "future"`) {
			t.Fatalf("%s: error = %v, want ErrSchema naming typed block \"future\"", what, err)
		}
	}

	useFutureBlock(t, 2)
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

	useFutureBlock(t, 3)
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
}
