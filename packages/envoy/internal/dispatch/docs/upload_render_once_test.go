package docs

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// countRenders counts every rendering of a document tree the package makes from here to the end
// of the test (documentRenders, countDocumentRender) and returns what it has counted so far. One
// test counts at a time, as one test at a time sets a hook on Service, and the counter is taken
// away in t.Cleanup.
func countRenders(t *testing.T) func() uint64 {
	t.Helper()
	count := new(atomic.Uint64)
	if !documentRenders.CompareAndSwap(nil, count) {
		t.Fatal("another test is counting the document's renders already")
	}
	t.Cleanup(func() { documentRenders.Store(nil) })
	return count.Load
}

// uploadRendersBase is how often an upload that changes the document renders it today, the base
// this measurement records. All three renders are the write's own: ReplaceText renders the
// document as it stands, to tell whether the upload changes it; it renders the replacement it
// parsed (renderTree), which it answers its caller with; and applyLive renders the tree the write
// left (documentMarkdownValidWithOffsets), the same text over again. Nothing else renders: the
// warm room's replica holds the rendering of the state the fork was read from, so applyLive takes
// that one rather than rendering the fork, and the room's update observer classifies the
// published update by the rendering the write made of it.
//
// Two of the three are of the new document, which is the number the acceptance counts: it asks
// that an upload render the new document twice at the base and once at the head. The third is of
// the document as it stands before the upload, which the upload needs and keeps. So the change's
// commit lowers this total to two, one of the new document and one of the old.
const uploadRendersBase = 3

// An upload that changes an existing document, written through the entry point every upload takes
// (ReplaceText in a joined transaction), renders the document uploadRendersBase times.
func TestUploadRenderOnceCountsTheRendersOfAnUpload(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "The quick brown fox.\n")
	// The seed's settlement stamps the block ids and writes the first version, and the upload
	// before the measured one leaves the room's replica holding the state it wrote, as any room
	// a browser or an earlier upload has written stands. The measured upload therefore meets a
	// settled document in a warm room, as a real upload does: its read of the fork is the
	// replica's (warmFork), so the rendering of the document before it is known without rendering
	// it, and the room's update observer classifies its update by the rendering it published.
	settleCurrentGeneration(t, service, artifactID)
	if _, err := joinedReplaceText(service, artifactID, "The quick brown fox jumps.\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("warm the room with a first upload: %v", err)
	}

	renders := countRenders(t)
	uploaded, err := joinedReplaceText(service, artifactID, "The quick brown dog.\n", model.Actor{Kind: "user", ID: "alice"})
	counted := renders()
	if err != nil {
		t.Fatalf("upload the replacement: %v", err)
	}
	t.Logf("an upload that changes the document costs %d renders of it", counted)
	if uploaded != "The quick brown dog.\n" {
		t.Fatalf("the upload wrote %q, want the uploaded markdown", uploaded)
	}
	if counted != uploadRendersBase {
		t.Fatalf("the upload rendered the document %d times, want %d", counted, uploadRendersBase)
	}
}
