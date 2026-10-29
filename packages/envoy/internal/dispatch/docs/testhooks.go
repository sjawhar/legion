package docs

import (
	"context"
	"errors"
	"fmt"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs/crafted"
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

// AfterSettleLockForTest sets the hook every settlement runs once it holds its document's owner
// row - its issue's row, for an issue's document - and before it reads the room. Tests in package
// api hold a settlement there to race an issue's close against it. It is set before the service
// settles anything, since a settlement reads it unlocked.
func (s *Service) AfterSettleLockForTest(hook func(room string)) {
	s.afterSettleLock = hook
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

// OutsideSchemaCorruptionsForTest are the stored trees outside the Proof schema a document can
// come to hold: one the reader refuses, one only the renderer refuses, one whose paragraph text
// carries an embed, and one whose text carries a mark nested past what ygo's text writers take.
// Each Corrupt appends its tree to artifactID's stored history, written while no room holds the
// document, as a crafted client or an older server could have left it. Tests here and in package
// api put a document through every one of them.
var OutsideSchemaCorruptionsForTest = []struct {
	Name    string
	Corrupt func(ctx context.Context, database *store.Store, artifactID string) error
}{
	{"a tree the reader refuses", appendSchemaInvalidUpdate},
	{"a tree only the renderer refuses", AppendRenderOnlySchemaViolationForTest},
	{"a paragraph whose text holds an embed", appendTextEmbed},
	{"a mark nested past the bound", appendNestedLink},
}

// appendSchemaInvalidUpdate persists the element a crafted client can write, which the tree
// reader refuses.
func appendSchemaInvalidUpdate(ctx context.Context, database *store.Store, artifactID string) error {
	doc := crdt.New()
	fragment := doc.GetXmlFragment(fragmentName)
	doc.Transact(func(transaction *crdt.Transaction) {
		fragment.InsertElement(transaction, 0, crdt.NewYXmlElement("callout"))
	})
	if _, err := NewPgVersioned(database).AppendUpdate(ctx, artifactID, crdt.EncodeStateAsUpdateV1(doc, nil)); err != nil {
		return fmt.Errorf("append crafted document update: %w", err)
	}
	return nil
}

// appendTextEmbed persists the update a crafted document websocket client can send: an embed in
// the first paragraph's text, which the tree reader refuses and pmdoc.Update cannot diff against.
func appendTextEmbed(ctx context.Context, database *store.Store, artifactID string) error {
	versioned := NewPgVersioned(database)
	loaded, err := versioned.Load(ctx, artifactID)
	if err != nil {
		return fmt.Errorf("load document before crafted embed: %w", err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		return fmt.Errorf("decode document before crafted embed: %w", err)
	}
	before := doc.StateVector()
	var text *crdt.YXmlText
	for _, block := range doc.GetXmlFragment(fragmentName).Children() {
		if paragraph, ok := block.(*crdt.YXmlElement); ok && paragraph.NodeName == "paragraph" {
			for _, child := range paragraph.Children() {
				if found, ok := child.(*crdt.YXmlText); ok {
					text = found
				}
			}
		}
	}
	if text == nil {
		return errors.New("document holds no paragraph text to embed into")
	}
	doc.Transact(func(transaction *crdt.Transaction) {
		text.InsertEmbed(transaction, 0, map[string]any{"image": "crafted"}, nil)
	})
	if _, err := versioned.AppendUpdate(ctx, artifactID, crdt.EncodeStateAsUpdateV1(doc, before)); err != nil {
		return fmt.Errorf("append crafted embed: %w", err)
	}
	return nil
}

// appendNestedLink persists a link over the first text whose value is the string "x" inside 100
// nested arrays, one level more than a mark's attribute may: the update a crafted document
// websocket client can send, and what a server whose bound did not count the map ygo stores a mark
// as could write.
func appendNestedLink(ctx context.Context, database *store.Store, artifactID string) error {
	versioned := NewPgVersioned(database)
	loaded, err := versioned.Load(ctx, artifactID)
	if err != nil {
		return fmt.Errorf("load document before crafted link: %w", err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		return fmt.Errorf("decode document before crafted link: %w", err)
	}
	update, err := crafted.NestedLinkUpdate(doc, crafted.NestedValue(100))
	if err != nil {
		return err
	}
	if _, err := versioned.AppendUpdate(ctx, artifactID, update); err != nil {
		return fmt.Errorf("append crafted link: %w", err)
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

// RoomCapsForTest is the service's room and connection caps (reserveRoom, reserveConnection).
// cmd/dispatch's settings test reads it to see DISPATCH_MAX_LIVE_ROOMS and
// DISPATCH_MAX_DOC_CONNECTIONS reach the service through Deps.
func (s *Service) RoomCapsForTest() (rooms, connections int) {
	return s.roomCap, s.connectionCap
}
