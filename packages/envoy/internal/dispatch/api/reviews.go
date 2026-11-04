package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
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
// answer is the whole change since the version he approved, or a version whose
// text is byte for byte the approved version's with no decision block in it
// decided since; then Dispatch carries the approval to it (docs writes that carry).
// A request for approval is an ask of kind "approval" with fixed options, so it reaches the
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
		documents []model.AskApprovalDocument
	}
	requests := map[string]openRequest{}
	// One row per document of each open request naming one of these documents, so a request that
	// covers a spec and its Verification document is found by either and reported on both.
	askRows, err := q.Query(ctx, `
		select a.id::text, a.author, document ->> 'artifact_id', a.approval -> 'documents', `+waitingOnExpression+`
		from asks a`+lastReplyJoin+`
		cross join lateral jsonb_array_elements(a.approval -> 'documents') document
		where a.kind = 'approval' and a.state = 'open'
		  and `+docs.ApprovalDocumentsJSONPath+` ?| $1::text[]
		  and document ->> 'artifact_id' = any($1::text[])
	`, ids)
	if err != nil {
		return fmt.Errorf("load open approval asks: %w", err)
	}
	for askRows.Next() {
		var id, artifactID, waitingOn string
		var author, documents []byte
		if err := askRows.Scan(&id, &author, &artifactID, &documents, &waitingOn); err != nil {
			askRows.Close()
			return err
		}
		var actor model.Actor
		if err := json.Unmarshal(author, &actor); err != nil {
			askRows.Close()
			return fmt.Errorf("decode approval ask author: %w", err)
		}
		var named []model.AskApprovalDocument
		if err := json.Unmarshal(documents, &named); err != nil {
			askRows.Close()
			return fmt.Errorf("decode approval ask documents: %w", err)
		}
		request := openRequest{id: id, author: actor, waitingOn: waitingOn}
		// One document is the document being read, which the chip already names; two or more are
		// what it must spell out.
		if len(named) > 1 {
			request.documents = named
		}
		requests[artifactID] = request
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
			approval.Kind = review.Kind
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
			approval.Documents = request.documents
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
	// Without a request open, the header reviews this document at its latest settled version;
	// with one, it answers that request, so it reviews every document the request named, each at
	// the version the request named, and the request is answered once for both.
	reviewed := []model.AskApprovalDocument{{ArtifactID: artifact.ID, Name: artifact.Name, Version: version}}
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
		reviewed = open.Approval.Documents
	}
	review, reviewEvents, err := s.queueApprovalReviews(documentCtx, tx, ledger, artifact, reviewed, state, actor, reason, askID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	appended = append(appended, reviewEvents...)
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(appendedEvents(appended...)...)
	WriteJSON(w, http.StatusCreated, *review)
}

// queueApprovalReviews queues state on every document a review covers - the one being reviewed,
// or every document the approval request it answers named - each at the version named for it,
// and returns the named document's review with one event per document, all filled in once
// Commit has run the write phase (docs.QueueReview). Each is a person's review
// (docs.ReviewKindPerson): a plan review writes its own, on one document.
//
// A second document's row is read here, in the read phase. This transaction does write artifacts:
// an event on a document no issue holds is owned by that document (docs.documentAskEvent), and the
// append moves the row's last_seq (events/broker.go). What makes the read safe is that no write
// this transaction queues changes a column artifactMetadata returns - artifactColumns, the row's
// id, issue_key, project_key, session_id, ref_key, slug, name, kind, is_primary, role, created_by
// and created_at - so the stored row is the one a read after the write phase would return.
func (s *server) queueApprovalReviews(
	ctx context.Context,
	tx pgx.Tx,
	ledger *docs.Ledger,
	artifact model.Artifact,
	documents []model.AskApprovalDocument,
	state string,
	actor model.Actor,
	reason *string,
	askID *string,
) (*model.ArtifactReview, []*model.Event, error) {
	named := new(model.ArtifactReview)
	appended := make([]*model.Event, 0, len(documents))
	for _, document := range documents {
		reviewed, review := artifact, named
		if document.ArtifactID != artifact.ID {
			var err error
			if reviewed, err = artifactMetadata(ctx, tx, document.ArtifactID); err != nil {
				return nil, nil, err
			}
			// Only the named document's review is answered with; another document's is written
			// and left.
			review = new(model.ArtifactReview)
		}
		reviewEvent := new(model.Event)
		if err := ledger.Queue(func(b *store.WriteBatch) error {
			return docs.QueueReview(b, s.deps.Events, reviewed, document.Version, state, docs.ReviewKindPerson,
				actor, reason, askID, func(written model.ArtifactReview, event model.Event) {
					*review, *reviewEvent = written, event
				})
		}); err != nil {
			return nil, nil, err
		}
		appended = append(appended, reviewEvent)
	}
	return named, appended, nil
}

// POST /api/v1/artifacts/{id}/approval-requests  {summary?, include_verification?}  (any
// authenticated actor)
func (s *server) requestArtifactApproval(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Actor   *model.Actor `json:"actor"`
		Summary *string      `json:"summary"`
		// IncludeVerification asks the human to approve the issue's Verification document in the
		// same request, each document at its own current settled version.
		IncludeVerification bool `json:"include_verification"`
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
	pool, err := s.readyPool()
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The coverage is read before the transaction opens, and a read that fails leaves the
	// request recorded all the same (approvalCoverage).
	coverage, coverageUnread, err := s.approvalCoverage(r.Context(), pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
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
	if documentPlays(artifact, model.ArtifactRolePlan) {
		writeError(w, "PLAN_APPROVED_BY_REVIEW", http.StatusConflict,
			"a plan is approved by its plan review, not by a person: record it with dispatch plan-approve (POST /api/v1/artifacts/{id}/plan-reviews)")
		return
	}
	// include_verification asks the human to approve the issue's spec and its Verification
	// document in one card, so it is refused on any other document: on the Verification
	// document itself it would name that document twice, and the one answer would write two
	// reviews and two artifact.approved events on it.
	if input.IncludeVerification && !isSpecDocument(artifact) {
		writeError(w, "NOT_SPEC_DOCUMENT", http.StatusConflict,
			"include_verification asks for the issue's spec and its Verification document together, and this document is not the issue's spec: request approval of it alone")
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
	documents := []model.AskApprovalDocument{
		{ArtifactID: artifact.ID, Name: artifact.Name, Version: version, RequestedVersion: version},
	}
	if input.IncludeVerification {
		verification, err := s.verificationDocumentFor(r.Context(), tx, *artifact.IssueKey)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		documents = append(documents, verification)
	}
	// A supplied summary is validated even when this request opens no row.
	question, err := approvalQuestion(documents, summary)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	type response struct {
		Ask        *model.Ask             `json:"ask"`
		ArtifactID string                 `json:"artifact_id"`
		Version    int                    `json:"version"`
		Approval   model.ArtifactApproval `json:"approval"`
		// Coverage is one line per requirement of the spec with no check, as the approver reads
		// them (coverage.go); never a missing plan task, since the plan comes after the
		// approval, and empty for any document that is not an issue's spec. A gap refuses
		// nothing: the request is recorded as it always was.
		Coverage []string `json:"coverage"`
		// CoverageUnread says the spec's coverage could not be read, so the empty Coverage is
		// no answer about it. The request is recorded either way.
		CoverageUnread bool `json:"coverage_unread,omitempty"`
	}
	if err := s.attachApproval(r.Context(), tx, &artifact); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// An approval of every document this request names, each at its current version, needs no
	// request: return the named document's as it stands.
	settled, err := docs.NamedDocumentsApproved(r.Context(), tx, documents, artifact.ID,
		artifact.Approval != nil && artifact.Approval.State == "approved")
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if settled {
		WriteJSON(w, http.StatusOK, response{Ask: nil, ArtifactID: artifact.ID, Version: version,
			Approval: *artifact.Approval, Coverage: coverage, CoverageUnread: coverageUnread})
		return
	}
	// The open request is found by any document this one names, not by the named document
	// alone: a request already open over the issue's Verification document is renewed to cover
	// the spec too, rather than a second request being opened on a document the first one is
	// still asking about, which would strand one of the two in the human's Inbox.
	named := make([]string, 0, len(documents))
	for _, document := range documents {
		named = append(named, document.ArtifactID)
	}
	open, err := docs.OpenApprovalAskForDocuments(r.Context(), tx, named)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ask := open
	var appended []*model.Event
	var opened *model.Event
	if open != nil {
		if appended, err = s.renewApprovalAsk(documentCtx, tx, ledger, open, actor, documents, summary); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	} else {
		if ask, opened, err = s.openApprovalAsk(documentCtx, tx, ledger, owner, artifact, actor, documents, question); err != nil {
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
	WriteJSON(w, status, response{Ask: ask, ArtifactID: artifact.ID, Version: version,
		Approval: approval, Coverage: coverage, CoverageUnread: coverageUnread})
}

// approvalAfterRequest is the document's approval as this request's writes leave it: the approval
// read before them (attachApprovals) with the request this route opened or handed back written
// over it. Those writes are queued, so the rows hold none of it until Commit and no read of this
// transaction can answer from them; the overlay applies the rule such a read would
// (waitingOnExpression, approvalMoved). A request waits on its agent while a version it has not
// handed back has moved any document it names past the version named for that document, and on
// the human otherwise: a request this route opened or handed back names every document's settled
// version, and the reply its hand-back recorded is the thread's newest one, which the owner lock
// this transaction holds keeps newest until it commits.
func approvalAfterRequest(approval model.ArtifactApproval, ask model.Ask) model.ArtifactApproval {
	requester := ask.Author
	id := ask.ID
	approval.State = "awaiting"
	approval.RequestedBy = &requester
	approval.AskID = &id
	approval.WaitingOn = "human"
	for _, document := range ask.Approval.Documents {
		if document.RequestedVersion < document.Version {
			approval.WaitingOn = "agent"
			break
		}
	}
	return approval
}

// openApprovalAsk queues the insert of the approval request naming documents, asking question,
// the index of its parts and the append of its ask.opened event; the requester follows it. A
// document a human approved before carries that approval's version, time and approver on the
// request (approval.approved), so the human reads what the re-approval follows; a first request
// carries none. The approval named is the first document's, the one the request was opened on.
// The event is filled in once Commit has run the write phase (queueLoggedEvent), and so is the
// ask's OpenedEventID, which its caller takes from the event then.
func (s *server) openApprovalAsk(
	ctx context.Context,
	tx pgx.Tx,
	ledger *docs.Ledger,
	owner owner,
	artifact model.Artifact,
	actor model.Actor,
	documents []model.AskApprovalDocument,
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
	approval := &model.AskApproval{Documents: documents}
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
	// The request's parts - its question and its two review options - beside its own references,
	// both named by the ask.opened below. The parts are planned against the ask this route
	// writes, since the row holding it is queued above, and their statements are queued after
	// that insert, which the rows of a part reference.
	askChanges, err = s.queueAskParts(ctx, ledger, ask, ask.CreatedAt, askChanges)
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
// "" when it named none, which keeps the open request's own; documents are the documents this
// request names, which are added to the open request's, each at its current version. A request
// that names fewer documents than the open one takes nothing away: the open request's documents
// stand until it is answered.
//
// The turn decides, read once before anything is written. A request waiting on its agent - moved
// to a later version, or answered in its thread - goes back to the human: a new summary, or a
// document the open request did not name, first rewords it (docs.RewriteApprovalAsk, ask.edited,
// whose event goes to the ledger, which publishes it), then handBackApprovalAsk hands it back. A
// request already waiting on the human is left as it stands. The same summary over the same
// documents, or none, is a retry and records nothing. Anything else is refused, since an approval
// request carries nothing new: rewording it would rewrite the card the human is reading, with no
// turn of theirs, and refuse an answer they had started (ASK_EDITED).
func (s *server) renewApprovalAsk(
	ctx context.Context,
	tx pgx.Tx,
	ledger *docs.Ledger,
	open *model.Ask,
	actor model.Actor,
	documents []model.AskApprovalDocument,
	summary string,
) ([]*model.Event, error) {
	openSummary, err := docs.ApprovalAskSummary(*open)
	if err != nil {
		return nil, err
	}
	if summary == "" {
		summary = openSummary
	}
	renewed := renewedApprovalDocuments(open.Approval.Documents, documents)
	changed := summary != openSummary || len(renewed) != len(open.Approval.Documents)
	// Every version write moves the open request to its version (docs.MoveApprovalAsk) under the
	// owner lock this request holds, so the turn read here is the one every read reports.
	waitingOn, err := askWaitingOn(ctx, tx, open.ID)
	if err != nil {
		return nil, err
	}
	var appended []*model.Event
	switch {
	case waitingOn == "human" && changed:
		return nil, errorf(http.StatusConflict, "APPROVAL_WAITS_ON_HUMAN",
			"the approval request already waits on the human, asking %q, and a different summary or another document would rewrite the card they are reading, so nothing was changed; raise what changed with the human first, in the request's thread or as a decision block in the document, and once they have agreed to every point in it and a reply or a new version leaves the request waiting on you, hand it back with the new summary",
			open.Question)
	case waitingOn == "agent":
		if changed {
			if err := docs.RewriteApprovalAsk(ctx, tx, s.deps.Events, open, actor, renewed, summary, s.deps.ServerURL); err != nil {
				return nil, err
			}
		}
		handedBack, err := s.handBackApprovalAsk(ledger, open, actor)
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

// renewedApprovalDocuments is the open request's documents, each keeping the requested_version
// the row records, plus any document this request names that the row does not: a request that
// adds the issue's Verification document to one already open grows it, and one that names fewer
// documents leaves it as it stands. The open row already carries each document's current version
// (docs.MoveApprovalAsk moves it on every version write), so nothing re-reads them here.
func renewedApprovalDocuments(open, requested []model.AskApprovalDocument) []model.AskApprovalDocument {
	renewed := slices.Clone(open)
	for _, document := range requested {
		if !slices.ContainsFunc(renewed, func(named model.AskApprovalDocument) bool {
			return named.ArtifactID == document.ArtifactID
		}) {
			renewed = append(renewed, document)
		}
	}
	return renewed
}

// handBackApprovalAsk queues the return of the open approval request to the human at every
// document's current version and the append of its ask.handed_back event, which is filled in once
// Commit has run the write phase (queueEvent). It records those versions as the ones handed back
// and the thread's newest reply as the one this hand-back answered (waitingOnExpression), which
// its own statement reads in that phase. The caller holds the owner lock every reply takes before
// it inserts, so no reply commits between the turn it read and this transaction's commit, and a
// reply that inserts after it gets a later created_at (the insert time, migration 0065), sorts
// after the one recorded and decides the turn, however early its own transaction began. The
// question is left as it stands: a request that changes it rewords first through
// docs.RewriteApprovalAsk, which ask.edited records.
func (s *server) handBackApprovalAsk(ledger *docs.Ledger, ask *model.Ask, actor model.Actor) (*model.Event, error) {
	for index := range ask.Approval.Documents {
		ask.Approval.Documents[index].RequestedVersion = ask.Approval.Documents[index].Version
	}
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

// approvalQuestion is an approval ask's question: every document and version it names, then the
// requester's summary of what the human is approving, when one was given. A summary that would
// take the question past the ask cap is refused naming the characters left for it.
func approvalQuestion(documents []model.AskApprovalDocument, summary string) (string, error) {
	question := docs.ApprovalQuestion(documents, summary)
	if summary == "" {
		return question, nil
	}
	// A version move rebuilds the question at the documents' new versions without asking again
	// (docs.MoveApprovalAsk), so the summary is budgeted against the longest version each document
	// can reach, and no move takes a question accepted here past the cap. It follows that prefix
	// after one space.
	longest := slices.Clone(documents)
	for index := range longest {
		longest[index].Version = maxApprovalVersion
	}
	left := max(0, maxAskQuestion16-len16(docs.ApprovalQuestion(longest, ""))-1)
	if length := len16(summary); length > left {
		return "", capExceededError("summary", length, left)
	}
	return question, nil
}

// artifactMetadata reads one document's row as QueueReview takes it, without its versions or its
// derived approval: the reader of a second document an approval request names.
func artifactMetadata(ctx context.Context, q queryer, id string) (model.Artifact, error) {
	artifact, err := scanArtifact(q.QueryRow(ctx, `select `+artifactColumns+` from artifacts where id = $1`, id))
	if err != nil {
		return model.Artifact{}, fmt.Errorf("read the document an approval request names: %w", err)
	}
	return artifact, nil
}

// verificationDocumentFor is the Verification document of the issue issueKey names, at its
// current settled version, for a request that asks a human to approve it and the issue's spec at
// once. An issue with no Verification document, and one whose Verification document has no
// settled version, each refuse the request, since there is nothing to name beside the spec. The
// one caller is behind the NOT_SPEC_DOCUMENT guard, which already refuses a document that is no
// issue's spec (isSpecDocument), so a document with no issue never reaches here.
func (s *server) verificationDocumentFor(ctx context.Context, q queryer, issueKey string) (model.AskApprovalDocument, error) {
	const noVerification = "the issue has no Verification document to approve beside this one; write verification.md first, or request approval of this document alone"
	verification, held, err := s.verificationDocument(ctx, q, issueKey, "")
	if err != nil {
		return model.AskApprovalDocument{}, fmt.Errorf("read the issue's Verification document: %w", err)
	}
	if !held {
		return model.AskApprovalDocument{}, errorf(http.StatusConflict, "NO_VERIFICATION_DOCUMENT", "%s", noVerification)
	}
	version, err := settledVersionNumber(ctx, q, verification.ID)
	if err != nil {
		return model.AskApprovalDocument{}, err
	}
	if version == 0 {
		return model.AskApprovalDocument{}, errorf(http.StatusConflict, "NO_VERSION",
			"%s has no settled version to approve yet", verification.Name)
	}
	return model.AskApprovalDocument{
		ArtifactID: verification.ID, Name: verification.Name, Version: version, RequestedVersion: version,
	}, nil
}

// POST /api/v1/artifacts/{id}/plan-reviews  {version}  (any authenticated actor)
//
// A plan is approved by the plan review an agent runs over it, never by a person: this records
// that approval of one plan version, as a person's answer records theirs, so every read of the
// document shows it approved and a later version makes it stale. It is refused on any document
// whose role is not plan, the issue's spec included.
func (s *server) createArtifactPlanReview(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Actor   *model.Actor `json:"actor"`
		Version int          `json:"version"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
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
	// The review is this route's one write, queued on the ledger and sent by Commit; everything
	// it is decided on - the document, its owner and its settled versions - is read before it.
	_, ledger := s.deps.Docs.Join(r.Context(), tx)
	defer ledger.Discard()
	artifact, err := s.loadArtifact(r.Context(), tx, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !documentPlays(artifact, model.ArtifactRolePlan) {
		writeError(w, "NOT_PLAN_DOCUMENT", http.StatusConflict,
			"only an issue's plan document (plan.md) is approved by a plan review")
		return
	}
	if err := s.requireOpenOwner(r.Context(), tx, ownerForArtifact(artifact)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	latest, err := settledVersionNumber(r.Context(), tx, artifact.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if latest == 0 {
		writeError(w, "NO_VERSION", http.StatusConflict, "the document has no settled version to review yet")
		return
	}
	if input.Version < 1 || input.Version > latest {
		writeError(w, "INVALID_REVIEW", http.StatusBadRequest,
			fmt.Sprintf("version must name a settled version of the plan, between 1 and %d", latest))
		return
	}
	var review model.ArtifactReview
	reviewEvent := new(model.Event)
	if err := ledger.Queue(func(b *store.WriteBatch) error {
		return docs.QueueReview(b, s.deps.Events, artifact, input.Version, "approved", docs.ReviewKindPlanReview,
			actor, nil, nil, func(written model.ArtifactReview, event model.Event) {
				review, *reviewEvent = written, event
			})
	}); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := ledger.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(*reviewEvent)
	WriteJSON(w, http.StatusCreated, review)
}
