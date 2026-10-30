package docs

import (
	"context"
	"fmt"
	"time"

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

// HoldNextAdmissionForTest makes the next socket to artifactID wait for wait before its admission
// (ServeHTTP), so a browser test can keep a document's first sync waiting as long as a browser in a
// burst waits for its room - for its stored version's count and the grant that count takes of the
// copy bound (storedCountWeight), all before the server answers the socket's upgrade - and see
// what the editor does meanwhile. The wait ends early, and the socket is answered nothing, if its
// request ends. A wait of zero or less takes back a hold no socket has used yet. It is mounted
// only through Dispatch's test-hook route.
func (s *Service) HoldNextAdmissionForTest(artifactID string, wait time.Duration) {
	if wait <= 0 {
		s.admissionHolds.Delete(artifactID)
		return
	}
	s.admissionHolds.Store(artifactID, wait)
}

// chainOrigin is the id of the last character of artifactID's last written text, the item the two
// tangle hooks below chain onto. It closes artifactID's room if this server holds it, so the next
// load reads the history the hook appends to, and decodes the stored history into a copy of its
// own, as a load decodes it.
func (s *Service) chainOrigin(ctx context.Context, artifactID string) (crdt.ID, error) {
	if err := s.Evict(ctx, artifactID); err != nil {
		return crdt.ID{}, err
	}
	loaded, err := s.persistence.Load(ctx, artifactID)
	if err != nil {
		return crdt.ID{}, err
	}
	doc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		return crdt.ID{}, err
	}
	text := lastWrittenText(doc.GetXmlFragment(fragmentName))
	if text == nil {
		return crdt.ID{}, fmt.Errorf("document %s holds no text to chain onto", artifactID)
	}
	last := crdt.CreateRelativePositionFromIndex(text, text.Len()-1, 0).Item
	if last == nil {
		return crdt.ID{}, fmt.Errorf("document %s's last text has no item for its last character", artifactID)
	}
	return *last, nil
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
	origin, err := s.chainOrigin(ctx, artifactID)
	if err != nil {
		return err
	}
	// Each scanner passes the chain after its first item and stops at the scanner before it, of
	// its own client and naming the same origins: about usedUpChain scans each.
	scanners := roomScanTotal/usedUpChain + 2
	frame := encoding.NewEncoder()
	frame.WriteVarUint(usedUpChain + 1) // client groups: the chain's, then the scanners'
	writeChain(frame, origin, usedUpChain, usedUpChainClient)
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

// The tangle TangleBeyondOneApplyForTest appends: a chain of tangledChain one-character items,
// each of a client of its own from tangledChainClient on, and the client of the probe item the
// hook measures the chain with, which stands for the browser whose insert meets it. A browser's
// client ID is a random 32-bit number, so these collide with one no more often than any two
// browsers' do, and all but about one in 30,000 of them are past the chain's, which is what makes
// a browser's insert scan the chain rather than stop at its first item (Item.integrate, case 1,
// crdt/item.go:155-168).
const (
	tangledChain       = 40_000
	tangledChainClient = 100_000
	tangledProbeClient = 4_000_000_000
)

// TangleBeyondOneApplyForTest closes artifactID's room if this server holds it and appends to its
// stored history one update whose structs no later apply of a browser's can merge: after the last
// character of the document's last text, a chain of tangledChain one-character items, each of a
// client of its own so none merges with its neighbour. An insert a browser made at that place
// before the chain arrived - a paste at the end of that text, offline - names the same left origin
// and no right origin, so its apply scans the chain to its end (Item.integrate, sjawhar/ygo
// v1.51.3-sami.7, crdt/item.go:117-192): 40,000 items, past the 27,594 an update no rule weighs is
// allowed (scanCeiling(0)), so the room refuses it whole and the browser is closed 4413
// DOC_TOO_TANGLED (documentTooTangled). Decoding the chain costs the room about one scan an item,
// 40,006 of its 1,000,000-item scan total measured on this pod (the browser row of
// packages/dispatch/e2e/document-refusals.e2e.ts, whose server logged a refusal at an allowance of
// 27,528 leaving 932,466 of the total), so the room the next load makes has far more left than one
// apply may scan and refuses that apply as too tangled rather than as needing a rebuild
// (roomScans.reserve). A document tangles that far only where tens of thousands of items have been
// made at one place since a browser's edit, which no browser test can type. The document reads the
// chain's "a"s at the end of its last text, and no stored rendering until the next room settles.
// It refuses to return without error unless the stored history leaves more of the room's scan
// total than one apply may scan, so that the ceiling is what refuses the apply, and unless an
// insert at the chain's head scans past that ceiling. It is mounted only through Dispatch's
// test-hook route.
func (s *Service) TangleBeyondOneApplyForTest(ctx context.Context, artifactID string) error {
	origin, err := s.chainOrigin(ctx, artifactID)
	if err != nil {
		return err
	}
	frame := encoding.NewEncoder()
	frame.WriteVarUint(tangledChain) // client groups: one for each item of the chain
	writeChain(frame, origin, tangledChain, tangledChainClient)
	frame.WriteVarUint(0) // no delete set
	if _, err := NewPgVersioned(s.store).AppendUpdate(ctx, artifactID, frame.Bytes()); err != nil {
		return fmt.Errorf("persist the tangle: %w", err)
	}
	stored, err := s.persistence.Load(ctx, artifactID)
	if err != nil {
		return err
	}
	state, err := decodeStoredState(stored.Update)
	if err != nil {
		return err
	}
	ceiling := uint64(scanCeiling(0))
	if left := roomScanTotal - int64(state.scans); left <= int64(ceiling) {
		return fmt.Errorf("document %s's history re-scans %d items with the tangle, leaving %d of the %d scan total, within the %d items one apply may scan",
			artifactID, state.scans, left, roomScanTotal, ceiling)
	}
	// What a browser's insert at the chain's head scans, measured as the room's apply measures it
	// (decodeCountingScans): the probe is that insert's shape, of a client past the chain's.
	scans, err := decodeCountingScans(state.doc, tangleProbe(origin))
	if err != nil {
		return err
	}
	if scans <= ceiling {
		return fmt.Errorf("document %s's tangle makes an insert at its head scan %d items, within the %d one unweighed apply may",
			artifactID, scans, ceiling)
	}
	return nil
}

// tangleProbe is the update one insert a browser made at origin before the tangle arrived looks
// like on the wire: a string item of a client past the chain's naming origin as its left origin
// and no right origin (writeOriginString), the shape of a paste at the end of a text that has been
// chained onto since.
func tangleProbe(origin crdt.ID) []byte {
	frame := encoding.NewEncoder()
	frame.WriteVarUint(1) // one client group
	frame.WriteVarUint(1) // structs in the group
	frame.WriteVarUint(tangledProbeClient)
	frame.WriteVarUint(0) // first clock
	writeOriginString(frame, origin, "z")
	frame.WriteVarUint(0) // no delete set
	return frame.Bytes()
}

// writeOriginString writes one string item of content naming origin as its left origin and no
// right origin, so its parent is its origin's and the wire does not name it (crdt/update.go:942-965).
func writeOriginString(frame *encoding.Encoder, origin crdt.ID, content string) {
	frame.WriteUint8(byte(structString) | infoHasOrigin)
	frame.WriteVarUint(uint64(origin.Client))
	frame.WriteVarUint(origin.Clock)
	frame.WriteVarString(content)
}

// writeChain writes a chain of chain one-character items onto frame, the first naming origin as
// its left origin and each later one the item before it, each of a client of its own from
// firstClient on so none merges with its neighbour. Each item is a client group of its own, which
// the caller counts in the header it wrote before this.
func writeChain(frame *encoding.Encoder, origin crdt.ID, chain, firstClient int) {
	for i := range chain {
		client := uint64(firstClient + i)
		frame.WriteVarUint(1) // structs in the group
		frame.WriteVarUint(client)
		frame.WriteVarUint(0) // first clock
		writeOriginString(frame, origin, "a")
		origin = crdt.ID{Client: crdt.ClientID(client), Clock: 0}
	}
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
