package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An approval ask's fixed options: the labels a human's answer is matched against, and the ones
// the approval request route offers.
const (
	ApprovalOptionApprove        = "Approve"
	ApprovalOptionRequestChanges = "Request changes"
)

// ReviewColumns selects one artifact_reviews row in the order ScanReview reads it.
const ReviewColumns = `id::text, artifact_id::text, version, state, actor, reason, ask_id::text, created_at`

// ScanReview decodes one ReviewColumns row.
func ScanReview(row pgx.Row) (model.ArtifactReview, error) {
	var review model.ArtifactReview
	var actor []byte
	if err := row.Scan(&review.ID, &review.ArtifactID, &review.Version, &review.State, &actor, &review.Reason, &review.AskID, &review.CreatedAt); err != nil {
		return model.ArtifactReview{}, err
	}
	if err := json.Unmarshal(actor, &review.Actor); err != nil {
		return model.ArtifactReview{}, fmt.Errorf("decode review actor: %w", err)
	}
	return review, nil
}

// WriteReview records a review of artifact pinned to version and appends its event,
// artifact.approved or artifact.changes_requested, on the document's owner: its issue when it
// belongs to one, else the document itself. askID is the approval ask the review answers, nil
// when it was given from the document header with no request open. The caller owns the
// transaction and publishes the event.
func WriteReview(ctx context.Context, tx pgx.Tx, broker *events.Broker, artifact model.Artifact, version int, state string, actor model.Actor, reason *string, askID *string) (model.ArtifactReview, model.Event, error) {
	actorJSON, err := json.Marshal(actor)
	if err != nil {
		return model.ArtifactReview{}, model.Event{}, fmt.Errorf("encode review actor: %w", err)
	}
	review, err := ScanReview(tx.QueryRow(ctx, `
		insert into artifact_reviews (artifact_id, version, state, actor, reason, ask_id)
		values ($1, $2, $3, $4, $5, $6)
		returning `+ReviewColumns, artifact.ID, version, state, actorJSON, reason, askID))
	if err != nil {
		return model.ArtifactReview{}, model.Event{}, fmt.Errorf("insert artifact review: %w", err)
	}
	eventType := "artifact.approved"
	if state == "changes_requested" {
		eventType = "artifact.changes_requested"
	}
	event, err := broker.Append(ctx, tx, documentAskEvent(
		artifactOwner{IssueKey: artifact.IssueKey}, artifact.ID, eventType, actor,
		model.ArtifactReviewEventPayload{
			ArtifactID: artifact.ID,
			Name:       artifact.Name,
			Version:    version,
			Actor:      actor,
			Reason:     reason,
			AskID:      askID,
		},
	))
	if err != nil {
		return model.ArtifactReview{}, model.Event{}, fmt.Errorf("append %s: %w", eventType, err)
	}
	return review, event, nil
}

// LatestApprovedReview is the newest approval of artifactID, whoever gave it, or nil when nobody
// has approved the document at any version.
func LatestApprovedReview(ctx context.Context, q Queryer, artifactID string) (*model.ArtifactReview, error) {
	return newestReview(ctx, q, "latest approved review", `
		select `+ReviewColumns+`
		from artifact_reviews
		where artifact_id = $1 and state = 'approved'
		order by created_at desc, id desc
		limit 1
	`, artifactID)
}

// latestReviewBy is the newest review of artifactID a person gave, in any state, by his canonical
// login (model.CanonicalLogin), or nil when he has reviewed the document at no version.
func latestReviewBy(ctx context.Context, q Queryer, artifactID, login string) (*model.ArtifactReview, error) {
	return newestReview(ctx, q, fmt.Sprintf("latest review by %q", login), `
		select `+ReviewColumns+`
		from artifact_reviews
		where artifact_id = $1 and actor->>'kind' = 'user' and lower(trim(actor->>'id')) = $2
		order by created_at desc, id desc
		limit 1
	`, artifactID, login)
}

// changesRequestedSince reports whether anyone requested changes on review's document after
// review was given.
func changesRequestedSince(ctx context.Context, q Queryer, review model.ArtifactReview) (bool, error) {
	var requested bool
	if err := q.QueryRow(ctx, `
		select exists(
			select 1 from artifact_reviews
			where artifact_id = $1 and state = 'changes_requested' and (created_at, id) > ($2, $3::uuid)
		)
	`, review.ArtifactID, review.CreatedAt, review.ID).Scan(&requested); err != nil {
		return false, fmt.Errorf("read reviews since the approval: %w", err)
	}
	return requested, nil
}

// reviewedArtifact reads the document a review names as WriteReview takes it: its id, name and
// issue.
func reviewedArtifact(ctx context.Context, q Queryer, artifactID string) (model.Artifact, error) {
	artifact := model.Artifact{ID: artifactID}
	if err := q.QueryRow(ctx, `select name, issue_key from artifacts where id = $1`, artifactID).Scan(&artifact.Name, &artifact.IssueKey); err != nil {
		return model.Artifact{}, fmt.Errorf("read the document for its review: %w", err)
	}
	return artifact, nil
}

// newestReview runs a query selecting ReviewColumns of at most one row and returns that review,
// nil when the query selects none; what names the read in the error any other failure wraps.
func newestReview(ctx context.Context, q Queryer, what, query string, args ...any) (*model.ArtifactReview, error) {
	review, err := ScanReview(q.QueryRow(ctx, query, args...))
	if err == nil {
		return &review, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return nil, fmt.Errorf("read %s: %w", what, err)
}
