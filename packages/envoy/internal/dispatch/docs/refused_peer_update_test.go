package docs

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A peer's update the room refuses leaves the room's document as every other peer of the room holds
// it. The room refuses an update whose delete set would park more delete ranges than its pending
// queue takes; a refusal that comes after the update has already deleted text the room holds leaves
// the room with a deletion it told the sender failed and sent no other peer, so the document the room
// stores and settles is no longer the one every open editor shows. Here one peer's update deletes
// text the room holds and names one delete range more than the room's pending queue takes, for a
// client the room has never seen; an ordinary keystroke the same connection sends next, which needs
// nothing of the refused update, then reaches the other peer, and the room and that peer must read
// the same document.
func TestARefusedPeerUpdateLeavesTheRoomAsItsOtherPeersHoldIt(t *testing.T) {
	service, artifactID := newTestService(t)
	shrinkPendingItems(t, service, nil)
	seedServiceText(t, service, artifactID, "hello world")
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	alpha := connectPeer(t, server.URL, artifactID, "alice")
	beta := connectPeer(t, server.URL, artifactID, "alice")
	waitFor(t, 10*time.Second, "both peers to hold the room's document", func() bool {
		return strings.TrimSpace(peerMarkdown(alpha)) == "hello world" && strings.TrimSpace(peerMarkdown(beta)) == "hello world"
	})

	// The keystroke sent after the refused update is made on a copy of the room's document as the
	// peers hold it now, so the room can integrate it whatever became of the refused one.
	unrelated := crdt.New()
	if err := crdt.ApplyUpdateV1(unrelated, crdt.EncodeStateAsUpdateV1(alpha.Doc, nil), nil); err != nil {
		t.Fatalf("copy the peer's document: %v", err)
	}
	after := docstest.Transact(unrelated, typeParagraph(unrelated, "after"))

	fragment := alpha.Doc.GetXmlFragment(fragmentName)
	var editErr error
	deletion := docstest.Transact(alpha.Doc, func(txn *crdt.Transaction) {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			editErr = err
			return
		}
		if len(tree.Children) != 1 || len(tree.Children[0].Children) != 1 {
			editErr = errors.New("the document is not one paragraph of one text run")
			return
		}
		tree.Children[0].Children[0].Text = "hello"
		editErr = pmdoc.Update(txn, fragment, tree)
	})
	if editErr != nil || deletion == nil {
		t.Fatalf("delete the text: update=%d bytes err=%v", len(deletion), editErr)
	}
	refused, err := crdt.MergeUpdatesV1(deletion, unknownDeleteRanges(testPendingItems+1))
	if err != nil {
		t.Fatalf("merge the deletion with the delete ranges: %v", err)
	}

	if err := alpha.Write(ygsync.EncodeUpdate(refused)); err != nil {
		t.Fatalf("send the update the room refuses: %v", err)
	}
	if err := alpha.Write(ygsync.EncodeUpdate(after)); err != nil {
		t.Fatalf("send the keystroke after it: %v", err)
	}
	waitFor(t, 10*time.Second, "the other peer to receive the keystroke sent after the refused update", func() bool {
		return strings.HasPrefix(peerMarkdown(beta), "after")
	})

	room := currentLiveMarkdown(t, service, artifactID)
	if other := peerMarkdown(beta); room != other {
		t.Fatalf("the room reads %q but its other peer reads %q: the room kept part of an update it sent no other peer", room, other)
	}
}

// unknownDeleteRanges is an update holding only a delete set: ranges non-adjacent delete ranges of a
// client no room has seen, each of which a room that receives it parks until the client's structs
// arrive.
func unknownDeleteRanges(ranges int) []byte {
	far := crdt.New(crdt.WithClientID(0x7fff0001))
	text := far.GetText("far")
	far.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 0, strings.Repeat("x", 2*ranges), nil)
	}, nil)
	far.Transact(func(txn *crdt.Transaction) {
		for index := ranges - 1; index >= 0; index-- {
			text.Delete(txn, 2*index, 1)
		}
	}, nil)
	// Encoded against the client's own state, the update carries none of its structs: the delete
	// set alone.
	return crdt.EncodeStateAsUpdateV1(far, far.StateVector())
}
