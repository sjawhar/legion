package docs

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A room's document decodes its stored state with the pending cap lifted, and ygo sets the room's
// cap, maxUpdateItems, once the state is in and before OnLoadDocument (sjawhar/ygo v1.51.3-sami.7,
// provider/websocket/server.go:1702, :1766-1773), so every peer's apply after the load is held to
// it. A room loads a stored state that parks structs and delete ranges; a browser then sends, over
// the document socket, an update parking delete ranges up to the cap exactly, which the room
// applies where ygo's default cap of 100,000 would refuse it, and one parking a range more, which
// the room refuses, as a lifted cap would not, leaving its parked queue as it was and the browser
// connected. The two reach the room's own apply of a peer update with the hook's weighing passed
// over for them alone: the struct total counts each parked range splitsAStructCanCause times within
// the same cap and refuses both first (weighPeerUpdate), so only ygo's apply can show the cap
// itself.
//
// The copies the service decodes a document into take what the room holds as they took it before
// the cap counted delete ranges: a cold read decodes the stored state that parks structs and delete
// ranges, and the copy a write's fork is made from (snapshotDocument) decodes the room holding as
// many parked delete ranges as the cap allows. The fold of a stored log that parks both is
// TestALoadAndACompactionKeepWhatTheDocumentParked.
func TestAPeerApplyIsHeldToTheRoomsPendingCapAfterTheLoad(t *testing.T) {
	const (
		parkedStructs = 1_000
		parkedRanges  = 1_000
		// The clients of the parked structs and of the three sets of ranges: none of them is a
		// client of the document's seed, so the structs wait behind client 8's clock 0 and every
		// range waits for structs the room never receives.
		structClient, storedRangeClient, capRangeClient, pastRangeClient = 8, 11, 12, 13
	)
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()

	tree, err := pmdoc.Parse("# First\n\nOne paragraph of ordinary prose.\n")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := encodeDocumentTree(tree)
	if err != nil {
		t.Fatal(err)
	}
	history := newDocumentCopy()
	parkedFrame := newV1Frame(1).group(parkedStructs, structClient, 1)
	for range parkedStructs {
		parkedFrame.gc(1)
	}
	for _, update := range [][]byte{seed, parkedFrame.noDeletes(), deleteRangesFrom(storedRangeClient, parkedRanges)} {
		if err := crdt.ApplyUpdateV1(history, update, nil); err != nil {
			t.Fatal(err)
		}
	}
	if stats := history.PendingStats(); stats.Items != parkedStructs || stats.DeleteRanges != parkedRanges {
		t.Fatalf("the stored state parks %d structs and %d delete ranges, want %d and %d", stats.Items, stats.DeleteRanges, parkedStructs, parkedRanges)
	}
	storeHistory(t, service, artifactID, crdt.EncodeStateAsUpdateV1(history, nil))
	if text, err := service.Text(ctx, artifactID); err != nil || !strings.Contains(text, "First") {
		t.Fatalf("a cold read of the stored state that parks structs and delete ranges reads %q (%v), want the document", text, err)
	}

	atTheCap := deleteRangesFrom(capRangeClient, maxUpdateItems-parkedRanges)
	pastTheCap := deleteRangesFrom(pastRangeClient, 1)
	applies := make(chan error, 2)
	weigh := service.srv.OnPeerUpdate
	service.srv.OnPeerUpdate = func(info websocket.PeerUpdateInfo, apply func() error) error {
		if !bytes.Equal(info.Update, atTheCap) && !bytes.Equal(info.Update, pastTheCap) {
			return weigh(info, apply)
		}
		err := apply()
		applies <- err
		return err
	}
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	browser := connectPeer(t, server.URL, artifactID, "alice")
	awaitAnswer(t, browser, "the document loaded from the stored state")
	room := roomDocument(t, service, artifactID)
	if stats := room.PendingStats(); stats.Items != parkedStructs || stats.DeleteRanges != parkedRanges {
		t.Fatalf("the loaded room parks %d structs and %d delete ranges, want the stored state's %d and %d", stats.Items, stats.DeleteRanges, parkedStructs, parkedRanges)
	}

	send := func(t *testing.T, what string, update []byte) error {
		t.Helper()
		if err := browser.Write(ygsync.EncodeUpdate(update)); err != nil {
			t.Fatalf("send %s: %v", what, err)
		}
		select {
		case err := <-applies:
			return err
		case <-browser.Ended:
			t.Fatalf("the room ended the browser's connection before applying %s: %v", what, browser.Closed())
		case <-time.After(time.Minute):
			t.Fatalf("the room never applied %s", what)
		}
		return nil
	}
	if err := send(t, "the update parking delete ranges up to the cap", atTheCap); err != nil {
		t.Fatalf("the room refused an update parking %d delete ranges, up to its %d cap: %v", maxUpdateItems-parkedRanges, maxUpdateItems, err)
	}
	if stats := room.PendingStats(); stats.DeleteRanges != maxUpdateItems || stats.Items != parkedStructs {
		t.Fatalf("the room parks %d delete ranges and %d structs, want the %d cap and the %d it loaded", stats.DeleteRanges, stats.Items, maxUpdateItems, parkedStructs)
	}
	if err := send(t, "the update parking one delete range past the cap", pastTheCap); !errors.Is(err, crdt.ErrInvalidUpdate) {
		t.Fatalf("the room answered %v to an update parking one delete range past its %d cap, want ygo's refusal", err, maxUpdateItems)
	}
	if stats := room.PendingStats(); stats.DeleteRanges != maxUpdateItems || stats.Items != parkedStructs {
		t.Fatalf("the refused update left the room parking %d delete ranges and %d structs, want the %d and %d before it", stats.DeleteRanges, stats.Items, maxUpdateItems, parkedStructs)
	}
	select {
	case <-browser.Ended:
		t.Fatalf("the room closed the browser whose update its pending cap refused: %v", browser.Closed())
	case <-time.After(200 * time.Millisecond):
	}

	snapshot, err := snapshotDocument(room)
	if err != nil {
		t.Fatalf("copy the room holding %d parked delete ranges: %v", maxUpdateItems, err)
	}
	if stats := snapshot.PendingStats(); stats.DeleteRanges != maxUpdateItems || stats.Items != parkedStructs {
		t.Fatalf("the copy parks %d delete ranges and %d structs, want the room's %d and %d", stats.DeleteRanges, stats.Items, maxUpdateItems, parkedStructs)
	}
}
