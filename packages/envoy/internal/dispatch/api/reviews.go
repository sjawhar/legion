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
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Document approval: a human review pinned to a document version. Approve, or
// request changes with a reason. A later version makes an approval stale, unless
// it is the version a human's own answer to a decision block writes and that
// answer is the whole change since the version he approved, in which case
// Dispatch carries his approval to it (docs writes that carry). A request for
// approval is an ask of kind "approval" with fixed options, so it reaches the
// human through the Inbox like any question; one opened on a document with an
// earlier approval carries that approval's version, time and approver
// (approval.approved: version, at, by). Answering it, or reviewing from
// the document header, writes the review (docs.QueueReview) and emits
// artifact.approved / artifact.changes_requested on the document's owner.

var approvalAskOptions = []model.AskOption{
	{Label: docs.ApprovalOptionApprove, Description: "Approve this version of the document."},
	{Label: docs.ApprovalOptionRequestChanges, Description: "Say what must change before it can be approved."},
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

// loadReviews is every review of the document, newest first in the order its events committed
// (docs.ReviewOrder).
func (s *server) loadReviews(ctx context.Context, q queryer, artifactID string) ([]model.ArtifactReview, error) {
	rows, err := q.Query(ctx, `select `+docs.ReviewColumns+` from artifact_reviews where artifact_id = $1 order by `+docs.ReviewOrder, artifactID)
	if err != nil {
		return nil, err
	}
	reviews, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (model.ArtifactReview, error) { return docs.ScanReview(row) })
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
		select distinct on (artifact_id) `+docs.ReviewColumns+`
		from artifact_reviews where artifact_id = any($1::uuid[])
		order by artifact_id, `+docs.ReviewOrder+`
	`, ids)
	if err != nil {
		return fmt.Errorf("load latest reviews: %w", err)
	}
	for rows.Next() {
		review, err := docs.ScanReview(rows)
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
			approval.Seq = review.EventSeq
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
	case docs.ApprovalOptionApprove:
		return "approved", reason, nil
	case docs.ApprovalOptionRequestChanges:
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
	// The answer of the open approval request and the review are this route's writes, queued on
	// the ledger in the order it makes them and sent as one batch by Commit.
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
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
	appended := []*model.Event{}
	var askID *string
	if open != nil {
		selected := docs.ApprovalOptionApprove
		if state == "changes_requested" {
			selected = docs.ApprovalOptionRequestChanges
		}
		answered, answeredEvents, err := s.closeAskTx(documentCtx, tx, ledger, open.ID, actor, answerTransition(actor, []string{selected}, reason, answerOptions{}))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		appended = append(appended, answeredEvents...)
		askID = &answered.ID
	}
	var review model.ArtifactReview
	reviewEvent := new(model.Event)
	if err := ledger.Queue(func(b *store.WriteBatch) error {
		return docs.QueueReview(b, s.deps.Events, artifact, version, state, actor, reason, askID, func(written model.ArtifactReview, event model.Event) {
			review, *reviewEvent = written, event
		})
	}); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	appended = append(appended, reviewEvent)
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(appendedEvents(appended...)...)
	WriteJSON(w, http.StatusCreated, review)
}

// POST /api/v1/artifacts/{id}/approval-requests  {summary?}  (any authenticated actor)
func (s *server) requestArtifactApproval(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Actor   *model.Actor `json:"actor"`
		Summary *string      `json:"summary"`
	}
	if !s.decodeOptionalJSON(w, r, &input) {
		return
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
	// The row this request opens or hands back, the references its question cites and its events
	// are this route's writes, queued on the ledger in the order it makes them and sent as one
	// batch by Commit. Everything the request is decided on - the document, its settled version,
	// the open request and the turn it waits on - is read before the first of them.
	documentCtx, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
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
	var appended []*model.Event
	var opened *model.Event
	if open != nil {
		if appended, err = s.renewApprovalAsk(documentCtx, tx, ledger, open, actor, version, summary); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	} else {
		if ask, opened, err = s.openApprovalAsk(documentCtx, tx, ledger, owner, artifact, actor, version, question); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		appended = []*model.Event{opened}
	}
	// The approval as this request leaves it: awaiting, waiting on whom the request now waits on.
	approval := approvalAfterRequest(*artifact.Approval, *ask)
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if opened != nil {
		// The id of the event that opened the request is known once the write phase has run.
		ask.OpenedEventID = &opened.ID
	}
	s.publish(appendedEvents(appended...)...)
	// 201 says this request opened the request, reworded it or handed it to the human; 200 says the
	// open request already stood as asked. A rewording is never this route's only write: the
	// hand-back that follows it appends an event of its own, which is published here, while the
	// rewording's goes to the ledger and is published by Commit.
	status := http.StatusOK
	if len(appended) > 0 {
		status = http.StatusCreated
	}
	WriteJSON(w, status, response{Ask: ask, ArtifactID: artifact.ID, Version: version, Approval: approval})
}

// approvalAfterRequest is the document's approval as this request's writes leave it: the approval
// read before them (attachApprovals) with the request this route opened or handed back written
// over it. Those writes are queued, so the rows hold none of it until Commit and no read of this
// transaction can answer from them; the overlay applies the rule such a read would
// (waitingOnExpression). A request waits on its agent while a version it has not handed back has
// moved it past the version it names, and on the human otherwise: a request this route opened or
// handed back names the document's settled version, and the reply its hand-back recorded is the
// thread's newest one, which the owner lock this transaction holds keeps newest until it commits.
func approvalAfterRequest(approval model.ArtifactApproval, ask model.Ask) model.ArtifactApproval {
	requester := ask.Author
	id := ask.ID
	approval.State = "awaiting"
	approval.RequestedBy = &requester
	approval.AskID = &id
	approval.WaitingOn = "human"
	if ask.Approval.RequestedVersion < ask.Approval.Version {
		approval.WaitingOn = "agent"
	}
	return approval
}

// openApprovalAsk queues the insert of the document's approval request at version, asking
// question, and the append of its ask.opened event; the requester follows it. A document a human
// approved before carries that approval's version, time and approver on the request
// (approval.approved), so the human reads what the re-approval follows; a first request carries
// none. The event is filled in once Commit has run the write phase (queueLoggedEvent), and so is
// the ask's OpenedEventID, which its caller takes from the event then.
func (s *server) openApprovalAsk(
	ctx context.Context,
	tx pgx.Tx,
	ledger *docs.Ledger,
	owner owner,
	artifact model.Artifact,
	actor model.Actor,
	version int,
	question string,
) (*model.Ask, *model.Event, error) {
	// The row's created_at is the transaction's now(), its column default, read with its id and
	// written explicitly, so the request this route answers with carries the time the row will
	// hold.
	var rowID string
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `select gen_random_uuid()::text, now()`).Scan(&rowID, &createdAt); err != nil {
		return nil, nil, err
	}
	approval := &model.AskApproval{
		ArtifactID: artifact.ID, Name: artifact.Name, Version: version, RequestedVersion: version,
	}
	approved, err := docs.LatestApprovedReview(ctx, tx, artifact.ID)
	if err != nil {
		return nil, nil, err
	}
	if approved != nil {
		approval.Approved = &model.AskApprovalApproved{Version: approved.Version, At: timestampValue(approved.CreatedAt), By: approved.Actor}
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
		CreatedAt:  createdAt,
		Approval:   approval,
	}
	options, err := encodeJSON(ask.Options)
	if err != nil {
		return nil, nil, err
	}
	author, err := encodeJSON(actor)
	if err != nil {
		return nil, nil, err
	}
	encodedApproval, err := docs.EncodeApproval(ask.Approval)
	if err != nil {
		return nil, nil, err
	}
	if err := queueExec(ledger, `
		insert into asks (id, issue_key, artifact_id, author, question, options, multiple, urgency, anchor, kind, approval, created_at)
		values ($1, $2, $3, $4, $5, $6, false, 'high', null, 'approval', $7, $8)
	`, ask.ID, owner.IssueKey, owner.ArtifactID, author, ask.Question, options, encodedApproval, ask.CreatedAt); err != nil {
		return nil, nil, err
	}
	if err := ledger.Queue(func(b *store.WriteBatch) error {
		asks.QueueFollowAuthor(b, ask.ID, actor)
		return nil
	}); err != nil {
		return nil, nil, err
	}
	askChanges, err := s.queueReferences(ctx, tx, ledger, "ask", ask.ID, ask.Question)
	if err != nil {
		return nil, nil, err
	}
	// The request's thread_logins are read by its event's insert, after the row's own insert and
	// the user_threads trigger it fires.
	event, err := s.queueLoggedEvent(ledger, owner.event(
		"ask.opened", actor, model.NewAskEventPayload(ask, askChanges),
	), events.ThreadLoginsOf(ask.ID), "ask", ask.ID)
	if err != nil {
		return nil, nil, err
	}
	return &ask, event, nil
}

// renewApprovalAsk answers a request on a document whose approval request is already open, and
// returns the events it queued for this route to publish after Commit. summary is the request's,
// "" when it named none, which keeps the open request's own.
//
// The turn decides, read once before anything is written. A request waiting on its agent - moved
// to a later version, or answered in its thread - goes back to the human: a new summary first
// rewords it (docs.RewriteApprovalAsk, ask.edited, whose event goes to the ledger, which publishes
// it), then handBackApprovalAsk hands it back. A request already waiting on the human is left as
// it stands. The same summary, or none, is a retry and records nothing. A different one is
// refused, since an approval request carries nothing new: rewording it would rewrite the card the
// human is reading, with no turn of theirs, and refuse an answer they had started (ASK_EDITED).
func (s *server) renewApprovalAsk(
	ctx context.Context,
	tx pgx.Tx,
	ledger *docs.Ledger,
	open *model.Ask,
	actor model.Actor,
	version int,
	summary string,
) ([]*model.Event, error) {
	openSummary, err := docs.ApprovalAskSummary(*open)
	if err != nil {
		return nil, err
	}
	if summary == "" {
		summary = openSummary
	}
	// Every version write moves the open request to its version (docs.MoveApprovalAsk) under the
	// owner lock this request holds, so the turn read here is the one every read reports.
	waitingOn, err := askWaitingOn(ctx, tx, open.ID)
	if err != nil {
		return nil, err
	}
	var appended []*model.Event
	switch {
	case waitingOn == "human" && summary != openSummary:
		return nil, errorf(http.StatusConflict, "APPROVAL_WAITS_ON_HUMAN",
			"the approval request already waits on the human, asking %q, and a different summary would rewrite the card they are reading, so nothing was changed; raise what changed with the human first, in the request's thread or as a decision block in the document, and once they have agreed to every point in it and a reply or a new version leaves the request waiting on you, hand it back with the new summary",
			open.Question)
	case waitingOn == "agent":
		if summary != openSummary {
			if err := docs.RewriteApprovalAsk(ctx, tx, s.deps.Events, open, actor, version, summary, s.deps.ServerURL); err != nil {
				return nil, err
			}
		}
		handedBack, err := s.handBackApprovalAsk(ledger, open, actor, version)
		if err != nil {
			return nil, err
		}
		appended = append(appended, handedBack)
	}
	if err := ledger.Queue(func(b *store.WriteBatch) error {
		asks.QueueFollowAuthor(b, open.ID, actor)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := s.attachOpenedEventIDs(ctx, tx, []*model.Ask{open}); err != nil {
		return nil, err
	}
	return appended, nil
}

// handBackApprovalAsk queues the return of the open approval request to the human at version and
// the append of its ask.handed_back event, which is filled in once Commit has run the write phase
// (queueEvent). It records version as the one handed back and the thread's newest reply as the one
// this hand-back answered (waitingOnExpression), which its own statement reads in that phase. The
// caller holds the owner lock every reply takes before it inserts, so no reply commits between the
// turn it read and this transaction's commit, and a reply that inserts after it gets a later
// created_at (the insert time, migration 0065), sorts after the one recorded and decides the turn,
// however early its own transaction began. The question is left as it stands: a request that
// changes it rewords first through docs.RewriteApprovalAsk, which ask.edited records.
func (s *server) handBackApprovalAsk(ledger *docs.Ledger, ask *model.Ask, actor model.Actor, version int) (*model.Event, error) {
	ask.Approval.RequestedVersion = version
	approval, err := docs.EncodeApproval(ask.Approval)
	if err != nil {
		return nil, err
	}
	if err := queueExec(ledger, `
		update asks a
		set approval = $2, handed_back_reply_id = (select lr.id from (`+newestReply+`) lr)
		where a.id = $1
	`, ask.ID, approval); err != nil {
		return nil, err
	}
	return s.queueEvent(ledger, ownerOf(ask.IssueKey, ask.ArtifactID).event(
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
