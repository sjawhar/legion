package docs

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// unreadableDocumentStore fails every read of one document with an error that is neither of the
// two the reconciliation records as no block (ErrDocumentUnloadable, ErrDocOutsideSchema).
type unreadableDocumentStore struct {
	VersionedStore
	unreadable string
}

func (s unreadableDocumentStore) DocumentStamp(ctx context.Context, room string) (DocumentStamp, error) {
	if room == s.unreadable {
		return DocumentStamp{}, errors.New("injected: the store cannot read this document")
	}
	return s.VersionedStore.DocumentStamp(ctx, room)
}

func (s unreadableDocumentStore) LoadDocument(ctx context.Context, room string) (LoadedDocument, error) {
	if room == s.unreadable {
		return LoadedDocument{}, errors.New("injected: the store cannot read this document")
	}
	return s.VersionedStore.LoadDocument(ctx, room)
}

// One document the reconciliation cannot read for any other reason than the two it records as no
// block is skipped for that pass; the other nineteen of its batch still get their excerpts, and
// the tick reports no error, so the document that sorts first never holds the rest back.
func TestOneUnreadableDocumentDoesNotHoldBackItsBatch(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	documents := make([]string, referenceExcerptBatch)
	for index := range documents {
		documents[index] = createProjectDocument(t, database, fmt.Sprintf("# Document %d\n\nSee dispatch://CORE-%d here.\n", index, index+1))
		if _, err := database.Pool.Exec(ctx, `insert into refs (from_kind, from_id, to_kind, to_id) values ('artifact', $1, 'issue', $2)`,
			documents[index], fmt.Sprintf("CORE-%d", index+1)); err != nil {
			t.Fatalf("index document %d: %v", index, err)
		}
	}
	var first string
	if err := database.Pool.QueryRow(ctx, `select min(from_id) from refs where excerpt_version is null`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	service := New(Deps{
		Store:       database,
		Persistence: unreadableDocumentStore{VersionedStore: NewPgVersioned(database), unreadable: first},
		Events:      events.NewBroker(),
		ServerURL:   "https://dispatch.example",
		Settle:      time.Hour,
	})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })

	reconciled, _, err := service.ReconcileReferenceExcerpts(ctx)
	if err != nil {
		t.Fatalf("a tick with one unreadable document failed: %v", err)
	}
	if reconciled != referenceExcerptBatch-1 {
		t.Fatalf("reconciled %d documents; want the %d the store can read", reconciled, referenceExcerptBatch-1)
	}
	// These documents have a version row and no stored Yjs state, so the reconciliation reads no
	// blocks and records each mention with no excerpt: recorded is excerpt_version set, owed null.
	var recorded, owed int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) filter (where excerpt_version is not null), count(*) filter (where excerpt_version is null) from refs
	`).Scan(&recorded, &owed); err != nil {
		t.Fatal(err)
	}
	if recorded != referenceExcerptBatch-1 || owed != 1 {
		t.Fatalf("recorded %d mentions and left %d owed; want %d recorded and the unreadable document's one owed", recorded, owed, referenceExcerptBatch-1)
	}
	var stillOwed string
	if err := database.Pool.QueryRow(ctx, `select from_id from refs where excerpt_version is null`).Scan(&stillOwed); err != nil || stillOwed != first {
		t.Fatalf("owed mention = %q (%v); want the unreadable document %q's", stillOwed, err, first)
	}
}
