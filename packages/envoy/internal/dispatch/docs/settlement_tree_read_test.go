package docs

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The settlement's read of a room hands back the tree the room's replica holds, read under the
// replica's lock (Service.treePoint), where a replica holding no state leaves it the copy taken
// from the room itself, decoded and read (stateTree). These tests hold that read to the read
// point's state decoded and read (Service.readPoint) on the same room at the same moment: the
// same tree, the same commit position and the same error.

// settlementTreeReads takes both reads of the room at one point - the read point's state decoded
// and read, and the tree read - and fails the test where their trees, their commit positions or
// their errors differ. Nothing commits between the two: the first read classifies everything the
// room's log has handed over, so the second drains nothing and stands where the first did. It
// returns the error both gave, which a room outside the schema holds to the refusal it expects.
func settlementTreeReads(t *testing.T, at corpusRoom) error {
	t.Helper()
	state, point, _, err := at.service.readPoint(at.room, at.live)
	if err != nil {
		t.Fatalf("%s: read the room's point: %v", at.stage, err)
	}
	want, wantErr := stateTree(state)
	got, gotPoint, _, gotErr := at.service.treePoint(at.room, at.live)
	if describeError(gotErr) != describeError(wantErr) {
		t.Fatalf("%s: the tree read answered %s, want the read point's %s", at.stage, describeError(gotErr), describeError(wantErr))
	}
	if gotPoint != point {
		t.Fatalf("%s: the tree read stands at %+v, want the read point's %+v", at.stage, gotPoint, point)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: the tree read's tree is not the read point's:\n got %s\nwant %s", at.stage, treeMarkdown(got), treeMarkdown(want))
	}
	return gotErr
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

// A room is driven through the corpus (driveCorpusRooms), read at each point the driver compares
// at: before the room's first transaction, when the replica holds no state and both reads take
// their copy from the room itself, and after each of its browser edits.
func TestTheSettlementTreeReadMatchesTheReadPointsTreeAcrossTheCorpus(t *testing.T) {
	reads, documents := driveCorpusRooms(t, func(at corpusRoom) {
		if err := settlementTreeReads(t, at); err != nil {
			t.Fatalf("%s: both reads of a corpus document answered %s, want the tree", at.stage, describeError(err))
		}
	})
	t.Logf("%d reads compared over %d corpus documents", reads, documents)
}

// A room whose document is outside the schema: the element a crafted client can write, which the
// tree reader refuses (appendSchemaInvalidUpdate). The walk of the replica and the decode of the
// read point's state refuse it alike, with the error a read of the document answers
// (OutsideSchemaError), so the two reads agree on the refusal as well as on the tree.
func TestTheSettlementTreeReadOfARoomOutsideTheSchemaRefusesItAsTheReadPointDoes(t *testing.T) {
	service := &Service{}
	live := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(live, newProofHistory(t, 1).write("## Heading\n\nA paragraph.\n"), nil); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	log := live.OpenUpdateLog()
	defer log.Close()
	replica := service.keepReplica(live, log)
	// The log records its base as of just before this commit, so the replica starts from the
	// room and takes the crafted element: the tree read walks a replica, not a copy of the room.
	// The fragment is taken before the transaction, which holds the document's lock.
	fragment := live.GetXmlFragment(fragmentName)
	live.Transact(func(txn *crdt.Transaction) {
		fragment.InsertElement(txn, 0, crdt.NewYXmlElement("callout"))
	})
	replica.observe("room")
	if replica.doc == nil {
		t.Fatal("the replica holds no state, so the tree read took its copy from the room instead of walking it")
	}

	err := settlementTreeReads(t, corpusRoom{service, replica, "room", live, "a room outside the schema"})
	var outside *OutsideSchemaError
	if !errors.As(err, &outside) {
		t.Fatalf("both reads answered %s, want the document's own OutsideSchemaError", describeError(err))
	}
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
// carries: the service's settlement read (treePoint) is the read point's state decoded and read
// (atPoint, stateTree), at the same point in the same room instance.
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
	// The read point without the arming readPoint does, so the comparison arms no settlement of
	// the room and moves no room generation, as the settlement's own read arms none.
	var encoded []byte
	copied, point, _, _, err := service.atPoint(artifactID, live, func(_ roomPoint, doc *crdt.Doc) {
		encoded = crdt.EncodeStateAsUpdateV1(doc, nil)
	})
	if err != nil {
		t.Fatalf("read the room's point: %v", err)
	}
	if copied != nil {
		encoded = copied
	}
	want, wantErr := stateTree(encoded)
	got, gotPoint, _, gotErr := service.treePoint(artifactID, live)
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
