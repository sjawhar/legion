package docs

import (
	"context"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A comment's or suggestion's margin record is written without reading or rendering the document
// twice (applyMargin), where it was once written as every other document write is (applyLive): the
// record set on the marks map, the document read and rendered before and after to tell what the
// write changed. The margin write keeps everything that write left - the stored update and the
// class it is stored with, the anchors it refreshes and the events that refresh appends, the
// rendering the transaction keeps for its version, the credits, the room's record of the publish
// and the rendering the room keeps after it - only cheaper. This drives both writes through the
// document calls each comment route makes, on real-size specs whose open anchors a browser has
// just moved, and holds the margin write to what the whole-document write leaves, field by field.
// A margin write that recorded its update as a content change would be settled as an edit and
// credited to its comment's author; one that skipped the anchor refresh would leave a moved
// comment quoting text the document no longer holds; one that kept a rendering from before a
// browser edit landed in its transaction would version, and refresh anchors against, a document
// that never existed.
//
// The two writes run on two services with a database each, every flow on a document of its own,
// seeded on both sides from the same bytes under the same ids, so the stored histories match byte
// for byte before each flow and every difference after it is the write's. When
// DISPATCH_TEST_SPEC_CORPUS names a directory, every *.md in it is one more document. Those may be
// private specs: the test logs only their size, sha256 and anchor count, and a difference on one
// names only lengths and sha256s.
func TestAMarginWriteMatchesTheWholeDocumentWrite(t *testing.T) {
	sources := []equivalenceSource{
		{name: "generated-390KiB", markdown: generatedSpec(390 << 10)},
		{name: "generated-600KiB", markdown: generatedSpec(600 << 10)},
	}
	sources = append(sources, corpusSpecs(t)...)
	sides := [2]*equivalenceSide{
		newEquivalenceSide(t, "margin write", true, projectMarginRecord),
		newEquivalenceSide(t, "whole-document write", false, wholeDocumentWrite),
	}
	flows := equivalenceFlows()
	number := 0
	for _, source := range sources {
		t.Run(source.name, func(t *testing.T) {
			document := prepareEquivalenceDocument(t, source)
			for _, flow := range flows {
				number++
				ids := flowIDsFor(number)
				t.Run(flow.name, func(t *testing.T) {
					compareFlow(t, sides, document, flow, ids)
				})
			}
		})
	}
}

// marginWriter writes record as markID's margin record in the transaction ctx joined: the margin
// write (ProjectMark) on one side, the whole-document write on the other. Every flow takes it, so
// one flow's code drives both.
type marginWriter func(ctx context.Context, s *Service, artifactID, markID string, record MarkRecord, actor model.Actor) error

func projectMarginRecord(ctx context.Context, s *Service, artifactID, markID string, record MarkRecord, actor model.Actor) error {
	return s.ProjectMark(ctx, artifactID, markID, record, actor)
}

// wholeDocumentWrite is ProjectMark as it was before the margin write: the record set on the marks
// map through applyLive, which reads and renders the document on either side of the write, as
// every other document write still does.
func wholeDocumentWrite(ctx context.Context, s *Service, artifactID, markID string, record MarkRecord, actor model.Actor) error {
	plain, err := toPlain(record)
	if err != nil {
		return err
	}
	return s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, _ *pmdoc.Node, transact func(func(*crdt.Transaction))) error {
		marks := doc.GetMap(marksMapName)
		transact(func(txn *crdt.Transaction) {
			marks.Set(txn, markID, plain)
		})
		return nil
	})
}
