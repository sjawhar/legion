package docs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestApprovalAskSummaryRoundTripsTheSharedQuestion(t *testing.T) {
	ask := model.Ask{
		ID:       "approval-1",
		Question: ApprovalQuestion("spec.md", 2, "Names the rollback plan."),
		Approval: &model.AskApproval{Name: "spec.md", Version: 2},
	}
	if summary, err := ApprovalAskSummary(ask); err != nil || summary != "Names the rollback plan." {
		t.Fatalf("approval summary = %q, %v", summary, err)
	}
}

// A rewording in a transaction no ledger joined is refused and changes nothing. With no ledger
// the update would run at once while the ask.edited event the rewording appends reached no ledger
// to publish it, so the request would read differently and nobody watching it would be told; the
// refusal keeps the row and its event together. A settlement's ledger and the block-id backfill's
// are in the context of the writes they own, so the only context refused here is one with no
// ledger at all.
func TestRewritingAnApprovalAskWithNoLedgerIsRefusedAndRewordsNothing(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	ctx := context.Background()
	approval, err := EncodeApproval(&model.AskApproval{ArtifactID: artifactID, Name: "document.md", Version: 1, RequestedVersion: 1})
	if err != nil {
		t.Fatalf("encode the approval: %v", err)
	}
	question := ApprovalQuestion("document.md", 1, "The first cut.")
	// The request belongs to the document's issue, as every ask on an issue's document does; the
	// document it asks about is the artifact its approval names.
	if _, err := database.Pool.Exec(ctx, `
		insert into asks (issue_key, author, question, kind, approval)
		values ('DOC-1', '{"kind":"session","id":"planner"}', $1, 'approval', $2)
	`, question, approval); err != nil {
		t.Fatalf("open the approval request: %v", err)
	}

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction no ledger joined: %v", err)
	}
	defer tx.Rollback(ctx)
	ask, err := OpenApprovalAsk(ctx, tx, artifactID)
	if err != nil || ask == nil {
		t.Fatalf("read the open approval request = %v, %v", ask, err)
	}
	mover := model.Actor{Kind: "user", ID: "alice"}
	if err := RewriteApprovalAsk(ctx, tx, events.NewBroker(), ask, mover, 2, "The second cut.", "https://dispatch.example"); !errors.Is(err, errUnjoined) {
		t.Fatalf("rewording an approval request with no ledger = %v, want errUnjoined", err)
	}
	var stored string
	var version int
	var editedAt *time.Time
	if err := tx.QueryRow(ctx, `
		select question, (approval->>'version')::int, edited_at from asks where id = $1::uuid
	`, ask.ID).Scan(&stored, &version, &editedAt); err != nil {
		t.Fatalf("read the approval request back: %v", err)
	}
	if stored != question || version != 1 || editedAt != nil {
		t.Fatalf("the approval request after the refused rewording = %q at version %d, edited at %v; want %q at version 1, never edited",
			stored, version, editedAt, question)
	}
}
