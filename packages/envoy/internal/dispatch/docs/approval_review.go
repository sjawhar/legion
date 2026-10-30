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
const ReviewColumns = `id::text, artifact_id::text, version, state, actor, reason, ask_id::text, created_at, event_seq`

// ReviewOrder sorts a document's reviews newest first by the order their events committed: the
// event sequence each review kept on its document's owner, then, for a review recorded before the
// sequence was kept, its own time and id. Every review written once migration 0120 committed
// carries a sequence, whichever release wrote it: this release's WriteReview writes it, and the
// migration's deferred trigger gives one to a review the release before it writes during a
// rollout or a rollback. A row without one was committed before that migration, so every row that
// carries a sequence is newer than every row without one, which `nulls last` says. created_at is
// the review's transaction start, so it places two reviews only when neither carries a sequence.
// This holds only while WriteReview is the one writer of the table: the api package's
// TestOnlyWriteReviewWritesReviews refuses an insert into or update of artifact_reviews anywhere
// else in the module. Its id is the table's column, artifact_reviews.id: a bare `id` in an order by
// names the output column ReviewColumns selects, `id::text`, which orders the same and which no
// index holds, so artifact_reviews_by_artifact_event_seq could not give the order whole. Every read
// that orders by it selects from artifact_reviews under that name.
const ReviewOrder = `event_seq desc nulls last, created_at desc, artifact_reviews.id desc`

// ScanReview decodes one ReviewColumns row.
func ScanReview(row pgx.Row) (model.ArtifactReview, error) {
	var review model.ArtifactReview
	var actor []byte
	if err := row.Scan(&review.ID, &review.ArtifactID, &review.Version, &review.State, &actor, &review.Reason, &review.AskID, &review.CreatedAt, &review.EventSeq); err != nil {
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
// when it was given from the document header with no request open. The review row keeps the
// event's sequence on that owner (event_seq), which places a read of the review among the owner's
// events. Every caller already holds the owner's row (and, when it appends `ask.answered` first,
// the commit-order lock) before it calls this; inside WriteReview the row is inserted before the
// event is appended and its sequence written after. The update that writes the sequence runs the
// foreign-key checks again, since Postgres re-checks a row its own transaction inserted, and so asks
// for the same FOR KEY SHARE locks the insert took: the transaction already holds them, so the
// request never waits. Migration 0120's trigger, which gives the release
// before this one's reviews their sequence, would give this row its sequence too, but only at the
// commit, the first moment the event exists for every release; so WriteReview writes it itself,
// and the row is right for any read the caller makes before it commits. The trigger then finds the
// row sequenced and leaves it. The caller owns the transaction and publishes the event.
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
	seq := int64(event.Seq)
	if _, err := tx.Exec(ctx, `update artifact_reviews set event_seq = $2 where id = $1`, review.ID, seq); err != nil {
		return model.ArtifactReview{}, model.Event{}, fmt.Errorf("record the sequence of %s: %w", eventType, err)
	}
	review.EventSeq = &seq
	return review, event, nil
}

// latestApprovedReviewQuery selects the newest approval of the document $1 names. Like
// latestReviewByQuery it reads one row from artifact_reviews_by_artifact_event_seq (migration
// 0120), the index in ReviewOrder's own order, rather than sorting every review of the document.
const latestApprovedReviewQuery = `
	select ` + ReviewColumns + `
	from artifact_reviews
	where artifact_id = $1 and state = 'approved'
	order by ` + ReviewOrder + `
	limit 1
`

// latestReviewByQuery selects the newest review of the document $1 names given by the person
// whose canonical login $2 names.
const latestReviewByQuery = `
	select ` + ReviewColumns + `
	from artifact_reviews
	where artifact_id = $1 and actor->>'kind' = 'user' and lower(trim(actor->>'id')) = $2
	order by ` + ReviewOrder + `
	limit 1
`

// LatestApprovedReview is the newest approval of artifactID, whoever gave it, or nil when nobody
// has approved the document at any version.
func LatestApprovedReview(ctx context.Context, q Queryer, artifactID string) (*model.ArtifactReview, error) {
	return newestReview(ctx, q, "latest approved review", latestApprovedReviewQuery, artifactID)
}

// latestReviewBy is the newest review of artifactID a person gave, in any state, by his canonical
// login (model.CanonicalLogin), or nil when he has reviewed the document at no version.
func latestReviewBy(ctx context.Context, q Queryer, artifactID, login string) (*model.ArtifactReview, error) {
	return newestReview(ctx, q, fmt.Sprintf("latest review by %q", login), latestReviewByQuery, artifactID, login)
}

// changesRequestedSince reports whether anyone requested changes on review's document after
// review was given, in the order the two events committed: by their sequences when both carry one,
// else a sequenced request after one that carries none, and by time and id when neither does
// (ReviewOrder's rule, read one row at a time).
func changesRequestedSince(ctx context.Context, q Queryer, review model.ArtifactReview) (bool, error) {
	var requested bool
	if err := q.QueryRow(ctx, `
		select exists(
			select 1 from artifact_reviews
			where artifact_id = $1 and state = 'changes_requested'
			and case
				when $4::bigint is not null and event_seq is not null then event_seq > $4::bigint
				when $4::bigint is not null then false
				when event_seq is not null then true
				else (created_at, id) > ($2, $3::uuid)
			end
		)
	`, review.ArtifactID, review.CreatedAt, review.ID, review.EventSeq).Scan(&requested); err != nil {
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
