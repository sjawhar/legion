package docs

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The settlement's read of a room hands back the tree the room's replica holds, read under the
// replica's lock (renderedReplica.readTreePoint), where it took the replica's state encoded and
// decoded it into a document of its own to read one tree from it (readPoint, stateTree). These
// tests hold the new read to the old one on the same room at the same moment: the same tree, the
// same commit position and the same error.

// settlementTreeReads takes both reads of the room at one point - the read point's state decoded
// and read, which the settlement took before, and the tree read - and fails the test where their
// trees, their commit positions or their errors differ. Nothing commits between the two: the
// first read classifies everything the room's log has handed over, so the second drains nothing
// and stands where the first did. A document outside the schema is refused by both alike, which
// is that same check.
func settlementTreeReads(t *testing.T, replica *renderedReplica, room string, live *crdt.Doc, stage string) {
	t.Helper()
	state, seq, _, _ := replica.readPoint(room, live)
	want, wantErr := stateTree(state)
	got, gotSeq, _, gotErr := replica.readTreePoint(room, live)
	if describeError(gotErr) != describeError(wantErr) {
		t.Fatalf("%s: the tree read answered %s, want the read point's %s", stage, describeError(gotErr), describeError(wantErr))
	}
	if gotSeq != seq {
		t.Fatalf("%s: the tree read stands at commit %d, want the read point's %d", stage, gotSeq, seq)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: the tree read's tree is not the read point's:\n got %s\nwant %s", stage, treeMarkdown(got), treeMarkdown(want))
	}
}

// describeError names an error by its type and its text, so two errors stand for each other only
// where a caller that classifies them (documentSchemaError's OutsideSchemaError) and one that
// reads them both see the same thing.
func describeError(err error) string {
	if err == nil {
		return "no error"
	}
	return fmt.Sprintf("%T: %v", err, err)
}

// A room is driven through the corpus: every in-repo document, as a room with the replica its
// update observer keeps, read before the room's first transaction - when the replica holds no
// state and both reads take their copy from the room itself - and after each of a series of
// browser edits, some classified by the observer and some by the read that follows them.
func TestTheSettlementTreeReadMatchesTheReadPointsTreeAcrossTheCorpus(t *testing.T) {
	paths, err := filepath.Glob("../pmdoc/testdata/corpus/*.md")
	if err != nil || len(paths) == 0 {
		t.Fatalf("corpus: %v (%d documents)", err, len(paths))
	}
	service := &Service{}
	reads := 0
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".md")
		markdown, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		parsed, err := pmdoc.Parse(string(markdown))
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		update, err := encodeDocumentTree(parsed)
		if err != nil {
			t.Fatalf("encode %s: %v", name, err)
		}
		live := newDocumentCopy()
		if err := crdt.ApplyUpdateV1(live, update, nil); err != nil {
			t.Fatalf("load %s into a room: %v", name, err)
		}
		log := live.OpenUpdateLog()
		replica := service.keepReplica(live, log)

		settlementTreeReads(t, replica, name, live, name+": a room with no transaction")
		reads++
		if replica.doc != nil {
			t.Fatalf("%s: the replica of a room with no transaction holds state, so the read took no copy from the room", name)
		}

		for index, edit := range []struct {
			what string
			edit func()
		}{
			{"a block appended", func() {
				editRoomDocument(t, live, appendBlocks(t, "A paragraph the settlement read.\n"))
			}},
			{"a table with marks appended", func() {
				editRoomDocument(t, live, appendBlocks(t, "| Column | Value |\n| --- | --- |\n| row | **cell** |\n"))
			}},
			{"a word typed", func() { typeIntoLastParagraph(t, live, "typed ") }},
			{"a character deleted", func() { deleteFromLastParagraph(t, live) }},
		} {
			edit.edit()
			// Half the edits are classified by the room's update observer, half by the read
			// that follows them, the two drainers of the room's log.
			if index%2 == 0 {
				replica.observe(name)
			}
			settlementTreeReads(t, replica, name, live, name+": "+edit.what)
			reads++
			if replica.doc == nil {
				t.Fatalf("%s: the replica holds no state after %s, so no read walked it", name, edit.what)
			}
		}
		log.Close()
	}
	t.Logf("%d reads compared over %d corpus documents", reads, len(paths))
}

// editRoomDocument writes edit's tree into live in one transaction, as a browser's edit reaches
// the room (editLiveTree), on a room held outside the service.
func editRoomDocument(t *testing.T, live *crdt.Doc, edit func(*pmdoc.Node) *pmdoc.Node) {
	t.Helper()
	fragment := live.GetXmlFragment(fragmentName)
	tree, err := pmdoc.Read(fragment)
	if err != nil {
		t.Fatalf("read the room's tree to edit it: %v", err)
	}
	want := edit(tree)
	live.Transact(func(txn *crdt.Transaction) {
		if err := pmdoc.Update(txn, fragment, want); err != nil {
			t.Fatalf("write the edit into the room: %v", err)
		}
	})
}

// typeIntoLastParagraph types text at the start of the room's last top-level paragraph, the way a
// browser's keystroke reaches the room: into the Yjs text itself, outside any tree write.
func typeIntoLastParagraph(t *testing.T, live *crdt.Doc, text string) {
	t.Helper()
	target := lastParagraphText(t, live)
	live.Transact(func(txn *crdt.Transaction) { target.Insert(txn, 0, text, nil) })
}

// deleteFromLastParagraph deletes the first character of the room's last top-level paragraph.
func deleteFromLastParagraph(t *testing.T, live *crdt.Doc) {
	t.Helper()
	target := lastParagraphText(t, live)
	live.Transact(func(txn *crdt.Transaction) { target.Delete(txn, 0, 1) })
}

func lastParagraphText(t *testing.T, live *crdt.Doc) *crdt.YXmlText {
	t.Helper()
	texts := paragraphTexts(live)
	if len(texts) == 0 {
		t.Fatal("the room holds no paragraph text to type into")
	}
	return texts[len(texts)-1]
}

// A resident room the service serves, with the marks, the anchor and the table a real document
// carries: the service's settlement read (readTreeQuietly) is the read point's state decoded and
// read (readPointQuietly, stateTree), at the same point in the same room instance.
func TestTheSettlementTreeReadOfAResidentRoomMatchesTheReadPointsTree(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "# Settlement read\n\n"+
		"A paragraph with **bold**, *emphasis*, `code`, a [link](https://example.com) and anchored words here.\n\n"+
		"| Column | Value |\n| --- | --- |\n| row | cell |\n| other | **cell** |\n\n"+
		"- first item\n- second item with `code`\n")
	if _, err := joinedMarkQuote(service, artifactID, MarkSpec{
		Kind: MarkComment, ID: "c-settlement-read", By: model.Actor{Kind: "user", ID: "alice"},
	}, "anchored words", nil); err != nil {
		t.Fatalf("anchor a comment: %v", err)
	}
	bob := model.Actor{Kind: "session", ID: "bob-session"}
	editAsConnectedPeer(t, service, artifactID, 1, bob, replaceRun("cell", "cell edited"))
	editAsConnectedPeer(t, service, artifactID, 2, bob, appendBlocks(t, "A line the browser typed.\n"))

	live := service.srv.GetDoc(artifactID)
	if live == nil {
		t.Fatal("the room is not resident")
	}
	encoded, point, _, _, err := service.readPointQuietly(artifactID, live)
	if err != nil {
		t.Fatalf("read the room's point: %v", err)
	}
	want, wantErr := stateTree(encoded)
	got, gotPoint, _, gotErr := service.readTreeQuietly(artifactID, live)
	if describeError(gotErr) != describeError(wantErr) {
		t.Fatalf("the tree read answered %s, want the read point's %s", describeError(gotErr), describeError(wantErr))
	}
	if gotPoint != point {
		t.Fatalf("the tree read is at %+v, want the read point's %+v", gotPoint, point)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the tree read's tree is not the read point's:\n got %s\nwant %s", treeMarkdown(got), treeMarkdown(want))
	}
	if anchors := marksOfType(got, string(MarkComment)); anchors != 1 {
		t.Fatalf("the read tree carries %d anchor marks, want the one the comment anchored", anchors)
	}
	markdown, err := documentMarkdown(got)
	if err != nil {
		t.Fatalf("render the read tree: %v", err)
	}
	for _, want := range []string{"| row | cell edited |", "A line the browser typed.", "**bold**"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("the read tree renders %q, want %q in it", markdown, want)
		}
	}
}

// marksOfType counts the text nodes of tree carrying a mark of type name.
func marksOfType(tree *pmdoc.Node, name string) int {
	if tree == nil {
		return 0
	}
	count := 0
	for _, mark := range tree.Marks {
		if mark.Type == name {
			count++
		}
	}
	for _, child := range tree.Children {
		count += marksOfType(child, name)
	}
	return count
}
