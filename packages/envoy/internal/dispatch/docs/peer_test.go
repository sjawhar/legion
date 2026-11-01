package docs

import (
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// roomPeer is a browser's editor on a room: its own copy of the document, synced from the room
// once, which sends the room the update each edit it makes leaves, as one of the room's connected
// peers, and takes in what another wrote when the room broadcasts it (sync). It can hold an edit
// back before it sends it (edit, flush), so two peers can edit one state concurrently and the room
// merge both. Both the typing script (TestRenderReuseABrowserTypingScript) and the edit storm
// (TestRenderReuseARandomisedEditStorm) drive their peers through it, so a fix to the peer's
// protocol reaches both.
type roomPeer struct {
	t          *testing.T
	service    *Service
	artifactID string
	doc        *crdt.Doc
	fragment   *crdt.YXmlFragment
	// pending is the state vector the peer's unsent edits started from, nil with none.
	pending crdt.StateVector
	// sent is every update the room has been sent, by its bytes, shared by the peers of one room:
	// a delete set names the client whose structs it deletes, not the peer that deleted them, so
	// two peers deleting one range encode the same bytes.
	sent map[string]bool
}

// newRoomPeer connects a peer on conn to the room of artifactID, as actor, with its own copy of
// the room's document. sent is the record of the updates the room has already been sent, shared
// by the room's peers; a caller with one peer passes nil for a record of its own.
func newRoomPeer(t *testing.T, service *Service, artifactID string, conn uint64, actor model.Actor, sent map[string]bool) *roomPeer {
	t.Helper()
	if err := service.warmLiveDocument(t.Context(), artifactID); err != nil {
		t.Fatalf("warm the room for %s: %v", actor.ID, err)
	}
	service.addConnection(artifactID, conn, actor)
	t.Cleanup(func() { service.removeConnection(artifactID, conn) })
	if sent == nil {
		sent = map[string]bool{}
	}
	p := &roomPeer{t: t, service: service, artifactID: artifactID, doc: crdt.New(), sent: sent}
	p.fragment = p.doc.GetXmlFragment(fragmentName)
	p.sync()
	return p
}

// sync takes in what the room holds that the peer's document does not.
func (p *roomPeer) sync() {
	p.t.Helper()
	room := p.service.srv.GetDoc(p.artifactID)
	if err := crdt.ApplyUpdateV1(p.doc, crdt.EncodeStateAsUpdateV1(room, p.doc.StateVector()), nil); err != nil {
		p.t.Fatalf("sync the peer from the room: %v", err)
	}
}

// edit makes edit in the peer's own document, in one transaction, without sending it.
func (p *roomPeer) edit(edit func(txn *crdt.Transaction)) {
	p.t.Helper()
	if p.pending == nil {
		p.pending = p.doc.StateVector()
	}
	p.doc.Transact(edit)
}

// flush sends the room every edit the peer has held back, as one update, unless the peer has sent
// those very bytes before: an edit that creates no struct and only deletes again what the peer has
// already deleted encodes to the update it sent then, and applying one twice moves the room
// nowhere, while the room would log and classify a second commit for it, which a shadow reads as
// two classes of one update.
func (p *roomPeer) flush() {
	p.t.Helper()
	if p.pending == nil {
		return
	}
	before := p.pending
	p.pending = nil
	room := p.service.srv.GetDoc(p.artifactID)
	update := crdt.EncodeStateAsUpdateV1(p.doc, before)
	if p.sent[string(update)] {
		return
	}
	p.sent[string(update)] = true
	if err := crdt.ApplyUpdateV1(room, update, "peer"); err != nil {
		p.t.Fatalf("apply the peer's update to the room: %v", err)
	}
}

// send is edit then flush: the peer's usual keystroke, which the room commits in one transaction
// of its own.
func (p *roomPeer) send(edit func(txn *crdt.Transaction)) {
	p.t.Helper()
	p.edit(edit)
	p.flush()
}

// editTree sends the room edit of the peer's tree, written as pmdoc.Update writes a tree, with
// every block the edit left without one stamped with a block id as the browser editor stamps one.
// An edit that answers no tree sends nothing.
func (p *roomPeer) editTree(edit func(*pmdoc.Node) *pmdoc.Node) {
	p.t.Helper()
	tree, err := treeOf(p.doc)
	if err != nil {
		p.t.Fatalf("read the peer's document: %v", err)
	}
	want := edit(tree)
	if want == nil {
		return
	}
	pmdoc.EnsureBlockIDs(want)
	var updateErr error
	p.send(func(txn *crdt.Transaction) { updateErr = pmdoc.Update(txn, p.fragment, want) })
	if updateErr != nil {
		p.t.Fatalf("edit the peer's document: %v", updateErr)
	}
}

// text is the text node of the peer's document whose text starts with prefix, wherever it stands.
func (p *roomPeer) text(prefix string) *crdt.YXmlText {
	p.t.Helper()
	return xmlTextWithPrefix(p.t, p.fragment, prefix)
}

// xmlTextWithPrefix is the text node under fragment whose text starts with prefix, wherever it
// stands. It finds in any document's fragment: a peer's (roomPeer.text), a room's (roomText) or
// a copy of one.
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

// texts is every text node under the peer's document fragment, in document order.
func (p *roomPeer) texts() []*crdt.YXmlText { return textsUnder(p.fragment, false) }

// markableTexts is every text node under the peer's document fragment that a mark may cover: the
// schema carries no mark inside a code block.
func (p *roomPeer) markableTexts() []*crdt.YXmlText { return textsUnder(p.fragment, true) }

// textsUnder is every text node under fragment, in document order. markable leaves out the text
// of a code block, which the schema carries no mark in. It walks any document's fragment: a
// peer's, a room's, or the room's replica's (replicaHoldsTheRoom).
func textsUnder(fragment *crdt.YXmlFragment, markable bool) []*crdt.YXmlText {
	var found []*crdt.YXmlText
	var walk func(parent *crdt.YXmlFragment)
	walk = func(parent *crdt.YXmlFragment) {
		for _, child := range parent.Children() {
			switch node := child.(type) {
			case *crdt.YXmlText:
				found = append(found, node)
			case *crdt.YXmlElement:
				if markable && node.NodeName == "code_block" {
					continue
				}
				walk(&node.YXmlFragment)
			}
		}
	}
	walk(fragment)
	return found
}
