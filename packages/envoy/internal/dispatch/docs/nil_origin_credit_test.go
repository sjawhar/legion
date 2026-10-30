package docs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// INFRA-644. ygo answers a remote update that leaves a text's formatting markers covering no text
// with a cleanup of its own (reearth/ygo #284): a local transaction on the document that applied
// the remote update, committed with no origin at all. In a Dispatch room that cleanup is one more
// logged update, which the room's update observer classifies and, when it counts as a content
// change, credits to every browser connected at the time (creditContentChange). Nobody typed it,
// so it must be credited to no one.
//
// The first test here is the scenario itself: a browser deletes exactly the text another browser's
// bold run covers, which leaves the markers covering nothing. Whether ygo commits a cleanup for it
// is the pinned version's answer - v1.51.3-sami.8 commits none, the pin that carries reearth/ygo
// #284 commits one - so the test holds what the room does with however many updates the delete
// leaves, and logs the count rather than asserting it. The second test is the path on which a room
// credits such a commit at all, and is deterministic on any ygo: a replica that cannot place an
// update hands it to recover, which classifies every update it could not place as a content
// change (renderedReplica.recover), so a nil-origin commit made while a browser is connected is
// the one that would credit that browser for an edit it never made.

const (
	nilOriginParagraph = "Paragraph 0 opens"
	nilOriginWord      = "enough"
)

// nilOriginCommit is what the service's own update observer made of one of the room's commits,
// read while the class it recorded is still there (recordUpdateClass, consumeUpdateClass takes it
// later), with the room's credit record just after it.
type nilOriginCommit struct {
	origin    string
	classes   int
	changed   bool
	durable   bool
	credited  bool
	creditSeq uint64
	authors   []string
	lastActor string
	after     creditState
}

func (c nilOriginCommit) String() string {
	credit := "credit=none"
	if c.credited {
		credit = fmt.Sprintf("credit=#%d authors=%v lastActor=%s", c.creditSeq, c.authors, c.lastActor)
	}
	return fmt.Sprintf("origin=%s classes=%d changed=%t durable=%t %s | room after: %s",
		c.origin, c.classes, c.changed, c.durable, credit, c.after)
}

// creditState is the room's credit record at one moment: who is connected, F (the in-flight
// credits), the latest edit source and how many ask-block authors it holds.
type creditState struct {
	connected []string
	creditSeq uint64
	inflight  []string
	lastActor string
	unsettled bool
	asks      int
}

func (s creditState) String() string {
	return fmt.Sprintf("connected=%v creditSeq=%d inflight=%v lastActor=%s unsettled=%t askAuthors=%d",
		s.connected, s.creditSeq, s.inflight, s.lastActor, s.unsettled, s.asks)
}

func readCreditState(service *Service, room string) creditState {
	state := service.lockExistingState(room)
	if state == nil {
		return creditState{lastActor: "none"}
	}
	defer service.unlockState(room, state)
	snapshot := creditState{
		creditSeq: state.creditSeq.Load(),
		lastActor: describeActor(state.lastActor),
		unsettled: state.unsettled,
		asks:      len(state.askAuthors),
	}
	for _, actor := range state.connected {
		snapshot.connected = append(snapshot.connected, actorKey(actor))
	}
	slices.Sort(snapshot.connected)
	for seq, record := range state.inflight {
		snapshot.inflight = append(snapshot.inflight, fmt.Sprintf("#%d authors=%v lastActor=%s consumed=%t",
			seq, sortedActorKeys(record.authors), describeActor(record.lastActor), record.consumed))
	}
	slices.Sort(snapshot.inflight)
	return snapshot
}

func sortedActorKeys(authors map[string]model.Actor) []string {
	keys := make([]string, 0, len(authors))
	for key := range authors {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func actorKeysOf(actors []model.Actor) []string {
	keys := make([]string, 0, len(actors))
	for _, actor := range actors {
		keys = append(keys, actorKey(actor))
	}
	slices.Sort(keys)
	return keys
}

func describeActor(actor *model.Actor) string {
	if actor == nil {
		return "none"
	}
	return actorKey(*actor)
}

func describeOrigin(origin any) string {
	switch typed := origin.(type) {
	case nil:
		return "nil"
	case string:
		return fmt.Sprintf("%q", typed)
	case *liveWriteOrigin:
		return "live write"
	case *identityClosureOrigin:
		return "identity repair"
	default:
		return fmt.Sprintf("%T", origin)
	}
}

// nilOriginWatch records every commit of the room, with the class and the credit the service's own
// observer recorded for it. Its observer is registered after the service's own and after the
// shadow's (watchRenders), and before ygo's persistence observer, which the room registers once
// OnLoadDocument returns: so the class is still recorded when it reads it.
type nilOriginWatch struct {
	service *Service
	room    string

	mu   sync.Mutex
	seen []nilOriginCommit
}

func watchNilOriginCredits(t *testing.T, service *Service, artifactID string) *nilOriginWatch {
	t.Helper()
	watch := &nilOriginWatch{service: service, room: artifactID}
	load := service.srv.OnLoadDocument
	service.srv.OnLoadDocument = func(ctx context.Context, room string, doc *crdt.Doc) error {
		if err := load(ctx, room, doc); err != nil {
			return err
		}
		if room != artifactID {
			return nil
		}
		doc.OnUpdate(func(update []byte, origin any) { watch.record(update, origin) })
		return nil
	}
	return watch
}

func (w *nilOriginWatch) record(update []byte, origin any) {
	commit := nilOriginCommit{origin: describeOrigin(origin), lastActor: "none"}
	if state := w.service.lockExistingState(w.room); state != nil {
		for _, class := range state.updateClasses {
			if !bytes.Equal(class.update, update) {
				continue
			}
			commit.classes++
			commit.changed, commit.durable = class.contentChanged, class.durable
			if class.credit != nil {
				commit.credited = true
				commit.creditSeq = class.credit.record.seq
				commit.authors = sortedActorKeys(class.credit.record.authors)
				commit.lastActor = describeActor(class.credit.record.lastActor)
			}
		}
		w.service.unlockState(w.room, state)
	}
	commit.after = readCreditState(w.service, w.room)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seen = append(w.seen, commit)
}

func (w *nilOriginWatch) mark() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.seen)
}

func (w *nilOriginWatch) since(mark int) []nilOriginCommit {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.seen[mark:])
}

// loggedSince is every update the room logged since m, as the shadow took it: the shadow's own
// verdict on whether it changed the rendering (changed) and the class the service recorded.
func loggedSince(sh *renderShadow, m renderMeasure) []*shadowUpdate {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	var all []*shadowUpdate
	for _, room := range sh.rooms {
		all = append(all, room.updates...)
	}
	return slices.Clone(all[m.updates:])
}

func reportLogged(t *testing.T, logged []*shadowUpdate, seen []nilOriginCommit) {
	t.Helper()
	t.Logf("the room logged %d update(s)", len(logged))
	for index, update := range logged {
		t.Logf("  logged %d: origin=%s observed=%d classes=%d classified changed=%t, the shadow's own render says changed=%t",
			index, describeOrigin(update.origin), update.observed, update.classes, update.class, update.changed)
	}
	for index, commit := range seen {
		t.Logf("  commit %d: %s", index, commit)
	}
	if len(logged) != len(seen) {
		t.Errorf("the room logged %d updates and the service's observer ran for %d", len(logged), len(seen))
	}
}

// docState is one document's CRDT state, read three independent ways: the state vector, the
// delete set alone (the whole state encoded against the document's own state vector, which carries
// no struct), and the XML of the document fragment.
type docState struct {
	stateVector []byte
	deleteSet   []byte
	xml         string
}

func readDocState(doc *crdt.Doc, fragment *crdt.YXmlFragment) docState {
	return docState{
		stateVector: crdt.EncodeStateVectorV1(doc),
		deleteSet:   crdt.EncodeStateAsUpdateV1(doc, doc.StateVector()),
		xml:         fragment.ToXML(),
	}
}

func (s docState) equal(other docState) bool {
	return bytes.Equal(s.stateVector, other.stateVector) &&
		bytes.Equal(s.deleteSet, other.deleteSet) &&
		s.xml == other.xml
}

func (s docState) String() string {
	return fmt.Sprintf("stateVector=%d bytes deleteSet=%d bytes xml=%d bytes", len(s.stateVector), len(s.deleteSet), len(s.xml))
}

// expectReplicaHoldsTheRoom asserts that the room's replica, once it holds every commit the room
// has made, holds the room's state.
func expectReplicaHoldsTheRoom(t *testing.T, service *Service, room *crdt.Doc) {
	t.Helper()
	replica := service.replicaOf(room)
	if replica == nil {
		t.Fatal("the room has no replica")
	}
	deadline := time.Now().Add(5 * time.Second)
	for replica.hold() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the replica never stood at the room's latest commit")
		}
		time.Sleep(time.Millisecond)
	}
	defer replica.mu.Unlock()
	roomState := readDocState(room, room.GetXmlFragment(fragmentName))
	replicaState := readDocState(replica.doc, replica.fragment)
	t.Logf("the room:    %s", roomState)
	t.Logf("the replica: %s", replicaState)
	if !replicaState.equal(roomState) {
		t.Errorf("the replica does not hold the room's state: stateVector equal=%t, deleteSet equal=%t, xml equal=%t",
			bytes.Equal(replicaState.stateVector, roomState.stateVector),
			bytes.Equal(replicaState.deleteSet, roomState.deleteSet),
			replicaState.xml == roomState.xml)
	}
}

// paragraphLine is the line of markdown that starts with prefix.
func paragraphLine(t *testing.T, markdown, prefix string) string {
	t.Helper()
	for line := range strings.SplitSeq(markdown, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("the rendering holds no line starting %q", prefix)
	return ""
}

func roomMarkdown(t *testing.T, room *crdt.Doc) string {
	t.Helper()
	copied, err := snapshotDocument(room)
	if err != nil {
		t.Fatalf("copy the room: %v", err)
	}
	markdown, _, err := shadowRender(copied)
	if err != nil || markdown == nil {
		t.Fatalf("render the room: %v", err)
	}
	return *markdown
}

// xmlTextWithPrefix is the text node under fragment whose text starts with prefix, wherever it
// stands, as a peer's own finder reads its copy (roomPeer.text).
func xmlTextWithPrefix(t *testing.T, fragment *crdt.YXmlFragment, prefix string) *crdt.YXmlText {
	t.Helper()
	var find func(parent *crdt.YXmlFragment) *crdt.YXmlText
	find = func(parent *crdt.YXmlFragment) *crdt.YXmlText {
		for _, child := range parent.Children() {
			switch node := child.(type) {
			case *crdt.YXmlText:
				if strings.HasPrefix(node.ToString(), prefix) {
					return node
				}
			case *crdt.YXmlElement:
				if found := find(&node.YXmlFragment); found != nil {
					return found
				}
			}
		}
		return nil
	}
	text := find(fragment)
	if text == nil {
		t.Fatalf("the document holds no text starting %q", prefix)
	}
	return text
}

// commitWithNoOrigin commits on the room's live document the way ygo's own formatting cleanup
// commits: doc.Transact with no origin argument at all, so the commit's origin is nil
// (crdt.Doc.Transact takes the origin variadically). Server.Apply holds the room for the call and
// captures only what its own transact helper commits, so this transaction is not captured and
// Apply answers ErrNoChanges - which is the point here: the commit reaches the room's update
// observers, and ygo's persistence, naming no one.
//
// prepare runs before the transaction opens and answers the change to commit: reading a shared
// type takes the document's lock, which the open transaction holds, so the nodes a change writes
// are found first.
func commitWithNoOrigin(t *testing.T, service *Service, artifactID string, prepare func(fragment *crdt.YXmlFragment) func(*crdt.Transaction)) {
	t.Helper()
	err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		change := prepare(doc.GetXmlFragment(fragmentName))
		doc.Transact(change)
	})
	if err != nil && !errors.Is(err, ygws.ErrNoChanges) {
		t.Fatalf("commit on the room with no origin: %v", err)
	}
}

// TestRenderReuseANilOriginCommitCreditsNoOne holds that a commit no one made - one ygo makes for
// the room itself, with no origin - credits no browser, wherever the room's observer classifies it
// as a content change.
func TestRenderReuseANilOriginCommitCreditsNoOne(t *testing.T) {
	t.Run("the browser's delete of a bold run it never saw", testADeleteThatDanglesABoldRun)
	t.Run("a nil-origin commit a recovered replica counts as changed", testANilOriginCommitAfterARecover)
}

// testADeleteThatDanglesABoldRun is the scenario: two browsers are connected, a third bolded a
// word and left, and one of the connected browsers deletes exactly that word. Its own document
// never held the markers, so it cannot tombstone them, and the room is left holding markers that
// cover no text - which the ygo that carries reearth/ygo #284 answers with a cleanup transaction
// of its own, committed with no origin. The browser's own update is credited to the browsers
// connected while it landed, as it always was; any further logged update is the cleanup's, and is
// credited to no one.
func testADeleteThatDanglesABoldRun(t *testing.T) {
	service, artifactID, sh := newRenderReuseService(t)
	watch := watchNilOriginCredits(t, service, artifactID)

	browsers := []model.Actor{reuseAlice, reuseBob}
	sent := map[string]bool{}
	deleter := newRoomPeer(t, service, artifactID, 1, browsers[0], sent)
	newRoomPeer(t, service, artifactID, 2, browsers[1], sent)
	room := service.srv.GetDoc(artifactID)
	if room == nil {
		t.Fatal("the browsers loaded no room")
	}

	// The bold run, made by a browser whose tab has closed since: the deleting browser's own
	// document holds those words without the markers, so the delete it sends cannot tombstone
	// markers it has never seen, which is what leaves them covering nothing in the room.
	bolder := newRoomPeer(t, service, artifactID, 99, model.Actor{Kind: "user", ID: "mallory"}, nil)
	service.removeConnection(artifactID, 99)
	bolded := bolder.text(nilOriginParagraph)
	boldAt := strings.Index(bolded.ToString(), nilOriginWord)
	if boldAt < 0 {
		t.Fatalf("the paragraph %q holds no %q to bold", nilOriginParagraph, nilOriginWord)
	}
	bolder.send(func(txn *crdt.Transaction) {
		bolded.Format(txn, boldAt, len(nilOriginWord), crdt.Attributes{"strong": map[string]any{}})
	})
	waitForLandedAppends(t, service, artifactID)
	boldLine := paragraphLine(t, roomMarkdown(t, room), nilOriginParagraph)
	if !strings.Contains(boldLine, "**"+nilOriginWord+"**") {
		t.Fatalf("the room's paragraph reads %q, which holds no bold %q", boldLine, nilOriginWord)
	}
	t.Logf("the room's paragraph before the delete: %q", boldLine)

	// The browser's delete: exactly the text the bold run covers, in one transaction of its own.
	// Its update is taken from the peer's own update observer rather than encoded from a state
	// vector, so what the room is sent is that transaction's update and nothing else.
	deleted := deleter.text(nilOriginParagraph)
	if at := strings.Index(deleted.ToString(), nilOriginWord); at != boldAt {
		t.Fatalf("the deleting browser holds %q at %d, the bold run stands at %d", nilOriginWord, at, boldAt)
	}
	var peerCommits [][]byte
	var peerOrigins []string
	deleter.doc.OnUpdate(func(update []byte, origin any) {
		peerCommits = append(peerCommits, bytes.Clone(update))
		peerOrigins = append(peerOrigins, describeOrigin(origin))
	})
	deleter.doc.Transact(func(txn *crdt.Transaction) { deleted.Delete(txn, boldAt, len(nilOriginWord)) })
	if len(peerCommits) == 0 {
		t.Fatal("the browser's delete committed nothing")
	}
	t.Logf("the browser's own document committed %d update(s) for its delete, origins %v", len(peerCommits), peerOrigins)

	before := readCreditState(service, artifactID)
	beforePending := pendingAuthorRows(t, service.store, artifactID)
	t.Logf("before the browser's update, the room: %s", before)
	t.Logf("before the browser's update, the durable pending authors: %v", actorKeysOf(beforePending))

	m := sh.measure()
	mark := watch.mark()
	if err := crdt.ApplyUpdateV1(room, peerCommits[0], "peer"); err != nil {
		t.Fatalf("apply the browser's update to the room: %v", err)
	}
	logged, seen := loggedSince(sh, m), watch.since(mark)
	// How many updates the delete leaves is the pinned ygo's answer, not this test's: one without
	// reearth/ygo #284, two with it.
	reportLogged(t, logged, seen)
	if len(seen) == 0 {
		t.Fatal("the room logged nothing for the browser's delete")
	}

	// The browser's own update, credited to the browsers connected while it landed, as ever.
	browserCommit := seen[0]
	want := actorKeysOf(browsers)
	if browserCommit.origin != `"peer"` {
		t.Errorf("the first logged update's origin is %s, want the browser's", browserCommit.origin)
	}
	if !browserCommit.changed {
		t.Error("the browser's own delete was classified as changing nothing")
	}
	if !browserCommit.credited || !slices.Equal(browserCommit.authors, want) {
		t.Errorf("the browser's update credited %v (credited=%t), want the connected browsers %v",
			browserCommit.authors, browserCommit.credited, want)
	}

	// Every further logged update is ygo's own cleanup, which names no one and credits no one: no
	// in-flight credit is taken for it (creditSeq counts every one the room ever took), and it
	// leaves the room's latest edit source where the browser's update left it.
	for index, commit := range seen[1:] {
		if commit.origin != "nil" {
			t.Errorf("logged update %d has origin %s, want the cleanup's nil: nothing else commits on the room here",
				index+1, commit.origin)
			continue
		}
		if commit.credited {
			t.Errorf("the cleanup credited %v: a commit ygo made for itself is nobody's edit", commit.authors)
		}
		if commit.after.creditSeq != browserCommit.after.creditSeq {
			t.Errorf("the cleanup took in-flight credit #%d; the browser's update left the room at #%d",
				commit.after.creditSeq, browserCommit.after.creditSeq)
		}
		if commit.after.lastActor != browserCommit.after.lastActor {
			t.Errorf("the cleanup left the room's latest edit source %s, the browser's update left it %s",
				commit.after.lastActor, browserCommit.after.lastActor)
		}
	}

	waitForLandedAppends(t, service, artifactID)
	after := readCreditState(service, artifactID)
	afterPending := pendingAuthorRows(t, service.store, artifactID)
	t.Logf("after the browser's update, the room: %s", after)
	t.Logf("after the browser's update, the durable pending authors: %v", actorKeysOf(afterPending))
	// The durable record owes what it owed, plus the browser's own update: one row per browser
	// connected while it landed. A cleanup credited as well would owe a second row for each.
	wantPending := append(actorKeysOf(beforePending), want...)
	slices.Sort(wantPending)
	if got := actorKeysOf(afterPending); !slices.Equal(got, wantPending) {
		t.Errorf("the durable pending authors are %v, want %v: the browser's own update owes its connected browsers and nothing else does",
			got, wantPending)
	}

	sh.check(t)
	expectReplicaHoldsTheRoom(t, service, room)
	line := paragraphLine(t, roomMarkdown(t, room), nilOriginParagraph)
	t.Logf("the room's paragraph after the delete: %q", line)
	if strings.Contains(line, nilOriginWord) || strings.Contains(line, "**") {
		t.Errorf("the room's paragraph reads %q: the deleted bold run is still there", line)
	}
}

// testANilOriginCommitAfterARecover is the path on which a room credits a nil-origin commit at
// all, and it needs no cleanup of ygo's to reach: a replica that holds no document hands every
// update it cannot place to recover, which classifies each as a content change
// (renderedReplica.recover). A commit made on the room with no origin - what ygo's own cleanup
// commits, here made by hand so any pinned ygo takes this path - is then a content change credited
// to whoever is connected, unless creditContentChange refuses it.
func testANilOriginCommitAfterARecover(t *testing.T) {
	service, artifactID, sh := newRenderReuseService(t)
	watch := watchNilOriginCredits(t, service, artifactID)

	// Alice types while she is the room's only browser, so the room's latest edit source is hers
	// and her authors are the only ones the durable record owes. Her keystroke is also the room's
	// first commit, which starts the replica.
	alice := newRoomPeer(t, service, artifactID, 1, reuseAlice, nil)
	typed := alice.text(nilOriginParagraph)
	alice.send(func(txn *crdt.Transaction) { typed.Insert(txn, typed.Len(), " and typed.", nil) })
	waitForLandedAppends(t, service, artifactID)
	room := service.srv.GetDoc(artifactID)
	if room == nil {
		t.Fatal("the browser loaded no room")
	}
	// Bob joins after it: with two browsers connected an edit cannot be pinned on either, so a
	// credit taken for the nil-origin commit would both name Bob and wipe the room's latest edit
	// source.
	newRoomPeer(t, service, artifactID, 2, reuseBob, nil)

	before := readCreditState(service, artifactID)
	beforePending := pendingAuthorRows(t, service.store, artifactID)
	t.Logf("before the nil-origin commit, the room: %s", before)
	t.Logf("before the nil-origin commit, the durable pending authors: %v", actorKeysOf(beforePending))
	if before.lastActor != actorKey(reuseAlice) {
		t.Fatalf("the room's latest edit source is %s, want the browser that typed alone, %s", before.lastActor, actorKey(reuseAlice))
	}

	// The replica loses its document, as the recovered-replica tests force it to: the next update
	// is one it cannot place, so recover classifies it as a content change.
	replica := service.replicaOf(room)
	if replica == nil {
		t.Fatal("the room has no replica")
	}
	replica.mu.Lock()
	replica.doc = nil
	replica.mu.Unlock()

	m := sh.measure()
	mark := watch.mark()
	commitWithNoOrigin(t, service, artifactID, func(fragment *crdt.YXmlFragment) func(*crdt.Transaction) {
		text := xmlTextWithPrefix(t, fragment, nilOriginParagraph)
		at := text.Len()
		return func(txn *crdt.Transaction) { text.Insert(txn, at, " Cleaned up.", nil) }
	})
	logged, seen := loggedSince(sh, m), watch.since(mark)
	reportLogged(t, logged, seen)
	if len(seen) != 1 || len(logged) != 1 {
		t.Fatalf("the nil-origin transaction left %d logged update(s) and %d observed commit(s), want one of each", len(logged), len(seen))
	}
	commit := seen[0]
	if commit.origin != "nil" || describeOrigin(logged[0].origin) != "nil" {
		t.Fatalf("the commit's origin is %s (the room logged %s), want nil: the transaction named no origin at all",
			commit.origin, describeOrigin(logged[0].origin))
	}
	if !commit.changed {
		t.Fatal("the recovered replica did not classify the nil-origin commit as a content change, so this test no longer reaches the credit path")
	}
	if commit.credited {
		t.Errorf("the nil-origin commit credited %v: ygo's own cleanup transaction is nobody's edit (reearth/ygo #284)", commit.authors)
	}
	after := commit.after
	if after.creditSeq != before.creditSeq {
		t.Errorf("the nil-origin commit took in-flight credit #%d, the room stood at #%d", after.creditSeq, before.creditSeq)
	}
	if !slices.Equal(after.inflight, before.inflight) {
		t.Errorf("the room's in-flight credits are %v, were %v", after.inflight, before.inflight)
	}
	if after.lastActor != before.lastActor {
		t.Errorf("the nil-origin commit left the room's latest edit source %s, want %s unchanged", after.lastActor, before.lastActor)
	}
	if after.asks != before.asks {
		t.Errorf("the nil-origin commit left %d ask-block authors, were %d", after.asks, before.asks)
	}

	waitForLandedAppends(t, service, artifactID)
	afterPending := pendingAuthorRows(t, service.store, artifactID)
	t.Logf("after the nil-origin commit, the room: %s", readCreditState(service, artifactID))
	t.Logf("after the nil-origin commit, the durable pending authors: %v", actorKeysOf(afterPending))
	if !slices.Equal(actorKeysOf(afterPending), actorKeysOf(beforePending)) {
		t.Errorf("the durable pending authors moved from %v to %v: the nil-origin commit owes no one",
			actorKeysOf(beforePending), actorKeysOf(afterPending))
	}

	// One more keystroke starts the replica again, so the room and its replica can be compared and
	// the shadow's own record checked.
	alice.send(func(txn *crdt.Transaction) { typed.Insert(txn, typed.Len(), "!", nil) })
	waitForLandedAppends(t, service, artifactID)
	sh.check(t)
	expectReplicaHoldsTheRoom(t, service, room)
}
