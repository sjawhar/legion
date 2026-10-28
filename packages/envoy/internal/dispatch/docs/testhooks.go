package docs

import (
	"context"
	"fmt"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// InjectSchemaInvalidForTest writes the malformed element a crafted client can persist. It is
// mounted only through Dispatch's test-hook route.
func (s *Service) InjectSchemaInvalidForTest(ctx context.Context, artifactID string) error {
	return s.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fragment := doc.GetXmlFragment(fragmentName)
		transact(func(transaction *crdt.Transaction) {
			fragment.InsertElement(transaction, 0, crdt.NewYXmlElement("callout"))
		})
	})
}

// SetRoomBudgetForTest replaces the room budget's limit with bytes, or restores roomBudgetBytes for
// zero, so a browser test can fill the budget with documents a test can type into. It is mounted
// only through Dispatch's test-hook route; the real-process drives use the real budget.
func (s *Service) SetRoomBudgetForTest(bytes int64) int64 {
	if bytes <= 0 {
		bytes = roomBudgetBytes
	}
	s.budget.setLimit(bytes)
	return bytes
}

// RoomLiveForTest reports whether artifactID's room is resident in this server, which a
// real-process drive reads to see that a room was closed or refused. It reads the server's rooms
// and nothing else, so it can neither load nor rebuild a document. It is mounted only through
// Dispatch's test-hook route.
func (s *Service) RoomLiveForTest(artifactID string) bool {
	return s.srv.GetDoc(artifactID) != nil
}

// The tangle UseUpScanTotalForTest appends: a chain of usedUpChain one-character items, each of a
// client of its own from usedUpChainClient on, and items of client usedUpScanClient that each scan
// the chain to its end. A browser's client ID is a random 32-bit number, so these few collide
// with one no more often than any two browsers' do.
const (
	usedUpChain       = 2_000
	usedUpChainClient = 1_000
	usedUpScanClient  = 4_000_000
)

// UseUpScanTotalForTest closes artifactID's room if this server holds it and appends to its stored
// history one update whose structs make a re-scan of the history pass roomScanTotal: after the
// last character of the document's last text, a chain of usedUpChain one-character items, each of
// a client of its own so none merges with its neighbour, and then items of one client each naming
// the chain's first as its left origin and no right origin, so each scans the chain to its end
// (Item.integrate, sjawhar/ygo v1.51.3-sami.7, crdt/item.go:117-192). The room the next load
// makes counts its scan total from that re-scan (servicePersistenceAdapter.LoadDoc), so it is
// used up: typing in place goes on within usedUpScanBudget, a larger apply is refused
// DOC_NEEDS_REBUILD, and a rebuild takes the document (nearACap). A real document reaches that
// only past about fifteen honest long sessions (roomScanTotal), which no browser test can type.
// The document reads the chain's "a"s and the scanners' "k"s at the end of its last text. It
// refuses to return without error unless the stored history's re-scan reaches roomScanTotal. It
// is mounted only through Dispatch's test-hook route.
func (s *Service) UseUpScanTotalForTest(ctx context.Context, artifactID string) error {
	if err := s.Evict(ctx, artifactID); err != nil {
		return err
	}
	loaded, err := s.persistence.Load(ctx, artifactID)
	if err != nil {
		return err
	}
	doc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		return err
	}
	text := lastWrittenText(doc.GetXmlFragment(fragmentName))
	if text == nil {
		return fmt.Errorf("document %s holds no text to chain onto", artifactID)
	}
	last := crdt.CreateRelativePositionFromIndex(text, text.Len()-1, 0).Item
	if last == nil {
		return fmt.Errorf("document %s's last text has no item for its last character", artifactID)
	}
	// Each scanner passes the chain after its first item and stops at the scanner before it, of
	// its own client and naming the same origins: about usedUpChain scans each.
	scanners := roomScanTotal/usedUpChain + 2
	frame := encoding.NewEncoder()
	frame.WriteVarUint(usedUpChain + 1) // client groups: the chain's, then the scanners'
	origin := *last
	for i := range usedUpChain {
		client := uint64(usedUpChainClient + i)
		frame.WriteVarUint(1) // structs in the group
		frame.WriteVarUint(client)
		frame.WriteVarUint(0) // first clock
		writeOriginString(frame, origin, "a")
		origin = crdt.ID{Client: crdt.ClientID(client), Clock: 0}
	}
	frame.WriteVarUint(uint64(scanners))
	frame.WriteVarUint(usedUpScanClient)
	frame.WriteVarUint(0)
	for range scanners {
		writeOriginString(frame, crdt.ID{Client: usedUpChainClient, Clock: 0}, "k")
	}
	frame.WriteVarUint(0) // no delete set
	if _, err := NewPgVersioned(s.store).AppendUpdate(ctx, artifactID, frame.Bytes()); err != nil {
		return fmt.Errorf("persist the tangle: %w", err)
	}
	stored, err := s.persistence.Load(ctx, artifactID)
	if err != nil {
		return err
	}
	scans, err := rescan(stored.Update)
	if err != nil {
		return err
	}
	if scans < roomScanTotal {
		return fmt.Errorf("document %s's history re-scans %d items with the tangle, short of the %d scan total", artifactID, scans, roomScanTotal)
	}
	return nil
}

// writeOriginString writes one string item of content naming origin as its left origin and no
// right origin, so its parent is its origin's and the wire does not name it (crdt/update.go:942-965).
func writeOriginString(frame *encoding.Encoder, origin crdt.ID, content string) {
	frame.WriteUint8(byte(structString) | infoHasOrigin)
	frame.WriteVarUint(uint64(origin.Client))
	frame.WriteVarUint(origin.Clock)
	frame.WriteVarString(content)
}

// lastWrittenText is the last text under node that holds a character, or nil.
func lastWrittenText(node any) *crdt.YXmlText {
	switch node := node.(type) {
	case *crdt.YXmlText:
		if node.Len() > 0 {
			return node
		}
	case *crdt.YXmlFragment:
		return lastWrittenChild(node.Children())
	case *crdt.YXmlElement:
		return lastWrittenChild(node.Children())
	}
	return nil
}

func lastWrittenChild[N any](children []N) *crdt.YXmlText {
	for i := len(children) - 1; i >= 0; i-- {
		if text := lastWrittenText(children[i]); text != nil {
			return text
		}
	}
	return nil
}

// AppendRenderOnlySchemaViolationForTest appends to artifactID's stored history a tree the
// validator reads and the renderer refuses: a checked task whose empty first paragraph precedes a
// heading has no markdown the browser reads back as a task. Tests here and in package api build
// that history with it, while no room holds the document.
func AppendRenderOnlySchemaViolationForTest(ctx context.Context, database *store.Store, artifactID string) error {
	doc := crdt.New()
	fragment := doc.GetXmlFragment(fragmentName)
	tree := &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{{
		Type: "bullet_list",
		Children: []*pmdoc.Node{{
			Type:  "list_item",
			Attrs: pmdoc.Attrs{"checked": true},
			Children: []*pmdoc.Node{
				{Type: "paragraph"},
				{Type: "heading", Attrs: pmdoc.Attrs{"level": float64(2)}, Children: []*pmdoc.Node{{Type: "text", Text: "Unreadable task"}}},
			},
		}},
	}}}
	if err := doc.TransactE(func(transaction *crdt.Transaction) error {
		return pmdoc.Update(transaction, fragment, tree)
	}); err != nil {
		return fmt.Errorf("write render-only violation: %w", err)
	}
	if _, err := NewPgVersioned(database).AppendUpdate(ctx, artifactID, crdt.EncodeStateAsUpdateV1(doc, nil)); err != nil {
		return fmt.Errorf("persist render-only violation: %w", err)
	}
	return nil
}

// DocumentReadWeightForTest is the cache weight of markdown parsed as a document read. The
// memory-process test uses it to choose enough distinct documents to cross documentReadBudget
// without duplicating the cache's accounting.
func DocumentReadWeightForTest(markdown string) (int64, error) {
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		return 0, err
	}
	read, err := buildDocumentRead("", DocumentStamp{}, tree)
	if err != nil {
		return 0, err
	}
	return read.weight, nil
}

// StateStructsForTest is the structs of state, a document's encoded Yjs state, as a walk of a
// room's own state counts them (walkUpdate, fromRoom; countRoomStructs, but for parked structs'
// splits). A real-process drive counts a document's stored state with it to know how many structs
// the writes it made added, without duplicating the walk.
func StateStructsForTest(state []byte) (int64, error) {
	tally, err := walkUpdate(state, fromRoom, nil)
	return int64(tally.structs), err
}
