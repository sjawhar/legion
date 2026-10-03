package docs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
)

// Two peers' updates reach the room's persistence in whatever order their read loops hand them
// on, which need not be the order the room applied them in: ygo fires a transaction's update
// observers after the transaction has released the document's lock, so one peer's update can be
// stored while another's is still between the room's update observer, which counts it as pending
// (recordUpdateClass), and ygo's persistence observer, which hands it to the room's worker.
// Settlement reads the room only once that count is zero, so a count an out-of-order store left
// behind would hold the room's settlement off for good: its edits unversioned and its
// pending-settlement row retried forever. Here the first peer's update is held between those two
// observers while the second peer's is stored. The room counts the held update, a settlement
// meanwhile writes nothing, and once the held update is stored the room settles both peers' text.
func TestARoomWhosePeersUpdatesAreStoredOutOfOrderSettlesBoth(t *testing.T) {
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
	alpha := newDeepPeer(t, server.URL, artifactID)
	beta := newDeepPeer(t, server.URL, artifactID)
	first.Store(uint64(alpha.doc.ClientID()))
	// A peer holds the room's document once it has applied the room's sync step 2, which comes
	// after the step 1 it answers, so each peer's answer reaches the room ahead of its keystroke
	// and the second peer types against the room as it stood before the first peer's update.
	seeded := service.srv.GetDoc(artifactID).StateVector()
	holdsRoom := func(peer *deepPeer) bool {
		has := peer.doc.StateVector()
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
	latest := func() int {
		var number int
		if err := service.store.Pool.QueryRow(ctx, `
			select coalesce(max(number), 0) from artifact_versions where artifact_id = $1
		`, artifactID).Scan(&number); err != nil {
			t.Fatalf("read the latest version: %v", err)
		}
		return number
	}
	before := latest()
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
	alpha.send(t, typeParagraph(alpha.doc, "alpha"))
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the room did not apply the first peer's update")
	}
	beta.send(t, typeParagraph(beta.doc, "beta"))
	waitFor(t, 10*time.Second, "the second peer's update to be stored", func() bool {
		return storedClock(beta.doc.ClientID()) > 0
	})
	if storedClock(alpha.doc.ClientID()) != 0 {
		t.Fatal("the first peer's update was stored while it was held")
	}

	state := service.room(artifactID)
	state.mu.Lock()
	generation := state.gen
	state.mu.Unlock()
	service.settleRoom(artifactID, generation)
	if attempts.Load() == 0 {
		t.Fatal("no settlement ran while the first peer's update was held")
	}
	if got := latest(); got != before {
		t.Fatalf("a settlement wrote version %d while the first peer's update was not stored", got)
	}

	releaseHeld()
	waitForDocumentVersion(t, service.store, artifactID, before+1)
	var markdown string
	if err := service.store.Pool.QueryRow(ctx, `
		select markdown from artifact_versions where artifact_id = $1 and number = $2
	`, artifactID, before+1).Scan(&markdown); err != nil {
		t.Fatalf("read the settled version: %v", err)
	}
	if !strings.Contains(markdown, "alpha") || !strings.Contains(markdown, "beta") {
		t.Fatalf("settled version = %q, want both peers' paragraphs", markdown)
	}
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
