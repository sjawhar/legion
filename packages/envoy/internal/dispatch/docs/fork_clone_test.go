package docs

import (
	"bytes"
	"fmt"
	"maps"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A warm fork takes the room's replica cloned under the replica's lock (Service.forkPoint),
// where a replica holding no state leaves it the copy taken from the room itself, decoded
// (buildFork). These tests hold the fork read to that decode of the read point's state
// (Service.readPoint) on the same room at the same moment: the same document, the same commit
// position, the same replica read, and the same update for the same write made on each.

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
// decoded, which a warm fork took before, and the fork read itself - and fails the test where
// the two forks, their commit positions, their replica reads or the updates they make for the
// same writes differ. Nothing commits between the two reads: the first classifies everything the
// room's log has handed over, so the second drains nothing and stands where the first did.
//
// It returns how many writes it made on each fork, which the test that drives it reports.
func compareForkReads(t *testing.T, at corpusRoom) int {
	t.Helper()
	state, point, read, err := at.service.readPoint(at.room, at.live)
	if err != nil {
		t.Fatalf("%s: read the room's point: %v", at.stage, err)
	}
	want, err := buildFork(forkWriteClient, state)
	if err != nil {
		t.Fatalf("%s: decode the read point's state: %v", at.stage, err)
	}
	got, gotPoint, gotRead, err := at.service.forkPoint(at.room, at.live, forkWriteClient, nil)
	if err != nil {
		t.Fatalf("%s: fork the room: %v", at.stage, err)
	}
	if gotPoint != point {
		t.Fatalf("%s: the fork read stands at %+v, want the read point's %+v", at.stage, gotPoint, point)
	}
	if gotRead.mark != read.mark {
		t.Fatalf("%s: the fork read found the replica at %+v, want the read point's %+v", at.stage, gotRead.mark, read.mark)
	}
	if describeMarkdown(gotRead.markdown) != describeMarkdown(read.markdown) {
		t.Fatalf("%s: the fork read's rendering is %s, want the read point's %s",
			at.stage, describeMarkdown(gotRead.markdown), describeMarkdown(read.markdown))
	}
	compareForks(t, got, want, at.stage+": the fork as it was read")
	writes := 0
	for _, kind := range forkWriteKinds {
		write := kind.prepare(t, want)
		if write == nil {
			continue
		}
		wantUpdate, gotUpdate := write(t, want), write(t, got)
		if !bytes.Equal(gotUpdate, wantUpdate) {
			t.Fatalf("%s: %s on the forked document made a %d-byte update, want the decoded fork's %d bytes",
				at.stage, kind.what, len(gotUpdate), len(wantUpdate))
		}
		compareForks(t, got, want, at.stage+": the fork after "+kind.what)
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

// A room is driven through the corpus (driveCorpusRooms), forked at each point the driver
// compares at: before the room's first transaction, when the replica holds no state and the fork
// read takes its copy from the room itself, and after each of its browser edits.
func TestTheClonedForkMatchesTheReadPointsForkAcrossTheCorpus(t *testing.T) {
	writes := 0
	forks, documents := driveCorpusRooms(t, func(at corpusRoom) {
		writes += compareForkReads(t, at)
	})
	t.Logf("%d fork pairs compared, with %d writes made on each fork, over %d corpus documents", forks, writes, documents)
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
	// A commit of its own gives the replica the room's state to follow: the fork of a replica
	// holding none is a decode of the room's own copy and not a clone.
	typeIntoLastParagraph(t, live, "first ")
	replica.observe("room")
	if replica.doc == nil {
		t.Fatal("the replica holds no state after the room's commit, so the fork below is no clone of it")
	}

	fork, _, _, err := service.forkPoint("room", live, forkWriteClient, nil)
	if err != nil {
		t.Fatalf("fork the room: %v", err)
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
