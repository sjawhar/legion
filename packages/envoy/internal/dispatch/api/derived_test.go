package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A write phase is one batch (store.WriteBatch), so a value a statement used to read back from
// the one before it is derived instead: in SQL, or in the read phase before the batch is built.
// Each test here checks one such value against the row the write left.

// derivedHandler is an interaction server whose document service settles after an hour, so no
// settlement writes beside the request under test.
func derivedHandler(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		service := docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
		return service
	})
	return withDeadline(handler), database
}

// A message is inserted in the batch that appends its event, so the message the route answers and
// its event carry is built before the insert runs (insertedMessage): its created_at, the
// transaction's now(); its in_reply_to, in the uuid type's text form whatever form the request
// spelled it in; its author as the jsonb column decodes. Each equals the stored row, as it did when
// the route read the row back from the insert.
func TestDerivedMessageIsTheStoredRow(t *testing.T) {
	handler, database := derivedHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Derived message", "A spec")
	path := "/api/v1/issues/" + issue.Key + "/messages"
	first := dispatchRequest(t, handler, http.MethodPost, path, map[string]any{"body": "Status: ready."}, "alice")
	if first.Code != http.StatusCreated {
		t.Fatalf("post the first message: status=%d body=%s", first.Code, first.Body.String())
	}
	parent := decodeBody[model.Message](t, first)
	reply := dispatchRequest(t, handler, http.MethodPost, path, map[string]any{
		"body": "Thanks; see dispatch://" + issue.Key + ".", "in_reply_to": "{" + strings.ToUpper(parent.ID) + "}",
	}, "bob")
	if reply.Code != http.StatusCreated {
		t.Fatalf("post the reply: status=%d body=%s", reply.Code, reply.Body.String())
	}
	answered := decodeBody[model.Message](t, reply)
	var stored model.Message
	var author []byte
	var payloadMatches bool
	if err := database.Pool.QueryRow(context.Background(), `
		select m.created_at, m.in_reply_to::text, m.author,
		       (e.payload->>'created_at')::timestamptz = m.created_at
		         and e.payload->>'in_reply_to' = m.in_reply_to::text
		         and e.payload->'author' = m.author
		         and e.payload->>'id' = m.id::text
		from messages m join events e on e.type = 'message.answered' and e.payload->>'id' = m.id::text
		where m.id = $1
	`, answered.ID).Scan(&stored.CreatedAt, &stored.InReplyTo, &author, &payloadMatches); err != nil {
		t.Fatalf("read the stored reply and its event: %v", err)
	}
	if !answered.CreatedAt.Equal(stored.CreatedAt) {
		t.Errorf("answered created_at %s, stored %s", answered.CreatedAt, stored.CreatedAt)
	}
	if answered.InReplyTo == nil || stored.InReplyTo == nil || *answered.InReplyTo != *stored.InReplyTo || *stored.InReplyTo != parent.ID {
		t.Errorf("answered in_reply_to %v, stored %v, want the parent's id %s in its text form", answered.InReplyTo, stored.InReplyTo, parent.ID)
	}
	if answered.Author.Kind != "user" || answered.Author.ID != "bob" || !strings.Contains(string(author), `"bob"`) {
		t.Errorf("answered author %+v, stored %s", answered.Author, author)
	}
	if !payloadMatches {
		t.Errorf("the message.answered event's message differs from the stored row")
	}
}

// An approval request moved by a version names the move's edited_at, read with clock_timestamp()
// in the read phase rather than returned by the update, on the row and on the ask.edited event's
// copy of the row alike.
func TestDerivedApprovalMoveEditedAtIsTheRowsEditedAt(t *testing.T) {
	handler, database := derivedHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Derived approval move", "A spec")
	requested := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{
		"actor": sessionActor(), "summary": "Caps retries at three attempts.",
	})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
	}
	askID := decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, requested).Ask.ID
	edited := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":     []map[string]string{{"op": "replace", "find": "A spec", "with": "A revised spec"}},
		"summary": "revise", "actor": sessionActor(),
	})
	if edited.Code != http.StatusOK {
		t.Fatalf("edit the document: status=%d body=%s", edited.Code, edited.Body.String())
	}
	var rowEditedAt time.Time
	var payloadEditedAt string
	var questionMatches bool
	if err := database.Pool.QueryRow(context.Background(), `
		select a.edited_at, e.payload->>'edited_at', e.payload->>'question' = a.question
		from asks a join events e on e.type = 'ask.edited' and e.payload->>'id' = a.id::text
		where a.id = $1
		order by e.id desc limit 1
	`, askID).Scan(&rowEditedAt, &payloadEditedAt, &questionMatches); err != nil {
		t.Fatalf("read the moved request and its event: %v", err)
	}
	carried, err := time.Parse(time.RFC3339Nano, payloadEditedAt)
	if err != nil {
		t.Fatalf("the ask.edited event's edited_at %q does not parse: %v", payloadEditedAt, err)
	}
	if !carried.Equal(rowEditedAt) {
		t.Errorf("ask.edited carries edited_at %s, the row holds %s", carried, rowEditedAt)
	}
	if !questionMatches {
		t.Errorf("the ask.edited event's question differs from the moved row's")
	}
}

// A review the answer to an approval request records takes its event's sequence in the write
// phase's batch (docs.QueueReview): the update names the event the statements before it appended
// (store.LatestEventID), so the row's event_seq is that artifact.approved event's sequence.
func TestDerivedReviewEventSeqIsItsEventsSequence(t *testing.T) {
	handler, database := derivedHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Derived review", "A spec")
	requested := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/approval-requests", map[string]any{
		"actor": sessionActor(), "summary": "Ready.",
	})
	if requested.Code != http.StatusCreated {
		t.Fatalf("request approval: status=%d body=%s", requested.Code, requested.Body.String())
	}
	askID := decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, requested).Ask.ID
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected": []string{docs.ApprovalOptionApprove}, "expected_edited_at": nil,
	}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer the approval request: status=%d body=%s", answered.Code, answered.Body.String())
	}
	var eventSeq, seq int64
	var answeredSeq int64
	if err := database.Pool.QueryRow(context.Background(), `
		select r.event_seq, e.seq,
		       (select seq from events where type = 'ask.answered' and payload->>'id' = $2)
		from artifact_reviews r
		join events e on e.type = 'artifact.approved' and e.payload->>'artifact_id' = r.artifact_id::text
		where r.artifact_id = $1
	`, issue.PrimaryArtifactID, askID).Scan(&eventSeq, &seq, &answeredSeq); err != nil {
		t.Fatalf("read the review and its event: %v", err)
	}
	if eventSeq != seq {
		t.Errorf("review event_seq = %d, want its artifact.approved event's sequence %d", eventSeq, seq)
	}
	if seq != answeredSeq+1 {
		t.Errorf("artifact.approved has seq %d, want the one after the answer's ask.answered (%d)", seq, answeredSeq)
	}
}
