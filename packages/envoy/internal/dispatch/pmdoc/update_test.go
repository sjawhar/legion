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
			if !pm.Equal(want) {
				actual, _ := pm.JSON()
				expected, _ := want.JSON()
				t.Fatalf("y-prosemirror decodes Go-authored bytes differently\n got: %s\nwant: %s", actual, expected)
			}
		})
	}
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

func fixtureNamed(t *testing.T, name string) Fixture {
	t.Helper()
	for _, fx := range loadFixtures(t) {
		if fx.Name == name {
			return fx
		}
	}
	t.Fatalf("fixture %q not found", name)
	return Fixture{}
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
