package pmdoc

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// Go-authored bytes read back as the tree, and the browser editor loads them as the node it would
// author the tree as, schema defaults applied, but for the null attributes whose default is not
// null: a table cell with no alignment holds "none", since that editor gives an absent alignment
// the default, left, and writes it back when the cell is edited, and a code block with no
// language and an image with no title hold "", that editor's own default.
func TestUpdateFromEmptyEqualsAuthoredByBrowser(t *testing.T) {
	for _, fx := range loadFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			want, err := FromJSON(fx.PMJSON)
			if err != nil {
				t.Fatal(err)
			}

			doc := crdt.New(crdt.WithClientID(1))
			frag := doc.GetXmlFragment("prosemirror")
			doc.Transact(func(txn *crdt.Transaction) {
				err = Update(txn, frag, want)
			})
			if err != nil {
				t.Fatal(err)
			}

			got, err := Read(frag)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(want) {
				t.Fatal("Read after Update differs from want")
			}

			pm := decodeWithYProsemirror(t, crdt.EncodeStateAsUpdateV1(doc, nil))
			if live := asTheLiveDocumentHoldsIt(want); !pm.Equal(live) {
				actual, _ := pm.JSON()
				expected, _ := live.JSON()
				t.Fatalf("y-prosemirror decodes Go-authored bytes differently\n got: %s\nwant: %s", actual, expected)
			}
		})
	}
}

// asTheLiveDocumentHoldsIt is doc with each table cell's null alignment written "none", and each
// code block's null language and image's null title "".
func asTheLiveDocumentHoldsIt(doc *Node) *Node {
	out := *doc
	set := func(name string, value any) {
		if doc.Attrs[name] != nil {
			return
		}
		attrs := Attrs{}
		for key, current := range out.Attrs {
			attrs[key] = current
		}
		attrs[name] = value
		out.Attrs = attrs
	}
	switch doc.Type {
	case "table_cell", "table_header":
		set("alignment", "none")
	case "code_block":
		set("language", "")
	case "image":
		set("title", "")
	}
	out.Children = make([]*Node, len(doc.Children))
	for index, child := range doc.Children {
		out.Children[index] = asTheLiveDocumentHoldsIt(child)
	}
	return &out
}

func TestUpdateIsNoOpForEqualTree(t *testing.T) {
	for _, fx := range loadFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			doc := crdt.New()
			if err := crdt.ApplyUpdateV1(doc, fx.YjsUpdateV1, nil); err != nil {
				t.Fatalf("apply browser update: %v", err)
			}
			frag := doc.GetXmlFragment("prosemirror")
			want, err := Read(frag)
			if err != nil {
				t.Fatal(err)
			}
			before := crdt.EncodeStateAsUpdateV1(doc, nil)

			doc.Transact(func(txn *crdt.Transaction) {
				err = Update(txn, frag, want)
			})
			if err != nil {
				t.Fatal(err)
			}
			after := crdt.EncodeStateAsUpdateV1(doc, nil)
			if !bytes.Equal(before, after) {
				t.Fatalf("Update wrote %d new bytes for an equal tree", len(after)-len(before))
			}
		})
	}
}

func TestUpdateEditsOnlyTheChangedRun(t *testing.T) {
	fx := fixtureNamed(t, "marks")
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, fx.YjsUpdateV1, nil); err != nil {
		t.Fatalf("apply browser update: %v", err)
	}
	frag := doc.GetXmlFragment("prosemirror")
	want, err := Read(frag)
	if err != nil {
		t.Fatal(err)
	}

	first := want.Children[0].Children[0]
	if !strings.Contains(first.Text, "sentence") {
		t.Fatalf("marks fixture first run = %q, want a run containing sentence", first.Text)
	}
	first.Text = strings.Replace(first.Text, "sentence", "line", 1)
	stateVector, err := crdt.DecodeStateVectorV1(crdt.EncodeStateVectorV1(doc))
	if err != nil {
		t.Fatalf("decode state vector: %v", err)
	}
	doc.Transact(func(txn *crdt.Transaction) {
		err = Update(txn, frag, want)
	})
	if err != nil {
		t.Fatal(err)
	}
	delta := crdt.EncodeStateAsUpdateV1(doc, stateVector)
	t.Logf("one-word update delta = %d bytes", len(delta))

	for _, mark := range []string{"proofComment", "dispatchAsk", "proofSuggestion"} {
		if bytes.Contains(delta, []byte(mark)) {
			t.Fatalf("update rewrote untouched %s formatting", mark)
		}
	}

	got, err := Read(frag)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(want) {
		t.Fatal("tree differs after edit")
	}
	if len(delta) > 64 {
		t.Fatalf("delta too large for a one-word edit: %d bytes", len(delta))
	}
	if !hasMark(got.Children[0], "proofComment") {
		t.Fatal("proofComment mark lost by an unrelated edit")
	}
}

func fixtureNamed(t *testing.T, name string) fixture {
	t.Helper()
	for _, fx := range loadFixtures(t) {
		if fx.Name == name {
			return fx
		}
	}
	t.Fatalf("fixture %q not found", name)
	return fixture{}
}

func hasMark(node *Node, markType string) bool {
	for _, mark := range node.Marks {
		if mark.Type == markType {
			return true
		}
	}
	for _, child := range node.Children {
		if hasMark(child, markType) {
			return true
		}
	}
	return false
}

func decodeWithYProsemirror(t *testing.T, update []byte) *Node {
	t.Helper()
	if _, err := exec.LookPath("bun"); err != nil {
		if os.Getenv("CI") == "" {
			t.Skip("bun is not on PATH; cross-language decoder is required in CI")
		}
		t.Fatalf("bun is required in CI: %v", err)
	}

	encoded := base64.StdEncoding.EncodeToString(update)
	output, stderr, err := runDecoder("decode.ts", encoded)
	if err != nil {
		t.Fatalf("run decode.ts: %v\nstderr:\n%s", err, stderr)
	}
	t.Log("y-prosemirror decoded with decode.ts")

	decoded, err := FromJSON(output)
	if err != nil {
		t.Fatalf("decode.ts output: %v\n%s", err, output)
	}
	return decoded
}

func runDecoder(script, encoded string) ([]byte, []byte, error) {
	cmd := exec.Command("bun", "run", script, encoded)
	cmd.Dir = filepath.Join("gen")
	output, err := cmd.Output()
	var stderr []byte
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		stderr = exitErr.Stderr
	}
	return output, stderr, err
}

// Every null attribute Go writes into the live document survives the browser editor: loaded as its
// sync plugin loads it, with a header cell, a body cell and a code block typed into and an image's
// alt text edited, written back as that plugin writes them, the document reads back with each
// attribute as written. The plugin writes an edited node back with the attributes the editor
// holds, schema defaults included: an unaligned column comes back left unless the live document
// holds "none", and a code block with no language and an image with no title come back holding ""
// unless the read gives "" back as null (liveNulls).
func TestNullAttributesSurviveABrowserEdit(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		if os.Getenv("CI") == "" {
			t.Skip("bun is not on PATH; the browser editor's sync is required in CI")
		}
		t.Fatalf("bun is required in CI: %v", err)
	}
	written, err := Parse("| a | b | c |\n| --- | :---: | ---: |\n| d | e | f |\n\n```\ncode\n```\n\n![alt](src.png) tail\n")
	if err != nil {
		t.Fatal(err)
	}
	doc := crdt.New(crdt.WithClientID(1))
	frag := doc.GetXmlFragment("prosemirror")
	doc.Transact(func(txn *crdt.Transaction) {
		err = Update(txn, frag, written)
	})
	if err != nil {
		t.Fatal(err)
	}

	output, stderr, err := runDecoder("edit-blocks.ts", base64.StdEncoding.EncodeToString(crdt.EncodeStateAsUpdateV1(doc, nil)))
	if err != nil {
		t.Fatalf("run edit-blocks.ts: %v\nstderr:\n%s", err, stderr)
	}
	update, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(output)))
	if err != nil {
		t.Fatalf("edit-blocks.ts output: %v\n%s", err, output)
	}
	edited := crdt.New()
	if err := crdt.ApplyUpdateV1(edited, update, nil); err != nil {
		t.Fatal(err)
	}
	tree, err := Read(edited.GetXmlFragment("prosemirror"))
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := Render(tree)
	if err != nil {
		t.Fatal(err)
	}
	if want := "| ax | b | c |\n| --- | :---: | ---: |\n| dy | e | f |\n\n```\ncodez\n```\n\n![alt2](src.png) tail\n"; markdown != want {
		t.Fatalf("after the browser editor's edits, the document renders %q, want %q", markdown, want)
	}
	for _, attr := range []struct{ node, name string }{{"code_block", "language"}, {"image", "title"}} {
		node := firstNodeOfType(tree, attr.node)
		if node == nil {
			t.Fatalf("no %s in the edited tree", attr.node)
		}
		if value := node.Attrs[attr.name]; value != nil {
			t.Errorf("after the browser editor's edit, the %s's %s reads back %#v, want null", attr.node, attr.name, value)
		}
	}
}

// firstNodeOfType is the first node of type in node's tree, in document order, or nil.
func firstNodeOfType(node *Node, nodeType string) *Node {
	if node.Type == nodeType {
		return node
	}
	for _, child := range node.Children {
		if found := firstNodeOfType(child, nodeType); found != nil {
			return found
		}
	}
	return nil
}

func TestUpdateAppliesMarksToInsertedTextAtRunBoundary(t *testing.T) {
	seed, err := Parse("A **B**\n")
	if err != nil {
		t.Fatal(err)
	}
	want, err := Parse("A **XB**\n")
	if err != nil {
		t.Fatal(err)
	}
	doc := crdt.New()
	frag := doc.GetXmlFragment("prosemirror")
	doc.Transact(func(txn *crdt.Transaction) {
		err = Update(txn, frag, seed)
	})
	if err != nil {
		t.Fatal(err)
	}
	doc.Transact(func(txn *crdt.Transaction) {
		err = Update(txn, frag, want)
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Read(frag)
	if err != nil {
		t.Fatal(err)
	}
	markdown, err := Render(got)
	if err != nil {
		t.Fatal(err)
	}
	if markdown != "A **XB**\n" {
		t.Fatalf("Update() rendered %q, want %q", markdown, "A **XB**\n")
	}
	if decoded := decodeWithYProsemirror(t, crdt.EncodeStateAsUpdateV1(doc, nil)); !decoded.Equal(want) {
		t.Fatal("y-prosemirror decoded tree differs after marked insertion")
	}
}

func TestUpdatePreservesMarksAcrossAstralEdit(t *testing.T) {
	seed, err := Parse("😀 before **marked**\n")
	if err != nil {
		t.Fatal(err)
	}
	want, err := Parse("😀 after **marked**\n")
	if err != nil {
		t.Fatal(err)
	}
	doc := crdt.New()
	frag := doc.GetXmlFragment("prosemirror")
	doc.Transact(func(txn *crdt.Transaction) {
		err = Update(txn, frag, seed)
	})
	if err != nil {
		t.Fatal(err)
	}
	doc.Transact(func(txn *crdt.Transaction) {
		err = Update(txn, frag, want)
	})
	if err != nil {
		t.Fatal(err)
	}
	if decoded := decodeWithYProsemirror(t, crdt.EncodeStateAsUpdateV1(doc, nil)); !decoded.Equal(want) {
		t.Fatal("y-prosemirror decoded tree differs after astral edit")
	}
}
