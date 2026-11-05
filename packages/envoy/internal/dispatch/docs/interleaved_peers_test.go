package docs

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
)

// Two peers' updates reach the room's persistence in whatever order their read loops hand them
// on, which need not be the order the room applied them in: ygo fires a transaction's update
// observers after the transaction has released the document's lock, so one peer's update can be
// handed to the room's worker while another's is still between the room's update observer, which
// counts it as pending (queueUpdateClass), and ygo's persistence observer, which hands it to the
// worker. Here the first peer's update is held between those two observers while the second
// peer's is stored. The second peer's append stores the held update first, in the room's commit
// order (flushThrough); a settlement meanwhile settles both peers' text without waiting for the
// held hand-over; and once that hand-over runs it stores nothing and leaves the room counting no
// update as pending, so nothing holds the room's settlement or its record (unusedLocked).
func TestARoomWhosePeersUpdatesReachItsPersistenceOutOfOrderSettlesBoth(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	var attempts atomic.Int64
	service.afterSettleWarm = func(string) { attempts.Add(1) }
	var first atomic.Uint64
	var holding atomic.Bool
	held, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseHeld := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseHeld)
	load := service.srv.OnLoadDocument
	service.srv.OnLoadDocument = func(ctx context.Context, room string, doc *crdt.Doc) error {
		if err := load(ctx, room, doc); err != nil {
			return err
		}
		// Registered after the room's own update observer and before ygo's persistence observer,
		// which ygo registers once OnLoadDocument returns, so it holds the first peer's update
		// counted and not yet handed to the room's worker.
		doc.OnUpdate(func([]byte, any) {
			client := crdt.ClientID(first.Load())
			if client != 0 && doc.StateVector().Clock(client) > 0 && holding.CompareAndSwap(true, false) {
				close(held)
				<-release
			}
		})
		return nil
	}
	seedServiceText(t, service, artifactID, "before")
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	alpha := connectPeer(t, server.URL, artifactID, "alice")
	beta := connectPeer(t, server.URL, artifactID, "alice")
	first.Store(uint64(alpha.Doc.ClientID()))
	// A peer holds the room's document once it has applied the room's sync step 2, which comes
	// after the step 1 it answers, so each peer's answer reaches the room ahead of its keystroke
	// and the second peer types against the room as it stood before the first peer's update.
	seeded := service.srv.GetDoc(artifactID).StateVector()
	holdsRoom := func(peer *docstest.Peer) bool {
		has := peer.Doc.StateVector()
		for client, clock := range seeded {
			if has.Clock(client) < clock {
				return false
			}
		}
		return true
	}
	waitFor(t, 10*time.Second, "both peers to hold the room's document", func() bool {
		return holdsRoom(alpha) && holdsRoom(beta)
	})

	ctx := context.Background()
	before := latestVersionNumber(t, service, artifactID)
	storedClock := func(client crdt.ClientID) uint64 {
		stored, err := service.persistence.Load(ctx, artifactID)
		if err != nil {
			t.Fatalf("load the stored document: %v", err)
		}
		document := newDocumentCopy()
		if err := crdt.ApplyUpdateV1(document, stored.Update, nil); err != nil {
			t.Fatalf("decode the stored document: %v", err)
		}
		return document.StateVector().Clock(client)
	}

	holding.Store(true)
	if _, err := alpha.Send(typeParagraph(alpha.Doc, "alpha")); err != nil {
		t.Fatalf("send the first peer's keystroke: %v", err)
	}
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the room did not apply the first peer's update")
	}
	if _, err := beta.Send(typeParagraph(beta.Doc, "beta")); err != nil {
		t.Fatalf("send the second peer's keystroke: %v", err)
	}
	waitFor(t, 10*time.Second, "the second peer's update to be stored", func() bool {
		return storedClock(beta.Doc.ClientID()) > 0
	})
	if storedClock(alpha.Doc.ClientID()) == 0 {
		t.Fatal("the second peer's update was stored ahead of the first's, which the room committed before it")
	}

	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.roomGeneration
	state.mu.Unlock()
	service.settleRoom(artifactID, generation)
	if attempts.Load() == 0 {
		t.Fatal("no settlement ran while the first peer's update was held")
	}
	if got := latestVersionNumber(t, service, artifactID); got != before+1 {
		t.Fatalf("latest version = %d, want %d: the settlement's, while the first peer's hand-over is held", got, before+1)
	}
	var markdown string
	if err := service.store.Pool.QueryRow(ctx, `
		select markdown from artifact_versions where artifact_id = $1 and number = $2
	`, artifactID, before+1).Scan(&markdown); err != nil {
		t.Fatalf("read the settled version: %v", err)
	}
	if !strings.Contains(markdown, "alpha") || !strings.Contains(markdown, "beta") {
		t.Fatalf("settled version = %q, want both peers' paragraphs", markdown)
	}

	stored := storedUpdateCount(t, service.store, artifactID)
	releaseHeld()
	waitFor(t, 10*time.Second, "the room to count no update its persistence has not taken", func() bool {
		return !service.hasPendingUpdates(artifactID) && !service.hasDurableAppend(artifactID)
	})
	if after := storedUpdateCount(t, service.store, artifactID); after != stored {
		t.Fatalf("the first peer's held hand-over left %d stored updates, want %d: it was stored already", after, stored)
	}
	assertStoredOrder(t, service, artifactID)
}

// A peer's update that waits on an item the room lacks parks every byte the room kept, and changes
// nothing, so ygo logs it with an empty Update and its bytes in LoggedUpdate.Parked but calls no
// OnUpdate for it. Dispatch drains the log after the peer's apply (onPeerUpdate) and stores the
// parked bytes then, on their own, so the edit survives a restart before its predecessor arrives:
// the store's load parks them again and integrates them once the predecessor comes. No class is
// queued for the entry, since no OnUpdate hands it to the room's persistence, so nothing is left
// pending. The replica applies Update then Parked, so a replica rebuilt from the log alone holds
// everything the live room holds.
func TestARoomsParkedOnlyCommitIsStoredAndReplaysFromItsLog(t *testing.T) {
	t.Run("valid", func(t *testing.T) { parkedOnlyCommitSurvivesReload(t, false) })
	t.Run("truncated_delete_set", func(t *testing.T) { parkedOnlyCommitSurvivesReload(t, true) })
}

func parkedOnlyCommitSurvivesReload(t *testing.T, truncated bool) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the seeded document's room: %v", err)
	}
	if owed := copyOwed(t, service, artifactID); owed != 0 {
		t.Fatalf("the settled document owes %d settlements before the test", owed)
	}
	room := service.srv.GetDoc(artifactID)
	if room == nil {
		t.Fatal("the seeded document has no resident room")
	}

	writer := crdt.New(crdt.WithClientID(7))
	text := writer.GetText("t")
	writer.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "base", nil) })
	base := crdt.EncodeStateAsUpdateV1(writer, nil)
	atBase := writer.StateVector()
	writer.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 4, " later", nil) })
	parked := crdt.EncodeStateAsUpdateV1(writer, atBase)
	if truncated {
		parked = append(append([]byte(nil), parked[:len(parked)-1]...), 1, 42)
	}

	var updates atomic.Int64
	room.OnUpdate(func([]byte, any) { updates.Add(1) })
	stored := storedUpdateCount(t, service.store, artifactID)
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	peer := connectPeer(t, server.URL, artifactID, "alice")
	if err := peer.Write(ygsync.EncodeUpdate(parked)); err != nil {
		t.Fatalf("send the later update ahead of its predecessor: %v", err)
	}
	// A peer update that only parks has no OnUpdate, so the peer path drains the log itself and
	// stores the parked bytes (onPeerUpdate). A shutdown in that window must not lose an edit the
	// room accepted.
	waitFor(t, 10*time.Second, "the parked peer update to become durable", func() bool {
		return storedUpdateCount(t, service.store, artifactID) > stored
	})
	if updates.Load() != 0 {
		t.Fatalf("the parked-only commit called OnUpdate %d time(s), want none", updates.Load())
	}
	if pending := room.PendingStats(); pending.Items == 0 {
		t.Fatalf("the room parked no item after its later update: %+v", pending)
	}
	if service.hasPendingUpdates(artifactID) || service.hasDurableAppend(artifactID) {
		t.Fatal("the parked-only commit left the room counting a store write")
	}
	// Parked bytes change nothing a settlement reads, so storing them marks no settlement owed:
	// the commit that integrates them marks it.
	if owed := copyOwed(t, service, artifactID); owed != 0 {
		t.Fatal("storing the parked bytes marked the document owing a settlement")
	}
	replica := service.replicaOf(room)
	if replica == nil {
		t.Fatal("the room has no rendered replica")
	}
	var applyErr error
	loaded, err := service.persistence.Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load the parked update before its predecessor arrives: %v", err)
	}
	restarted := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(restarted, loaded.Update, nil); err != nil {
		t.Fatalf("open the parked update after restart: %v", err)
	}
	if err := crdt.ApplyUpdateV1(restarted, base, nil); err != nil {
		t.Fatalf("send the predecessor after restart: %v", err)
	}
	if got := restarted.GetText("t").ToString(); got != "base later" {
		t.Fatalf("the restart then predecessor holds %q, want the parked successor too", got)
	}
	// The stored-order check reads the stored parked edit as one awaiting its predecessor, not as
	// a row stored out of order: nothing the store holds disagrees with what the room accepted.
	if report := assertStoredOrder(t, service, artifactID); report.UpdatesAwaitingPredecessor != 1 {
		t.Fatalf("stored-order report before the predecessor = %+v, want one update awaiting its predecessor", report)
	}

	if err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		applyErr = crdt.ApplyUpdateV1(doc, base, "peer")
	}); err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		t.Fatalf("apply the predecessor that drains the parked update: %v", err)
	}
	if applyErr != nil {
		t.Fatalf("apply the predecessor update: %v", applyErr)
	}
	waitFor(t, 10*time.Second, "the room to count no persistence update after its parked-only commit", func() bool {
		return !service.hasPendingUpdates(artifactID) && !service.hasDurableAppend(artifactID)
	})
	// The predecessor's commit stores the successor's structs again, in its own update: a load of
	// the whole history holds each once.
	history, err := service.persistence.Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load the history once the predecessor is stored: %v", err)
	}
	reloaded := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(reloaded, history.Update, nil); err != nil {
		t.Fatalf("open the stored history: %v", err)
	}
	if got := reloaded.GetText("t").ToString(); got != "base later" {
		t.Fatalf("the stored history holds %q, want %q", got, "base later")
	}
	if pending := reloaded.PendingStats(); pending.Items != 0 {
		t.Fatalf("the stored history still parks %+v once the predecessor is stored", pending)
	}
	// The predecessor's commit carries the parked edit again, so the check finds the history
	// replays: no row out of order and nothing left awaiting.
	if report := assertStoredOrder(t, service, artifactID); report.UpdatesAwaitingPredecessor != 0 {
		t.Fatalf("stored-order report once the predecessor is stored = %+v, want nothing awaiting", report)
	}

	replayed := replica.hold()
	if replayed == nil {
		t.Fatal("the rendered replica did not drain the room's log")
	}
	defer replica.mu.Unlock()
	live, err := snapshotDocument(room)
	if err != nil {
		t.Fatalf("copy the live room: %v", err)
	}
	if got, want := crdt.EncodeStateAsUpdateV1(replayed, nil), crdt.EncodeStateAsUpdateV1(live, nil); !bytes.Equal(got, want) {
		t.Fatal("the replica rebuilt from the log does not hold the live room's state")
	}
}

// A peer's update can change the room and park at once: one client's structs integrate, another's
// wait for an edit the room lacks. ygo hands OnUpdate, and so the room's persistence, only what the
// commit integrated, and logs what it parked in LoggedUpdate.Parked. Dispatch stores those parked
// bytes too, as it stores a commit that only parked, so a restart before the predecessor arrives
// keeps both clients' edits.
func TestAPeerUpdateThatChangesTheRoomAndParksKeepsBothAcrossARestart(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the seeded document's room: %v", err)
	}

	now := crdt.New(crdt.WithClientID(7))
	nowText := now.GetText("a")
	now.Transact(func(txn *crdt.Transaction) { nowText.Insert(txn, 0, "now", nil) })
	integrates := crdt.EncodeStateAsUpdateV1(now, nil)
	other := crdt.New(crdt.WithClientID(8))
	otherText := other.GetText("b")
	other.Transact(func(txn *crdt.Transaction) { otherText.Insert(txn, 0, "base", nil) })
	predecessor := crdt.EncodeStateAsUpdateV1(other, nil)
	atBase := other.StateVector()
	other.Transact(func(txn *crdt.Transaction) { otherText.Insert(txn, 4, " later", nil) })
	waits := crdt.EncodeStateAsUpdateV1(other, atBase)
	mixed, err := crdt.MergeUpdatesV1(integrates, waits)
	if err != nil {
		t.Fatalf("merge the two clients' updates: %v", err)
	}

	stored := storedUpdateCount(t, service.store, artifactID)
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	peer := connectPeer(t, server.URL, artifactID, "alice")
	if err := peer.Write(ygsync.EncodeUpdate(mixed)); err != nil {
		t.Fatalf("send an update that changes the room and parks: %v", err)
	}
	waitFor(t, 10*time.Second, "both parts of the peer's update to be stored", func() bool {
		return storedUpdateCount(t, service.store, artifactID) >= stored+2
	})
	if pending := service.srv.GetDoc(artifactID).PendingStats(); pending.Items == 0 {
		t.Fatalf("the room parked nothing of the update: %+v", pending)
	}
	waitFor(t, 10*time.Second, "the room to count no persistence update", func() bool {
		return !service.hasPendingUpdates(artifactID) && !service.hasDurableAppend(artifactID)
	})

	loaded, err := service.persistence.Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load the history before the predecessor arrives: %v", err)
	}
	restarted := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(restarted, loaded.Update, nil); err != nil {
		t.Fatalf("open the stored history after a restart: %v", err)
	}
	if err := crdt.ApplyUpdateV1(restarted, predecessor, nil); err != nil {
		t.Fatalf("send the predecessor after the restart: %v", err)
	}
	if got := restarted.GetText("a").ToString(); got != "now" {
		t.Fatalf("the restarted room's integrated edit = %q, want %q", got, "now")
	}
	if got := restarted.GetText("b").ToString(); got != "base later" {
		t.Fatalf("the restart then predecessor holds %q, want the parked edit too", got)
	}
	assertStoredOrder(t, service, artifactID)
}

// typeParagraph is one keystroke's transaction that types text into a new first paragraph.
func typeParagraph(document *crdt.Doc, text string) func(*crdt.Transaction) {
	fragment := document.GetXmlFragment(fragmentName)
	return func(txn *crdt.Transaction) {
		paragraph := crdt.NewYXmlElement("paragraph")
		fragment.InsertElement(txn, 0, paragraph)
		run := crdt.NewYXmlText()
		paragraph.InsertText(txn, 0, run)
		run.Insert(txn, 0, text, nil)
	}
}
