package refs

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// A write phase stamps a source's new edges in the batch that appends the source's event
// (QueueStamp): the stamp names the event's id as the id sequence's currval, which the event's
// insert just set, rather than an id read back in Go. Every new edge of the document - its refs
// row and the mention its block holds - carries the id of the event appended right before the
// stamp, not of an event the same batch appends after it, and an edge stamped before keeps its own.
func TestDerivedStampSourceSeqIsTheStampingEventsID(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	const server = "https://dispatch.example"
	markdown := "See dispatch://STAMP-1 and dispatch://STAMP-2."
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "STAMP")
	createReferenceIssue(t, tx, "STAMP-1", "STAMP")
	document, _ := createReferenceArtifact(t, tx, "STAMP-1", "STAMP", "spec")
	if _, err := replaceRefs(ctx, tx, "artifact", document, text.Extract(markdown, server), nil); err != nil {
		t.Fatalf("index the document's references: %v", err)
	}
	located := text.ExtractAt(markdown, server)
	blocks := []model.ArtifactBlock{{ID: "b-stamp", Type: "paragraph", From: 0, To: len(markdown)}}
	if _, err := IndexBlocks(ctx, tx, document, 1, time.Now(), markdown, blocks, located); err != nil {
		t.Fatalf("index the document's blocks: %v", err)
	}
	if _, err := tx.Exec(ctx, `insert into refs (from_kind, from_id, to_kind, to_id, source_seq) values ('artifact', $1, 'issue', 'STAMP-9', 7)`, document); err != nil {
		t.Fatalf("store an edge stamped before: %v", err)
	}
	broker := events.NewBroker()
	alice := model.Actor{Kind: "user", ID: "alice"}
	var stamping, after model.Event
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		if err := broker.QueueAppend(b, model.Event{IssueKey: new("STAMP-1"), Type: "artifact.version", Actor: alice, Payload: map[string]any{}}, func(written model.Event) { stamping = written }); err != nil {
			return err
		}
		QueueStamp(b, "artifact", document)
		return broker.QueueAppend(b, model.Event{IssueKey: new("STAMP-1"), Type: "test.after", Actor: alice, Payload: map[string]any{}}, func(written model.Event) { after = written })
	}); err != nil {
		t.Fatalf("append and stamp: %v", err)
	}
	if after.ID <= stamping.ID {
		t.Fatalf("the event after has id %d, not after the stamping event's %d", after.ID, stamping.ID)
	}
	var refsStamped, refsTotal, blockStamped, blockTotal, keptSeq int64
	if err := tx.QueryRow(ctx, `
		select (select count(*) from refs where from_kind = 'artifact' and from_id = $1 and to_id <> 'STAMP-9' and source_seq = $2),
		       (select count(*) from refs where from_kind = 'artifact' and from_id = $1 and to_id <> 'STAMP-9'),
		       (select count(*) from block_refs where artifact_id = $1::uuid and source_seq = $2),
		       (select count(*) from block_refs where artifact_id = $1::uuid),
		       (select source_seq from refs where from_kind = 'artifact' and from_id = $1 and to_id = 'STAMP-9')
	`, document, stamping.ID).Scan(&refsStamped, &refsTotal, &blockStamped, &blockTotal, &keptSeq); err != nil {
		t.Fatalf("read the stamps: %v", err)
	}
	if refsTotal != 2 || refsStamped != refsTotal {
		t.Errorf("%d of the document's %d new refs carry source_seq %d, want both", refsStamped, refsTotal, stamping.ID)
	}
	if blockTotal != 2 || blockStamped != blockTotal {
		t.Errorf("%d of the document's %d block mentions carry source_seq %d, want both", blockStamped, blockTotal, stamping.ID)
	}
	if keptSeq != 7 {
		t.Errorf("the edge stamped before carries source_seq %d, want its own 7", keptSeq)
	}
}
