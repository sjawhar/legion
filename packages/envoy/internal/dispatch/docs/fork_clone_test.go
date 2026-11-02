package docs

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A warm fork takes the room's replica cloned under the replica's lock
// (renderedReplica.clonePoint) where it decoded the replica's state encoded into a document of
// its own (readPoint, buildFork). These tests hold the clone to that decode on the same room at
// the same moment: the same document, the same commit position, the same replica read, and the
// same update for the same write made on each.

// forkWriteClient is the client id both forks of a comparison are built with, as a write's fork
// is built with its transaction's (liveWrite.clientID): the two forks author the same items, so
// the update each makes for the same write is comparable byte for byte.
const forkWriteClient crdt.ClientID = 991

// forkWriteKinds are the writes each comparison makes on both forks: a word typed into the
// document's last paragraph, the way a keystroke reaches a text, and a paragraph appended
// through pmdoc.Update, the call a write's own mutate makes (applyLive).
//
// Each is prepared from one of the two forks, which hold the same state, and the write it hands
// back is then made on both: the tree a write writes carries block ids drawn at random
// (appendBlocks), so a write prepared for each fork would compare two different writes. A
// preparation that hands back nothing is a write the document does not take - the corpus holds
// a document with no paragraph to type into.
var forkWriteKinds = []struct {
	what    string
	prepare func(t *testing.T, from *crdt.Doc) func(*testing.T, *crdt.Doc) []byte
}{
	{"a word typed into its last paragraph", prepareTypedWord},
	{"a paragraph appended", prepareAppendedParagraph},
}

// prepareTypedWord is a word typed at the start of a fork's own last paragraph text.
func prepareTypedWord(t *testing.T, from *crdt.Doc) func(*testing.T, *crdt.Doc) []byte {
	t.Helper()
	if len(paragraphTexts(from)) == 0 {
		return nil
	}
	return func(t *testing.T, fork *crdt.Doc) []byte {
		t.Helper()
		texts := paragraphTexts(fork)
		target := texts[len(texts)-1]
		return writeUpdate(t, fork, func(txn *crdt.Transaction) { target.Insert(txn, 0, "typed ", nil) })
	}
}

// prepareAppendedParagraph is from's tree with a paragraph appended, written into a fork through
// pmdoc.Update as a write's own mutate writes the tree it wants.
func prepareAppendedParagraph(t *testing.T, from *crdt.Doc) func(*testing.T, *crdt.Doc) []byte {
	t.Helper()
	tree, err := pmdoc.Read(from.GetXmlFragment(fragmentName))
	if err != nil {
		t.Fatalf("read the fork's tree to append to it: %v", err)
	}
	appended := appendBlocks(t, "A paragraph the fork write appended.\n")(tree)
	return func(t *testing.T, fork *crdt.Doc) []byte {
		t.Helper()
		fragment := fork.GetXmlFragment(fragmentName)
		// The write's error is kept for after the transaction: failing inside one would leave
		// the document locked for every read that follows.
		var writeErr error
		update := writeUpdate(t, fork, func(txn *crdt.Transaction) {
			writeErr = pmdoc.Update(txn, fragment, appended)
		})
		if writeErr != nil {
			t.Fatalf("append a paragraph to the fork: %v", writeErr)
		}
		return update
	}
}

// writeUpdate makes one write to fork and returns the update its transaction committed, taken as
// a write's operation takes its own (liveOperation.change).
func writeUpdate(t *testing.T, fork *crdt.Doc, write func(*crdt.Transaction)) []byte {
	t.Helper()
	var update []byte
	unsubscribe := fork.OnUpdate(func(made []byte, _ any) { update = append(update, made...) })
	defer unsubscribe()
	fork.Transact(write)
	if len(update) == 0 {
		t.Fatal("the write on the fork committed no update")
	}
	return update
}

// compareForkReads takes both fork reads of the room at one point - the read point's state
// decoded, which a warm fork took before, and the clone read - and fails the test where the two
// forks, their commit positions, their replica reads or the updates they make for the same
// writes differ. Nothing commits between the two reads: the first classifies everything the
// room's log has handed over, so the second drains nothing and stands where the first did.
//
// It returns how many writes it made on each fork, which the test that drives it reports.
func compareForkReads(t *testing.T, replica *renderedReplica, room string, live *crdt.Doc, stage string) int {
	t.Helper()
	state, seq, read, _ := replica.readPoint(room, live)
	want, err := buildFork(forkWriteClient, state)
	if err != nil {
		t.Fatalf("%s: decode the read point's state: %v", stage, err)
	}
	got, copied, gotSeq, gotRead, _ := replica.clonePoint(room, live, forkOptions(forkWriteClient)...)
	if got == nil {
		if got, err = buildFork(forkWriteClient, copied); err != nil {
			t.Fatalf("%s: decode the clone read's copy of the room: %v", stage, err)
		}
	} else if copied != nil {
		t.Fatalf("%s: the clone read handed back a clone and a copy of the room's state", stage)
	}
	if gotSeq != seq {
		t.Fatalf("%s: the clone read stands at commit %d, want the read point's %d", stage, gotSeq, seq)
	}
	if gotRead.mark != read.mark {
		t.Fatalf("%s: the clone read found the replica at %+v, want the read point's %+v", stage, gotRead.mark, read.mark)
	}
	if describeMarkdown(gotRead.markdown) != describeMarkdown(read.markdown) {
		t.Fatalf("%s: the clone read's rendering is %s, want the read point's %s",
			stage, describeMarkdown(gotRead.markdown), describeMarkdown(read.markdown))
	}
	compareForks(t, got, want, stage+": the fork as it was read")
	writes := 0
	for _, kind := range forkWriteKinds {
		write := kind.prepare(t, want)
		if write == nil {
			continue
		}
		wantUpdate, gotUpdate := write(t, want), write(t, got)
		if !bytes.Equal(gotUpdate, wantUpdate) {
			t.Fatalf("%s: %s on the cloned fork made a %d-byte update, want the decoded fork's %d bytes",
				stage, kind.what, len(gotUpdate), len(wantUpdate))
		}
		compareForks(t, got, want, stage+": the fork after "+kind.what)
		writes++
	}
	return writes
}

// compareForks fails the test where two forks of the same state differ in what they encode to,
// which is what a write stores and publishes, or in the state vector a reader of either asks
// them for (forkHeld, captureLiveTextAndAuthors).
func compareForks(t *testing.T, got, want *crdt.Doc, stage string) {
	t.Helper()
	gotState, wantState := crdt.EncodeStateAsUpdateV1(got, nil), crdt.EncodeStateAsUpdateV1(want, nil)
	if !bytes.Equal(gotState, wantState) {
		t.Fatalf("%s: the cloned fork encodes to %d bytes, want the decoded fork's %d", stage, len(gotState), len(wantState))
	}
	if !maps.Equal(got.StateVector(), want.StateVector()) {
		t.Fatalf("%s: the cloned fork stands at %v, want the decoded fork's %v", stage, got.StateVector(), want.StateVector())
	}
}

// describeMarkdown names a replica read's rendering: a read that holds none stands for no
// rendering alone, and two renderings stand for each other only where they read the same.
func describeMarkdown(markdown *string) string {
	if markdown == nil {
		return "no rendering"
	}
	return fmt.Sprintf("a %d-byte rendering: %q", len(*markdown), *markdown)
}

// A room is driven through the corpus: every in-repo document, as a room with the replica its
// update observer keeps, forked before the room's first transaction - when the replica holds no
// state and both reads take their copy from the room itself - and after each of a series of
// browser edits, some classified by the observer and some by the read that follows them.
func TestTheClonedForkMatchesTheReadPointsForkAcrossTheCorpus(t *testing.T) {
	paths, err := filepath.Glob("../pmdoc/testdata/corpus/*.md")
	if err != nil || len(paths) == 0 {
		t.Fatalf("corpus: %v (%d documents)", err, len(paths))
	}
	service := &Service{}
	forks, writes := 0, 0
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

		writes += compareForkReads(t, replica, name, live, name+": a room with no transaction")
		forks++
		if replica.doc != nil {
			t.Fatalf("%s: the replica of a room with no transaction holds state, so no fork took its copy from the room", name)
		}

		for index, edit := range []struct {
			what string
			edit func()
		}{
			{"a block appended", func() {
				editRoomDocument(t, live, appendBlocks(t, "A paragraph the fork read.\n"))
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
			writes += compareForkReads(t, replica, name, live, name+": "+edit.what)
			forks++
			if replica.doc == nil {
				t.Fatalf("%s: the replica holds no state after %s, so no fork cloned it", name, edit.what)
			}
		}
		log.Close()
	}
	t.Logf("%d fork pairs compared, with %d writes made on each fork, over %d corpus documents", forks, writes, len(paths))
}

// The clone is a document of its own: the write that runs on it changes nothing the replica
// holds, and the room's own commits, which the replica consumes, change nothing the fork holds.
func TestAClonedForkAndItsReplicaAreIndependent(t *testing.T) {
	service := &Service{}
	live := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(live, newProofHistory(t, 1).write("## Heading\n\nThe paragraph a fork writes into.\n"), nil); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	log := live.OpenUpdateLog()
	defer log.Close()
	replica := service.keepReplica(live, log)
	// A commit of its own gives the replica the room's state to follow: a replica holding none
	// has nothing to clone.
	typeIntoLastParagraph(t, live, "first ")
	replica.observe("room")

	fork, copied, _, _, _ := replica.clonePoint("room", live, forkOptions(forkWriteClient)...)
	if fork == nil {
		t.Fatalf("the clone read of a replica holding state took a copy of the room instead (%d bytes)", len(copied))
	}
	replicaState := replicaEncoding(t, replica)
	forkState := crdt.EncodeStateAsUpdateV1(fork, nil)
	if !bytes.Equal(forkState, replicaState) {
		t.Fatalf("the fork encodes to %d bytes, want the replica's %d", len(forkState), len(replicaState))
	}

	typed := prepareTypedWord(t, fork)
	if typed == nil {
		t.Fatal("the fork holds no paragraph to write into")
	}
	typed(t, fork)
	forkWritten := crdt.EncodeStateAsUpdateV1(fork, nil)
	if bytes.Equal(forkWritten, forkState) {
		t.Fatal("the write on the fork left it encoding as it did before")
	}
	if after := replicaEncoding(t, replica); !bytes.Equal(after, replicaState) {
		t.Fatalf("the write on the fork left the replica encoding to %d bytes, want the %d it held", len(after), len(replicaState))
	}

	typeIntoLastParagraph(t, live, "second ")
	replica.observe("room")
	if after := replicaEncoding(t, replica); bytes.Equal(after, replicaState) {
		t.Fatal("the room's commit left the replica encoding as it did before")
	}
	if after := crdt.EncodeStateAsUpdateV1(fork, nil); !bytes.Equal(after, forkWritten) {
		t.Fatalf("the room's commit left the fork encoding to %d bytes, want the %d it held", len(after), len(forkWritten))
	}
}

// replicaEncoding is the replica's document encoded, taken under the replica's lock as every
// read of it is.
func replicaEncoding(t *testing.T, replica *renderedReplica) []byte {
	t.Helper()
	doc := replica.hold()
	if doc == nil {
		t.Fatal("the replica holds no document to encode")
	}
	defer replica.mu.Unlock()
	return crdt.EncodeStateAsUpdateV1(doc, nil)
}
