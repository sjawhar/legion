package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// previousReleaseReview writes a review in tx exactly as the release before migration 0117 does
// (docs.WriteReview before it kept the sequence): the review row, naming no event_seq, then its
// artifact.* event appended through the real broker. It returns the row's id and the event.
func previousReleaseReview(t *testing.T, ctx context.Context, tx pgx.Tx, broker *events.Broker, artifact model.Artifact, version int, state string, reason *string) (string, model.Event) {
	t.Helper()
	actor := model.Actor{Kind: "user", ID: "alice"}
	actorJSON, err := json.Marshal(actor)
	if err != nil {
		t.Fatalf("encode the review actor: %v", err)
	}
	var reviewID string
	if err := tx.QueryRow(ctx, `
		insert into artifact_reviews (artifact_id, version, state, actor, reason, ask_id)
		values ($1, $2, $3, $4, $5, $6)
		returning id::text`, artifact.ID, version, state, actorJSON, reason, nil).Scan(&reviewID); err != nil {
		t.Fatalf("insert the previous release's review: %v", err)
	}
	return reviewID, appendPreviousReleaseReviewEvent(t, ctx, tx, broker, artifact, version, state, reason)
}

// appendPreviousReleaseReviewEvent appends the artifact.* event the previous release appends after
// it inserts a review.
func appendPreviousReleaseReviewEvent(t *testing.T, ctx context.Context, tx pgx.Tx, broker *events.Broker, artifact model.Artifact, version int, state string, reason *string) model.Event {
	t.Helper()
	actor := model.Actor{Kind: "user", ID: "alice"}
	eventType := "artifact.approved"
	if state == "changes_requested" {
		eventType = "artifact.changes_requested"
	}
	event := model.Event{IssueKey: artifact.IssueKey, Type: eventType, Actor: actor, Payload: model.ArtifactReviewEventPayload{
		ArtifactID: artifact.ID, Name: artifact.Name, Version: version, Actor: actor, Reason: reason,
	}}
	if artifact.IssueKey == nil {
		event.ArtifactID = &artifact.ID
	}
	appended, err := broker.Append(ctx, tx, event)
	if err != nil {
		t.Fatalf("append the previous release's %s: %v", eventType, err)
	}
	return appended
}

// reviewEventSeq is the review's event_seq, 0 when it carries none (the column's check refuses 0).
func reviewEventSeq(t *testing.T, database *store.Store, reviewID string) int64 {
	t.Helper()
	var seq int64
	if err := database.Pool.QueryRow(context.Background(), `select coalesce(event_seq, 0) from artifact_reviews where id = $1`, reviewID).Scan(&seq); err != nil {
		t.Fatalf("read review %s's sequence: %v", reviewID, err)
	}
	return seq
}

// A review the release before migration 0117 writes while a rollout (or a rollback) runs both
// releases names no event_seq. It is newer than the review this release wrote before it, and the
// approval read must say so: the migration's trigger gives it its event's sequence at its commit.
// Without the trigger it would rank below the sequenced review (`event_seq desc nulls last`), so a
// person's changes request written by an old pod would be overridden by the approval before it, and
// a person's approval written by an old pod would read back as the changes requested before it.
func TestAReviewThePreviousReleaseWritesDuringARolloutReadsAsTheNewest(t *testing.T) {
	for _, order := range []struct {
		name          string
		first, second string
	}{
		{name: "changes requested by an old pod after an approval", first: "approved", second: "changes_requested"},
		{name: "approval by an old pod after a changes request", first: "changes_requested", second: "approved"},
	} {
		t.Run(order.name, func(t *testing.T) {
			handler, database, deps := newTestServer(t, testServerOptions{})
			issue := createInteractionIssue(t, handler, "TEST", "Rolling review", "A spec")
			artifact := decodeBody[model.Artifact](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID, nil, "alice"))
			ctx := context.Background()

			body := map[string]any{"state": order.first}
			if order.first == "changes_requested" {
				body["reason"] = "Name the rollback path."
			}
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/reviews", body, "alice")
			if response.Code != http.StatusCreated {
				t.Fatalf("review as this release: status=%d body=%s", response.Code, response.Body.String())
			}
			first := decodeBody[model.ArtifactReview](t, response)
			if reviewEventSeq(t, database, first.ID) == 0 {
				t.Fatalf("this release's review carries no sequence")
			}

			var reason *string
			if order.second == "changes_requested" {
				reason = new("And name who runs the restore.")
			}
			tx, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the previous release's review: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			reviewID, event := previousReleaseReview(t, ctx, tx, deps.Events, artifact, first.Version, order.second, reason)
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit the previous release's review: %v", err)
			}

			type sequencedRead struct {
				Approval struct {
					State string `json:"state"`
					Seq   *int64 `json:"seq"`
				} `json:"approval"`
			}
			got := decodeBody[sequencedRead](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID, nil, "alice"))
			if got.Approval.State != order.second || got.Approval.Seq == nil || *got.Approval.Seq != int64(event.Seq) {
				readSeq := "none"
				if got.Approval.Seq != nil {
					readSeq = strconv.FormatInt(*got.Approval.Seq, 10)
				}
				t.Errorf("approval reads %s at sequence %s, want %s, the old pod's newer review (sequence %d)",
					got.Approval.State, readSeq, order.second, event.Seq)
			}
			if seq := reviewEventSeq(t, database, reviewID); seq != int64(event.Seq) {
				t.Errorf("the previous release's review sequence = %d (0 is none), want its event's %d", seq, event.Seq)
			}
		})
	}
}

// The trigger gives a review the event its own transaction appended, and no other: two old-pod
// reviews of one document, version and state, the first begun before the second and committed
// after it, each get their own event's sequence, and a review whose transaction appended no event
// stays without one whatever other transactions append.
func TestTheReviewSequenceTriggerTouchesOnlyItsOwnTransactionsReview(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "TEST", "Concurrent reviews", "A spec")
	artifact := decodeBody[model.Artifact](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID, nil, "alice"))
	ctx := context.Background()
	reason := new("Name the rollback path.")
	actorJSON := `{"kind":"user","id":"alice"}`

	// A review whose transaction appends no event, committed first.
	var bare string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifact_reviews (artifact_id, version, state, actor, reason)
		values ($1, 1, 'changes_requested', $2, $3) returning id::text`, artifact.ID, actorJSON, reason).Scan(&bare); err != nil {
		t.Fatalf("insert a review with no event: %v", err)
	}

	// The earlier transaction inserts its review and holds off its event.
	earlier, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the earlier review: %v", err)
	}
	defer func() { _ = earlier.Rollback(ctx) }()
	var earlierID string
	if err := earlier.QueryRow(ctx, `
		insert into artifact_reviews (artifact_id, version, state, actor, reason)
		values ($1, 1, 'changes_requested', $2, $3) returning id::text`, artifact.ID, actorJSON, reason).Scan(&earlierID); err != nil {
		t.Fatalf("insert the earlier review: %v", err)
	}

	// The later transaction writes its review and event and commits while the earlier one waits.
	later, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the later review: %v", err)
	}
	defer func() { _ = later.Rollback(ctx) }()
	laterID, laterEvent := previousReleaseReview(t, ctx, later, deps.Events, artifact, 1, "changes_requested", reason)
	if err := later.Commit(ctx); err != nil {
		t.Fatalf("commit the later review: %v", err)
	}

	earlierEvent := appendPreviousReleaseReviewEvent(t, ctx, earlier, deps.Events, artifact, 1, "changes_requested", reason)
	if err := earlier.Commit(ctx); err != nil {
		t.Fatalf("commit the earlier review: %v", err)
	}

	if seq := reviewEventSeq(t, database, laterID); seq != int64(laterEvent.Seq) {
		t.Errorf("the later review's sequence = %d (0 is none), want its own event's %d", seq, laterEvent.Seq)
	}
	if seq := reviewEventSeq(t, database, earlierID); seq != int64(earlierEvent.Seq) {
		t.Errorf("the earlier review's sequence = %d (0 is none), want its own event's %d", seq, earlierEvent.Seq)
	}
	if seq := reviewEventSeq(t, database, bare); seq != 0 {
		t.Errorf("a review whose transaction appended no event took sequence %d from another transaction", seq)
	}
}
