package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// Document approval: a human review pinned to a document version. Approve, or
// request changes with a reason; a later version makes an approval stale. A
// request for approval is an ask of kind "approval" with fixed options, so it
// reaches the human through the Inbox like any question; answering it, or
// reviewing from the document header, writes the review and emits
// artifact.approved / artifact.changes_requested on the document's owner.

const (
	approvalOptionApprove        = "Approve"
	approvalOptionRequestChanges = "Request changes"
)

var approvalAskOptions = []model.AskOption{
	{Label: approvalOptionApprove, Description: "Approve this version of the document."},
	{Label: approvalOptionRequestChanges, Description: "Say what must change before it can be approved."},
}

// latestVersionNumber is the newest settled version of an artifact, 0 when none exists.
func latestVersionNumber(ctx context.Context, q queryer, artifactID string) (int, error) {
	var latest *int
	if err := q.QueryRow(ctx, `select max(number) from artifact_versions where artifact_id = $1`, artifactID).Scan(&latest); err != nil {
		return 0, fmt.Errorf("latest artifact version: %w", err)
	}
	if latest == nil {
		return 0, nil
	}
	return *latest, nil
}

func scanReview(row pgx.Row) (model.ArtifactReview, error) {
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

const reviewColumns = `id::text, artifact_id::text, version, state, actor, reason, ask_id::text, created_at`

func (s *server) loadReviews(ctx context.Context, q queryer, artifactID string) ([]model.ArtifactReview, error) {
	rows, err := q.Query(ctx, `select `+reviewColumns+` from artifact_reviews where artifact_id = $1 order by created_at desc, id desc`, artifactID)
	if err != nil {
		return nil, err
	}
	reviews, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.ArtifactReview, error) { return scanReview(row) })
	if err != nil {
		return nil, err
	}
	if reviews == nil {
		reviews = []model.ArtifactReview{}
	}
	return reviews, nil
}

// openApprovalAsk is the open approval ask about an artifact, if any.
func (s *server) openApprovalAsk(ctx context.Context, q queryer, artifactID string) (*model.Ask, error) {
	ask, err := scanAsk(q.QueryRow(ctx, `
		select `+listIssueAsksColumns+`
		from asks where kind = 'approval' and state = 'open' and approval->>'artifact_id' = $1
		order by created_at desc limit 1
	`, artifactID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ask, nil
}

// attachApprovals fills Approval for every document in place: one query for the
// latest review per artifact and one for open approval asks.
func (s *server) attachApprovals(ctx context.Context, q queryer, artifacts []*model.Artifact) error {
	ids := make([]string, 0, len(artifacts))
	byID := make(map[string]*model.Artifact, len(artifacts))
	for _, artifact := range artifacts {
		if artifact.Kind != "doc" {
			continue
		}
		ids = append(ids, artifact.ID)
		byID[artifact.ID] = artifact
	}
	if len(ids) == 0 {
		return nil
	}
	latestReviews := map[string]model.ArtifactReview{}
	rows, err := q.Query(ctx, `
		select distinct on (artifact_id) `+reviewColumns+`
		from artifact_reviews where artifact_id = any($1::uuid[])
		order by artifact_id, created_at desc, id desc
	`, ids)
	if err != nil {
		return fmt.Errorf("load latest reviews: %w", err)
	}
	for rows.Next() {
		review, err := scanReview(rows)
		if err != nil {
			rows.Close()
			return err
		}
		latestReviews[review.ArtifactID] = review
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	type openRequest struct {
		id     string
		author model.Actor
	}
	requests := map[string]openRequest{}
	askRows, err := q.Query(ctx, `
		select id::text, author, approval->>'artifact_id'
		from asks where kind = 'approval' and state = 'open' and approval->>'artifact_id' = any($1::text[])
	`, ids)
	if err != nil {
		return fmt.Errorf("load open approval asks: %w", err)
	}
	for askRows.Next() {
		var id, artifactID string
		var author []byte
		if err := askRows.Scan(&id, &author, &artifactID); err != nil {
			askRows.Close()
			return err
		}
		var actor model.Actor
		if err := json.Unmarshal(author, &actor); err != nil {
			askRows.Close()
			return fmt.Errorf("decode approval ask author: %w", err)
		}
		requests[artifactID] = openRequest{id: id, author: actor}
	}
	askRows.Close()
	if err := askRows.Err(); err != nil {
		return err
	}
	for _, artifact := range byID {
		latest := 0
		for _, version := range artifact.Versions {
			if version.Number > latest {
				latest = version.Number
			}
		}
		approval := &model.ArtifactApproval{State: "draft", LatestVersion: latest}
		if review, ok := latestReviews[artifact.ID]; ok {
			version := review.Version
			at := review.CreatedAt.UTC().Format(time.RFC3339Nano)
			actor := review.Actor
			approval.Version = &version
			approval.By = &actor
			approval.At = &at
			approval.Reason = review.Reason
			approval.AskID = review.AskID
			switch {
			case review.State == "changes_requested":
				approval.State = "changes_requested"
			case review.Version == latest:
				approval.State = "approved"
			default:
				approval.State = "stale"
			}
		}
		if request, ok := requests[artifact.ID]; ok && approval.State != "approved" {
			author := request.author
			id := request.id
			approval.State = "awaiting"
			approval.RequestedBy = &author
			approval.AskID = &id
		}
		artifact.Approval = approval
	}
	return nil
}

func (s *server) attachApproval(ctx context.Context, q queryer, artifact *model.Artifact) error {
	return s.attachApprovals(ctx, q, []*model.Artifact{artifact})
}

// writeReview records a review pinned to version and appends its event; the
// caller owns the transaction and publishes the event.
func (s *server) writeReview(ctx context.Context, tx pgx.Tx, artifact model.Artifact, version int, state string, actor model.Actor, reason *string, askID *string) (model.ArtifactReview, model.Event, error) {
	actorJSON, err := encodeJSON(actor)
	if err != nil {
		return model.ArtifactReview{}, model.Event{}, err
	}
	review, err := scanReview(tx.QueryRow(ctx, `
		insert into artifact_reviews (artifact_id, version, state, actor, reason, ask_id)
		values ($1, $2, $3, $4, $5, $6)
		returning `+reviewColumns, artifact.ID, version, state, actorJSON, reason, askID))
	if err != nil {
		return model.ArtifactReview{}, model.Event{}, fmt.Errorf("insert artifact review: %w", err)
	}
	eventType := "artifact.approved"
	if state == "changes_requested" {
		eventType = "artifact.changes_requested"
	}
	event, err := s.appendEvent(ctx, tx, ownerForArtifact(artifact).event(eventType, actor, model.ArtifactReviewEventPayload{
		ArtifactID: artifact.ID,
		Name:       artifact.Name,
		Version:    version,
		Actor:      actor,
		Reason:     reason,
		AskID:      askID,
	}))
	if err != nil {
		return model.ArtifactReview{}, model.Event{}, err
	}
	return review, event, nil
}

// reviewFromAnswer maps an approval ask's answer to a review state and reason.
func reviewFromAnswer(selected []string, text *string) (string, *string, error) {
	if len(selected) != 1 {
		return "", nil, errorf(http.StatusBadRequest, "INVALID_ANSWER", "an approval ask takes exactly one of Approve or Request changes")
	}
	reason := (*string)(nil)
	if text != nil && strings.TrimSpace(*text) != "" {
		trimmed := strings.TrimSpace(*text)
		reason = &trimmed
	}
	switch selected[0] {
	case approvalOptionApprove:
		return "approved", reason, nil
	case approvalOptionRequestChanges:
		if reason == nil {
			return "", nil, errorf(http.StatusBadRequest, "REASON_REQUIRED", "requesting changes requires a reason")
		}
		return "changes_requested", reason, nil
	default:
		return "", nil, errorf(http.StatusBadRequest, "INVALID_ANSWER", "selected answers must be ask option labels")
	}
}

// GET /api/v1/artifacts/{id}/reviews
func (s *server) listArtifactReviews(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	artifact, err := s.loadArtifact(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	reviews, err := s.loadReviews(r.Context(), s.deps.Store.Pool, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reviews)
}

// POST /api/v1/artifacts/{id}/reviews  {state, reason?}  (humans only)
func (s *server) createArtifactReview(w http.ResponseWriter, r *http.Request) {
	var input struct {
		State  string  `json:"state"`
		Reason *string `json:"reason"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	state := strings.TrimSpace(input.State)
	if state != "approved" && state != "changes_requested" {
		writeError(w, "INVALID_REVIEW", http.StatusBadRequest, "state must be approved or changes_requested")
		return
	}
	var reason *string
	if input.Reason != nil && strings.TrimSpace(*input.Reason) != "" {
		trimmed := strings.TrimSpace(*input.Reason)
		reason = &trimmed
	}
	if state == "changes_requested" && reason == nil {
		writeError(w, "REASON_REQUIRED", http.StatusBadRequest, "requesting changes requires a reason")
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	artifact, err := s.loadArtifact(r.Context(), tx, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "only documents are reviewed")
		return
	}
	if err := s.requireOpenOwner(r.Context(), tx, ownerForArtifact(artifact)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	version, err := latestVersionNumber(r.Context(), tx, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if version == 0 {
		writeError(w, "NO_VERSION", http.StatusConflict, "the document has no settled version to review yet")
		return
	}
	events := []model.Event{}
	var askID *string
	if open, err := s.openApprovalAsk(r.Context(), tx, artifact.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	} else if open != nil {
		selected := approvalOptionApprove
		if state == "changes_requested" {
			selected = approvalOptionRequestChanges
		}
		answered, err := s.answerAskTx(r.Context(), tx, open.ID, actor, []string{selected}, reason)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		event, err := s.appendEvent(r.Context(), tx, ownerOf(answered.IssueKey, answered.ArtifactID).event("ask.answered", actor, answered))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		events = append(events, event)
		id := answered.ID
		askID = &id
	}
	review, event, err := s.writeReview(r.Context(), tx, artifact, version, state, actor, reason, askID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events = append(events, event)
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(events...)
	writeJSON(w, http.StatusCreated, review)
}

// POST /api/v1/artifacts/{id}/approval-requests  (any authenticated actor)
func (s *server) requestArtifactApproval(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Actor *model.Actor `json:"actor"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &input); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	artifact, err := s.loadArtifact(r.Context(), tx, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Kind != "doc" {
		writeError(w, "NOT_DOCUMENT", http.StatusBadRequest, "only documents can be approved")
		return
	}
	owner := ownerForArtifact(artifact)
	if err := s.requireOpenOwner(r.Context(), tx, owner); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	version, err := latestVersionNumber(r.Context(), tx, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if version == 0 {
		writeError(w, "NO_VERSION", http.StatusConflict, "the document has no settled version to approve yet")
		return
	}
	type response struct {
		Ask        *model.Ask             `json:"ask"`
		ArtifactID string                 `json:"artifact_id"`
		Version    int                    `json:"version"`
		Approval   model.ArtifactApproval `json:"approval"`
	}
	// An approval at the current version needs no request: return it as it stands.
	if err := s.attachApproval(r.Context(), tx, &artifact); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if artifact.Approval != nil && artifact.Approval.State == "approved" {
		writeJSON(w, http.StatusOK, response{Ask: nil, ArtifactID: artifact.ID, Version: version, Approval: *artifact.Approval})
		return
	}
	if open, err := s.openApprovalAsk(r.Context(), tx, artifact.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	} else if open != nil {
		if err := s.attachOpenedEventIDs(r.Context(), tx, []*model.Ask{open}); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, response{Ask: open, ArtifactID: artifact.ID, Version: open.Approval.Version, Approval: *artifact.Approval})
		return
	}
	var rowID string
	if err := tx.QueryRow(r.Context(), `select gen_random_uuid()::text`).Scan(&rowID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ask := model.Ask{
		ID:         rowID,
		IssueKey:   owner.IssueKey,
		ArtifactID: owner.ArtifactID,
		Author:     actor,
		Kind:       "approval",
		Question:   fmt.Sprintf("Approve %s (version %d)?", artifact.Name, version),
		Options:    approvalAskOptions,
		Multiple:   false,
		Urgency:    "high",
		Anchor:     nil,
		State:      "open",
		Approval:   &model.AskApproval{ArtifactID: artifact.ID, Name: artifact.Name, Version: version},
	}
	options, err := encodeJSON(ask.Options)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	author, err := encodeJSON(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	approval, err := encodeJSON(ask.Approval)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.QueryRow(r.Context(), `
		insert into asks (id, issue_key, artifact_id, author, question, options, multiple, urgency, anchor, kind, approval)
		values ($1, $2, $3, $4, $5, $6, false, 'high', null, 'approval', $7)
		returning created_at
	`, ask.ID, owner.IssueKey, owner.ArtifactID, author, ask.Question, options, approval).Scan(&ask.CreatedAt); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := refs.Replace(r.Context(), tx, "ask", ask.ID, ask.Question, s.deps.ServerURL); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, owner.event("ask.opened", actor, ask))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ask.OpenedEventID = &event.ID
	event.Payload = ask
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	awaiting := *artifact.Approval
	awaiting.State = "awaiting"
	awaiting.RequestedBy = &actor
	awaiting.AskID = &ask.ID
	writeJSON(w, http.StatusCreated, response{Ask: &ask, ArtifactID: artifact.ID, Version: version, Approval: awaiting})
}
