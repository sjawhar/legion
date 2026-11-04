package docs

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// pastedAskBlock is the ask block a browser pastes: its own block id, the client-owned attributes
// the paste carries, and no answer.
const pastedAskBlock = ":::ask{#d2 urgency=\"med\" multiple=\"false\"}\nWhich one?\n\n- A\n- B\n:::\n"

// browserAskServerAttributes is what the browser editor writes into the room for the server-owned
// attributes of a block it pastes. Its schema declares every one of them, and it writes each
// attribute of the node it builds: the one its schema gives a default, state, with that default,
// and each of the others as a null, which the room keeps as an attribute whose value is null.
// A tree read of the room holds those nulls as attributes that are present
// (crdt.YXmlElement.GetAttributeValues), while pmdoc reads a null attribute and an absent one as
// one attribute set (attrsEqual), so a write that only takes them off leaves the document as it
// was.
var browserAskServerAttributes = map[string]any{
	"state":                "open",
	"answered_by":          nil,
	"answered_at":          nil,
	"selected":             nil,
	"answer":               nil,
	"invalid":              nil,
	"copied_from":          nil,
	"copied_from_document": nil,
}

// A browser pastes an ask block whose server-owned attributes the room holds as nulls. Settlement
// repairs those attributes - taking the nulls off and leaving the state it indexed the ask at -
// which changes the tree it writes them into and leaves the document as it was, so ygo commits no
// update for the write and it comes back with no suppression slot. That is not the document
// moving: the ask's row, its ask.opened event and the pending-settlement row are the pass's to
// commit, and a settlement that read the missing update as a move planned the same repairs again
// until its pass bound, leaving the ask unrecorded (INFRA-592).
func TestAPastedAskBlockWhoseServerAttributesAreNullOpensItsAsk(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "End.\n")
	settleCurrentGeneration(t, service, artifactID)
	if owed := copyOwed(t, service, artifactID); owed != 0 {
		t.Fatalf("the settled document owes %d settlements before the paste", owed)
	}
	live := service.srv.GetDoc(artifactID)
	if live == nil {
		t.Fatal("the seeded document has no resident room")
	}
	// The passes of each settlement of this room, so a settlement that reads its repairs as a
	// move - which costs a pass for the plan and a pass for the repairs, every time - is told
	// from one that commits them.
	var mu sync.Mutex
	passes, most := 0, 0
	service.beforeSettleStart = func(room string) {
		if room != artifactID {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		passes = 0
	}
	service.beforeSettleTake = func(room string) {
		if room != artifactID {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		passes++
		most = max(most, passes)
	}

	peer := newRoomPeer(t, service, artifactID, 2, model.Actor{Kind: "user", ID: "alice"}, nil)
	before := peer.doc.StateVector()
	tree, err := treeOf(peer.doc)
	if err != nil {
		t.Fatalf("read the peer's document: %v", err)
	}
	pasted, err := pmdoc.Parse(pastedAskBlock)
	if err != nil {
		t.Fatalf("parse the pasted ask block: %v", err)
	}
	// The editor stamps a block id on every block it creates and keeps the one the paste carries.
	pmdoc.EnsureBlockIDs(pasted)
	tree.Children = append(tree.Children, pasted.Children...)
	var updateErr error
	peer.doc.Transact(func(txn *crdt.Transaction) { updateErr = pmdoc.Update(txn, peer.fragment, tree) })
	if updateErr != nil {
		t.Fatalf("write the pasted ask block into the peer's document: %v", updateErr)
	}
	ask := askElementIn(t, peer.fragment)
	peer.doc.Transact(func(txn *crdt.Transaction) {
		for _, name := range askServerAttributeNames {
			ask.SetAttributeValue(txn, name, browserAskServerAttributes[name])
		}
	})
	// Both transactions are one paste, so the room takes them as the one update the browser's
	// connection sends (sendPeerUpdate, Service.onPeerUpdate).
	sendPeerUpdate(t, service, artifactID, live, crdt.EncodeStateAsUpdateV1(peer.doc, before))
	if got := askServerAttributesInRoom(t, live); got["answer"] != nil || len(got) != len(browserAskServerAttributes) {
		t.Fatalf("the room holds the pasted block's server attributes as %v, want every one of them, the empty ones null", got)
	}
	settleCurrentGeneration(t, service, artifactID)

	var state, question string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select state, question from asks where block_artifact_id = $1 and block_id = 'd2'
	`, artifactID).Scan(&state, &question); err != nil {
		t.Fatalf("read the pasted block's ask: %v", err)
	}
	if state != "open" || question != "Which one?" {
		t.Fatalf("the pasted block's ask is %q asking %q, want an open \"Which one?\"", state, question)
	}
	var opened int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select count(*) from events e
		join asks a on e.payload->>'id' = a.id::text
		where e.type = 'ask.opened' and a.block_artifact_id = $1 and a.block_id = 'd2'
	`, artifactID).Scan(&opened); err != nil {
		t.Fatalf("read the pasted block's ask.opened events: %v", err)
	}
	if opened != 1 {
		t.Fatalf("the pasted block's ask has %d ask.opened event(s), want one", opened)
	}
	if owed := copyOwed(t, service, artifactID); owed != 0 {
		t.Fatalf("the settled document still owes %d settlements", owed)
	}
	mu.Lock()
	defer mu.Unlock()
	if most >= maxSettlePasses {
		t.Fatalf("a settlement of the pasted ask ran %d passes, want fewer than %d: it read its own repairs as the document moving", most, maxSettlePasses)
	}
}

// askElementIn is the one ask element under fragment, a document's fragment.
func askElementIn(t *testing.T, fragment *crdt.YXmlFragment) *crdt.YXmlElement {
	t.Helper()
	var found []*crdt.YXmlElement
	var walk func(parent *crdt.YXmlFragment)
	walk = func(parent *crdt.YXmlFragment) {
		for _, child := range parent.Children() {
			element, ok := child.(*crdt.YXmlElement)
			if !ok {
				continue
			}
			if element.NodeName == "ask" {
				found = append(found, element)
			}
			walk(&element.YXmlFragment)
		}
	}
	walk(fragment)
	if len(found) != 1 {
		t.Fatalf("the document holds %d ask elements, want one", len(found))
	}
	return found[0]
}

// askServerAttributesInRoom is the server-owned attributes the room's ask element holds, each
// name the element carries with the value it carries, null included.
func askServerAttributesInRoom(t *testing.T, live *crdt.Doc) map[string]any {
	t.Helper()
	held := make(map[string]any)
	values := askElementIn(t, live.GetXmlFragment(fragmentName)).GetAttributeValues()
	for _, name := range askServerAttributeNames {
		if value, ok := values[name]; ok {
			held[name] = value
		}
	}
	return held
}
