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

// Go-authored bytes read back as the tree, and y-prosemirror decodes them as the browser editor
// would author the tree, but for a table cell with no alignment: it holds "none", since the
// browser editor gives a cell whose alignment attribute is absent its schema's default, left, and
// writes that back when the cell is edited.
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
			if live := unalignedCellsAsNone(want); !pm.Equal(live) {
				actual, _ := pm.JSON()
				expected, _ := live.JSON()
				t.Fatalf("y-prosemirror decodes Go-authored bytes differently\n got: %s\nwant: %s", actual, expected)
			}
		})
	}
}

// unalignedCellsAsNone is doc with each table cell's null alignment written "none".
func unalignedCellsAsNone(doc *Node) *Node {
	out := *doc
	if (doc.Type == "table_cell" || doc.Type == "table_header") && doc.Attrs["alignment"] == nil {
		out.Attrs = Attrs{}
		for key, value := range doc.Attrs {
			out.Attrs[key] = value
		}
		out.Attrs["alignment"] = "none"
	}
	out.Children = make([]*Node, len(doc.Children))
	for index, child := range doc.Children {
		out.Children[index] = unalignedCellsAsNone(child)
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

// A table Go writes into the live document keeps each column's alignment through the browser
// editor: loaded as its sync plugin loads it, a header cell and a body cell typed into and written
// back as that plugin writes them, it reads back with every column as written. The editor gives a
// cell whose alignment attribute is absent its schema's default, left, so an unaligned column
// written that way came back left-aligned from its first edit.
func TestTableAlignmentSurvivesABrowserCellEdit(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		if os.Getenv("CI") == "" {
			t.Skip("bun is not on PATH; the browser editor's sync is required in CI")
		}
		t.Fatalf("bun is required in CI: %v", err)
	}
	written, err := Parse("| a | b | c |\n| --- | :---: | ---: |\n| d | e | f |\n")
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

	output, stderr, err := runDecoder("edit-cells.ts", base64.StdEncoding.EncodeToString(crdt.EncodeStateAsUpdateV1(doc, nil)))
	if err != nil {
		t.Fatalf("run edit-cells.ts: %v\nstderr:\n%s", err, stderr)
	}
	update, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(output)))
	if err != nil {
		t.Fatalf("edit-cells.ts output: %v\n%s", err, output)
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
	if want := "| ax | b | c |\n| --- | :---: | ---: |\n| dy | e | f |\n"; markdown != want {
		t.Fatalf("after the browser editor's cell edits, the table renders %q, want %q", markdown, want)
	}
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
