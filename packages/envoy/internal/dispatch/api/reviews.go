package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
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

// latestVersionNumber is the newest version an artifact has, nil when it has none.
func latestVersionNumber(ctx context.Context, q queryer, artifactID string) (*int, error) {
	var latest *int
	if err := q.QueryRow(ctx, `select max(number) from artifact_versions where artifact_id = $1`, artifactID).Scan(&latest); err != nil {
		return nil, fmt.Errorf("latest artifact version: %w", err)
	}
	return latest, nil
}

// settledVersionNumber is latestVersionNumber for the callers that read a missing version as 0.
func settledVersionNumber(ctx context.Context, q queryer, artifactID string) (int, error) {
	latest, err := latestVersionNumber(ctx, q, artifactID)
	if err != nil || latest == nil {
		return 0, err
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
		id        string
		author    model.Actor
		waitingOn string
	}
	requests := map[string]openRequest{}
	askRows, err := q.Query(ctx, `
		select a.id::text, a.author, a.approval->>'artifact_id', `+waitingOnExpression+`
		from asks a`+lastReplyJoin+`
		where a.kind = 'approval' and a.state = 'open' and a.approval->>'artifact_id' = any($1::text[])
	`, ids)
	if err != nil {
		return fmt.Errorf("load open approval asks: %w", err)
	}
	for askRows.Next() {
		var id, artifactID, waitingOn string
		var author []byte
		if err := askRows.Scan(&id, &author, &artifactID, &waitingOn); err != nil {
			askRows.Close()
			return err
		}
		var actor model.Actor
		if err := json.Unmarshal(author, &actor); err != nil {
			askRows.Close()
			return fmt.Errorf("decode approval ask author: %w", err)
		}
		requests[artifactID] = openRequest{id: id, author: actor, waitingOn: waitingOn}
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
			approval.WaitingOn = request.waitingOn
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
	WriteJSON(w, http.StatusOK, reviews)
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
	version, err := settledVersionNumber(r.Context(), tx, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if version == 0 {
		writeError(w, "NO_VERSION", http.StatusConflict, "the document has no settled version to review yet")
		return
	}
	// A moved approval ask names this latest version and is answered by the header action, keeping
	// its thread as the review's provenance.
	open, err := docs.OpenApprovalAsk(r.Context(), tx, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events := []model.Event{}
	var askID *string
	if open != nil {
		selected := approvalOptionApprove
		if state == "changes_requested" {
			selected = approvalOptionRequestChanges
		}
		answered, answeredEvents, err := s.closeAskTx(r.Context(), tx, open.ID, actor, answerTransition(actor, []string{selected}, reason, nil, nil))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		events = append(events, answeredEvents...)
		askID = &answered.ID
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
	WriteJSON(w, http.StatusCreated, review)
}

// POST /api/v1/artifacts/{id}/approval-requests  {summary?}  (any authenticated actor)
func (s *server) requestArtifactApproval(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Actor   *model.Actor `json:"actor"`
		Summary *string      `json:"summary"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &input); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// A summary is trimmed and must hold text, so "" below means the request named none.
	var summary string
	if input.Summary != nil {
		summary = strings.TrimSpace(*input.Summary)
		if summary == "" {
			writeError(w, "SUMMARY_INPUT", http.StatusBadRequest, "summary is blank; say what the human is approving, or omit it")
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
	version, err := settledVersionNumber(r.Context(), tx, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if version == 0 {
		writeError(w, "NO_VERSION", http.StatusConflict, "the document has no settled version to approve yet")
		return
	}
	// A supplied summary is validated even when this request opens no row.
	question, err := approvalQuestion(artifact.Name, version, summary)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	type response struct {
		Ask        *model.Ask             `json:"ask"`
		ArtifactID string                 `json:"artifact_id"`
		Version    int                    `json:"version"`
		Approval   model.ArtifactApproval `json:"approval"`
	}
	if err := s.attachApproval(r.Context(), tx, &artifact); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// An approval at the current version needs no request: return it as it stands.
	if artifact.Approval != nil && artifact.Approval.State == "approved" {
		WriteJSON(w, http.StatusOK, response{Ask: nil, ArtifactID: artifact.ID, Version: version, Approval: *artifact.Approval})
		return
	}
	open, err := docs.OpenApprovalAsk(r.Context(), tx, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ask := open
	var events []model.Event
	if open != nil {
		if events, err = s.renewApprovalAsk(r.Context(), tx, open, actor, version, summary); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	} else {
		var opened model.Event
		if ask, opened, err = s.openApprovalAsk(r.Context(), tx, owner, artifact, actor, version, question); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		events = []model.Event{opened}
	}
	// The approval as this request leaves it: awaiting, waiting on whom the request now waits on.
	if err := s.attachApproval(r.Context(), tx, &artifact); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(events...)
	// 201 says this request opened the request, reworded it or handed it to the human; 200 says the
	// open request already stood as asked.
	status := http.StatusOK
	if len(events) > 0 {
		status = http.StatusCreated
	}
	WriteJSON(w, status, response{Ask: ask, ArtifactID: artifact.ID, Version: version, Approval: *artifact.Approval})
}

// openApprovalAsk inserts the document's approval request at version, asking question, and appends
// its ask.opened event; the requester follows it.
func (s *server) openApprovalAsk(
	ctx context.Context,
	tx pgx.Tx,
	owner owner,
	artifact model.Artifact,
	actor model.Actor,
	version int,
	question string,
) (*model.Ask, model.Event, error) {
	var rowID string
	if err := tx.QueryRow(ctx, `select gen_random_uuid()::text`).Scan(&rowID); err != nil {
		return nil, model.Event{}, err
	}
	ask := model.Ask{
		ID:         rowID,
		IssueKey:   owner.IssueKey,
		ArtifactID: owner.ArtifactID,
		Author:     actor,
		Kind:       "approval",
		Question:   question,
		Options:    approvalAskOptions,
		Multiple:   false,
		Urgency:    "high",
		Anchor:     nil,
		State:      "open",
		Approval: &model.AskApproval{
			ArtifactID: artifact.ID, Name: artifact.Name, Version: version, RequestedVersion: version,
		},
	}
	options, err := encodeJSON(ask.Options)
	if err != nil {
		return nil, model.Event{}, err
	}
	author, err := encodeJSON(actor)
	if err != nil {
		return nil, model.Event{}, err
	}
	approval, err := docs.EncodeApproval(ask.Approval)
	if err != nil {
		return nil, model.Event{}, err
	}
	if err := tx.QueryRow(ctx, `
		insert into asks (id, issue_key, artifact_id, author, question, options, multiple, urgency, anchor, kind, approval)
		values ($1, $2, $3, $4, $5, $6, false, 'high', null, 'approval', $7)
		returning created_at
	`, ask.ID, owner.IssueKey, owner.ArtifactID, author, ask.Question, options, approval).Scan(&ask.CreatedAt); err != nil {
		return nil, model.Event{}, err
	}
	if err := asks.FollowAuthor(ctx, tx, ask.ID, actor); err != nil {
		return nil, model.Event{}, err
	}
	askChanges, err := s.replaceReferences(ctx, tx, "ask", ask.ID, ask.Question)
	if err != nil {
		return nil, model.Event{}, err
	}
	event, err := s.appendEvent(ctx, tx, owner.event(
		"ask.opened", actor, model.NewAskEventPayload(ask, askChanges),
	))
	if err != nil {
		return nil, model.Event{}, err
	}
	if err := refs.Stamp(ctx, tx, "ask", ask.ID, event.ID); err != nil {
		return nil, model.Event{}, err
	}
	ask.OpenedEventID = &event.ID
	return &ask, event, nil
}

// renewApprovalAsk answers a request on a document whose approval request is already open, and
// returns the events it appended. summary is the request's, "" when it named none, which keeps the
// open request's own.
//
// The turn decides, read once before anything is written. A request waiting on its agent - moved
// to a later version, or answered in its thread - goes back to the human: a new summary first
// rewords it (docs.RewriteApprovalAsk, ask.edited), then handBackApprovalAsk hands it back. A
// request already waiting on the human is left as it stands. The same summary, or none, is a retry
// and records nothing. A different one is refused, since an approval request carries nothing new:
// rewording it would rewrite the card the human is reading, with no turn of theirs, and refuse an
// answer they had started (ASK_EDITED).
func (s *server) renewApprovalAsk(
	ctx context.Context,
	tx pgx.Tx,
	open *model.Ask,
	actor model.Actor,
	version int,
	summary string,
) ([]model.Event, error) {
	openSummary, err := docs.ApprovalAskSummary(*open)
	if err != nil {
		return nil, err
	}
	if summary == "" {
		summary = openSummary
	}
	// Every version write moves the open request to its version (docs.MoveApprovalAsk) under the
	// owner row this request holds, so the turn read here is the one every read reports.
	waitingOn, err := askWaitingOn(ctx, tx, open.ID)
	if err != nil {
		return nil, err
	}
	var events []model.Event
	switch {
	case waitingOn == "human" && summary != openSummary:
		return nil, errorf(http.StatusConflict, "APPROVAL_WAITS_ON_HUMAN",
			"the approval request already waits on the human, asking %q, and a different summary would rewrite the card they are reading, so nothing was changed; raise what changed with the human first, in the request's thread or as a decision block in the document, and once they have agreed to every point in it and a reply or a new version leaves the request waiting on you, hand it back with the new summary",
			open.Question)
	case waitingOn == "agent":
		if summary != openSummary {
			edited, err := docs.RewriteApprovalAsk(ctx, tx, s.deps.Events, open, actor, version, summary, s.deps.ServerURL)
			if err != nil {
				return nil, err
			}
			events = append(events, edited)
		}
		handedBack, err := s.handBackApprovalAsk(ctx, tx, open, actor, version)
		if err != nil {
			return nil, err
		}
		events = append(events, handedBack)
	}
	if err := asks.FollowAuthor(ctx, tx, open.ID, actor); err != nil {
		return nil, err
	}
	if err := s.attachOpenedEventIDs(ctx, tx, []*model.Ask{open}); err != nil {
		return nil, err
	}
	return events, nil
}

// handBackApprovalAsk returns the open approval request to the human at version and appends its
// ask.handed_back event. It records version as the one handed back and the thread's newest reply as
// the one this hand-back answered (waitingOnExpression). The caller holds the owner row every reply
// takes before it inserts, so no reply commits between that read and this transaction's commit, and
// a reply that inserts after it gets a later created_at (the insert time, migration 0065), sorts
// after the one recorded and decides the turn, however early its own transaction began. The
// question is left as it stands: a request that changes it rewords first through
// docs.RewriteApprovalAsk, which ask.edited records.
func (s *server) handBackApprovalAsk(ctx context.Context, tx pgx.Tx, ask *model.Ask, actor model.Actor, version int) (model.Event, error) {
	ask.Approval.RequestedVersion = version
	approval, err := docs.EncodeApproval(ask.Approval)
	if err != nil {
		return model.Event{}, err
	}
	if _, err := tx.Exec(ctx, `
		update asks a
		set approval = $2, handed_back_reply_id = (select lr.id from (`+newestReply+`) lr)
		where a.id = $1
	`, ask.ID, approval); err != nil {
		return model.Event{}, fmt.Errorf("hand approval ask back: %w", err)
	}
	return s.appendEvent(ctx, tx, ownerOf(ask.IssueKey, ask.ArtifactID).event(
		"ask.handed_back", actor, model.NewAskEventPayload(*ask, model.ReferenceChanges{}),
	))
}

// maxApprovalVersion is the largest version an approval ask can name: asks_approval_kind_check
// (migration 0064) admits a version of at most ten digits.
const maxApprovalVersion = 9_999_999_999

// approvalQuestion is an approval ask's question: the document and version it names, then the
// requester's summary of what the human is approving, when one was given. A summary that would
// take the question past the ask cap is refused naming the characters left for it.
func approvalQuestion(name string, version int, summary string) (string, error) {
	question := docs.ApprovalQuestion(name, version, summary)
	if summary == "" {
		return question, nil
	}
	// A version move rebuilds the question at the document's new version without asking again
	// (docs.MoveApprovalAsk), so the summary is budgeted against the longest version the request can
	// reach, and no move takes a question accepted here past the cap. It follows that prefix after
	// one space.
	left := max(0, maxAskQuestion16-len16(docs.ApprovalQuestion(name, maxApprovalVersion, ""))-1)
	if length := len16(summary); length > left {
		return "", capExceededError("summary", length, left)
	}
	return question, nil
}
