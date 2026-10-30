package docs

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// recordedMarkRefs reads the marks the rows anchored on one document record: its anchored
// comments, suggestions and asks. A row anchored on another document, a comment with no anchor
// and an ask block (block_artifact_id set, no anchor) record none of its marks.
func TestRecordedMarkRefsListsOnlyTheDocumentsAnchoredRows(t *testing.T) {
	database := storetest.Open(t)
	documentA := createDocument(t, database, "")
	documentB := createDocument(t, database, "")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, documentA, "The quick brown fox")
	seedServiceText(t, service, documentB, "The quick brown fox")

	insertAnchoredCommentWithID(t, service, documentA, "00000000-0000-4000-8000-0000000000a1", "quick")
	insertAnchoredAsk(t, service, documentA, "brown")
	insertAnchoredCommentWithID(t, service, documentB, "00000000-0000-4000-8000-0000000000b1", "quick")
	insertRecordedMarkRow(t, service, `
		insert into comments (id, issue_key, author, body, anchor, suggestion)
		values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Suggestion', $2, '{"kind":"replace","content":"slow"}')
	`, "00000000-0000-4000-8000-0000000000a2", markedAnchor(t, service, documentA, MarkSuggestion, "00000000-0000-4000-8000-0000000000a2", "fox"))
	insertRecordedMarkRow(t, service, `
		insert into comments (id, issue_key, author, body)
		values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Unanchored comment')
	`, "00000000-0000-4000-8000-0000000000c1")
	insertRecordedMarkRow(t, service, `
		insert into asks (id, issue_key, author, question, anchor)
		values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Ask on B', $2)
	`, "00000000-0000-4000-8000-0000000000b2", markedAnchor(t, service, documentB, MarkAsk, "00000000-0000-4000-8000-0000000000b2", "brown"))
	insertRecordedMarkRow(t, service, `
		insert into asks (id, issue_key, author, question, block_id, block_artifact_id)
		values ($1, 'DOC-1', '{"kind":"user","id":"alice"}', 'Ask block on A', 'ttl', $2)
	`, "00000000-0000-4000-8000-0000000000a3", documentA)

	recorded, err := service.recordedMarkRefs(context.Background(), documentA)
	if err != nil {
		t.Fatalf("recordedMarkRefs: %v", err)
	}
	want := map[pmdoc.MarkRef]struct{}{
		{Type: "dispatchAsk", ID: "00000000-0000-4000-8000-000000000001"}:     {},
		{Type: "proofComment", ID: "00000000-0000-4000-8000-0000000000a1"}:    {},
		{Type: "proofSuggestion", ID: "00000000-0000-4000-8000-0000000000a2"}: {},
	}
	if !maps.Equal(recorded, want) {
		t.Fatalf("recorded marks = %v, want %v", recorded, want)
	}
}

func insertRecordedMarkRow(t *testing.T, service *Service, statement string, args ...any) {
	t.Helper()
	if _, err := service.store.Pool.Exec(context.Background(), statement, args...); err != nil {
		t.Fatalf("insert row: %v", err)
	}
}
