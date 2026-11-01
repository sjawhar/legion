package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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
// carries a sequence, whichever release wrote it: this release's QueueReview writes it, and the
// migration's deferred trigger gives one to a review the release before it writes during a
// rollout or a rollback. A row without one was committed before that migration, so every row that
// carries a sequence is newer than every row without one, which `nulls last` says. created_at is
// the review's transaction start, so it places two reviews only when neither carries a sequence.
// This holds only while QueueReview is the one writer of the table: the api package's
// TestOnlyQueueReviewWritesReviews refuses an insert into or update of artifact_reviews anywhere
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

// QueueReview records a review of artifact pinned to version and appends its event,
// artifact.approved or artifact.changes_requested, on the document's owner: its issue when it
// belongs to one, else the document itself. askID is the approval ask the review answers, nil
// when it was given from the document header with no request open. The review row keeps the
// event's sequence on that owner (event_seq), which places a read of the review among the owner's
// events.
//
// Every caller already holds the owner's lock before it queues this, and its statements go on one
// write phase's batch: the row is inserted before the event is appended and its sequence written
// after. The review's id is chosen here, so the statement that writes that sequence names the row,
// and takes the sequence from the event the statements before it appended (store.LatestEventID):
// nothing is read back in Go between them. The update that writes the sequence runs the
// foreign-key checks again, since Postgres re-checks a row its own transaction inserted, and so
// asks for the same FOR KEY SHARE locks the insert took: the transaction already holds them, so
// the request never waits. Migration 0120's trigger, which gives the release before this one's
// reviews their sequence, would give this row its sequence too, but only at the commit, the first
// moment the event exists for every release; so the batch writes it itself, and the row is right
// for any read the caller makes before it commits. The trigger then finds the row sequenced and
// leaves it. written receives the review and its event as the batch leaves them, and the caller,
// which owns the transaction, publishes the event after its commit.
func QueueReview(b *store.WriteBatch, broker *events.Broker, artifact model.Artifact, version int, state string, actor model.Actor, reason *string, askID *string, written func(model.ArtifactReview, model.Event)) error {
	actorJSON, err := json.Marshal(actor)
	if err != nil {
		return fmt.Errorf("encode review actor: %w", err)
	}
	eventType := "artifact.approved"
	if state == "changes_requested" {
		eventType = "artifact.changes_requested"
	}
	id := uuid.NewString()
	var review model.ArtifactReview
	var event model.Event
	b.Queue("", `
		insert into artifact_reviews (id, artifact_id, version, state, actor, reason, ask_id)
		values ($1, $2, $3, $4, $5, $6, $7)
		returning `+ReviewColumns, id, artifact.ID, version, state, actorJSON, reason, askID).QueryRow(func(row pgx.Row) error {
		scanned, err := ScanReview(row)
		if err != nil {
			return fmt.Errorf("insert artifact review: %w", err)
		}
		review = scanned
		return nil
	})
	if err := broker.QueueAppend(b, documentAskEvent(
		artifactOwner{IssueKey: artifact.IssueKey}, artifact.ID, eventType, actor,
		model.ArtifactReviewEventPayload{
			ArtifactID: artifact.ID,
			Name:       artifact.Name,
			Version:    version,
			Actor:      actor,
			Reason:     reason,
			AskID:      askID,
		},
	), func(appended model.Event) { event = appended }); err != nil {
		return fmt.Errorf("append %s: %w", eventType, err)
	}
	b.Queue("record the sequence of "+eventType, `
		update artifact_reviews set event_seq = (select seq from events where id = `+store.LatestEventID+`)
		where id = $1
	`, id).Exec(func(pgconn.CommandTag) error {
		seq := int64(event.Seq)
		review.EventSeq = &seq
		written(review, event)
		return nil
	})
	return nil
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

// newestReviewQuery selects the newest review of the document $1 names, in any state. Like the
// two above it reads one row from artifact_reviews_by_artifact_event_seq (migration 0120), the
// index in ReviewOrder's own order, rather than sorting every review of the document.
const newestReviewQuery = `
	select ` + ReviewColumns + `
	from artifact_reviews
	where artifact_id = $1
	order by ` + ReviewOrder + `
	limit 1
`

// LatestApprovedReview is the newest approval of artifactID, whoever gave it, or nil when nobody
// has approved the document at any version.
func LatestApprovedReview(ctx context.Context, q Queryer, artifactID string) (*model.ArtifactReview, error) {
	return newestReview(ctx, q, "latest approved review", latestApprovedReviewQuery, artifactID)
}

// NewestReview is the newest review of artifactID in any state, whoever gave it, or nil when
// nobody has reviewed the document. It orders as every newest-review read does, by the order the
// reviews' events committed (ReviewOrder): artifact_reviews.created_at is its transaction's start
// and its id is random, so a review that committed later can carry the earlier pair - an approval
// a version write carries forward (carryApproval) against an approval committed while that write
// was open, among others.
func NewestReview(ctx context.Context, q Queryer, artifactID string) (*model.ArtifactReview, error) {
	return newestReview(ctx, q, "newest review", newestReviewQuery, artifactID)
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

// reviewedArtifact reads the document a review names as QueueReview takes it: its id, name and
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
