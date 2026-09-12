package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

const (
	maxAskQuestion16 = 800
	maxAskOptions    = 8
)

func validateAskQuestion(question string) error {
	if strings.TrimSpace(question) == "" {
		return errorf(http.StatusBadRequest, "INVALID_ASK", "ask question is required")
	}
	if length := len16(question); length > maxAskQuestion16 {
		return errorf(http.StatusBadRequest, "CAP_EXCEEDED", "question length %d exceeds limit %d", length, maxAskQuestion16)
	}
	return nil
}

func validateAskOptions(options []model.AskOption) error {
	if len(options) > maxAskOptions {
		return errorf(http.StatusBadRequest, "CAP_EXCEEDED", "options length %d exceeds limit %d", len(options), maxAskOptions)
	}
	seen := make(map[string]struct{}, len(options))
	for index := range options {
		label := strings.TrimSpace(options[index].Label)
		if label == "" {
			return errorf(http.StatusBadRequest, "INVALID_ASK", "ask option labels are required")
		}
		if _, duplicate := seen[label]; duplicate {
			return errorf(http.StatusBadRequest, "INVALID_ASK", "ask option labels must be unique")
		}
		seen[label] = struct{}{}
		options[index].Label = label
	}
	return nil
}

func normalizeAskUrgency(value string) (string, error) {
	urgency := strings.TrimSpace(value)
	if urgency == "" {
		urgency = "med"
	}
	if !validUrgency(urgency) {
		return "", errorf(http.StatusBadRequest, "INVALID_ASK", "ask urgency must be low, med, high, or blocking")
	}
	return urgency, nil
}

func (s *server) createAsk(w http.ResponseWriter, r *http.Request) {
	s.createAskFor(w, r, issueOwner(r.PathValue("key")))
}

func (s *server) createAskFor(w http.ResponseWriter, r *http.Request, owner owner) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input struct {
		Question string             `json:"question"`
		Options  []model.AskOption  `json:"options"`
		Multiple *bool              `json:"multiple"`
		Urgency  string             `json:"urgency"`
		Anchor   *model.AnchorInput `json:"anchor"`
		Actor    *model.Actor       `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	if err := validateAskQuestion(input.Question); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.Options == nil {
		input.Options = []model.AskOption{}
	}
	if err := validateAskOptions(input.Options); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	multiple := false
	if input.Multiple != nil {
		multiple = *input.Multiple
	}
	urgency, err := normalizeAskUrgency(input.Urgency)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	evictOnFailure := false
	evictArtifactID := ""
	defer func() {
		if evictOnFailure {
			_ = s.deps.Docs.Evict(r.Context(), evictArtifactID)
		}
	}()
	defer tx.Rollback(r.Context())
	if err := s.requireOpenOwner(r.Context(), tx, owner); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var rowID string
	if err := tx.QueryRow(r.Context(), `select gen_random_uuid()::text`).Scan(&rowID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	anchor, artifactName, snapshot, err := s.resolveAnchor(r.Context(), tx, owner, input.Anchor, docs.MarkAsk, rowID, actor)
	if anchor != nil {
		evictOnFailure = true
		evictArtifactID = anchor.ArtifactID
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	options, err := encodeJSON(input.Options)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	author, err := encodeJSON(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var anchorJSON any
	if anchor != nil {
		anchorJSON, err = encodeJSON(anchor)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	var ask model.Ask
	if err := tx.QueryRow(r.Context(), `
		insert into asks (id, issue_key, artifact_id, author, question, options, multiple, urgency, anchor, kind)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'question')
		returning created_at
	`, rowID, owner.IssueKey, owner.ArtifactID, author, input.Question, options, multiple, urgency, anchorJSON).Scan(&ask.CreatedAt); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ask.ID = rowID
	ask.IssueKey = owner.IssueKey
	ask.ArtifactID = owner.ArtifactID
	ask.Author = actor
	ask.Question = input.Question
	ask.Options = input.Options
	ask.Multiple = multiple
	ask.Urgency = urgency
	ask.Anchor = anchor
	ask.State = "open"
	ask.Kind = "question"
	if err := refs.Replace(r.Context(), tx, "ask", ask.ID, ask.Question, s.deps.ServerURL); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events := []model.Event{}
	if snapshot != nil {
		snapshotEvent, err := s.appendEvent(r.Context(), tx, owner.event(
			"artifact.version",
			actor,
			versionEventPayload(anchor.ArtifactID, artifactName, *snapshot, nil),
		))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		events = append(events, snapshotEvent)
	}
	event, err := s.appendEvent(r.Context(), tx, owner.event("ask.opened", actor, ask))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ask.OpenedEventID = &event.ID
	event.Payload = ask
	events = append(events, event)
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	evictOnFailure = false
	if snapshot != nil {
		s.deps.Docs.CommitVersion(anchor.ArtifactID, *snapshot)
	}
	s.publish(events...)
	writeJSON(w, http.StatusCreated, ask)
}

func (s *server) editAsk(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input struct {
		Question *string            `json:"question"`
		Options  *[]model.AskOption `json:"options"`
		Multiple *bool              `json:"multiple"`
		Urgency  *string            `json:"urgency"`
		Actor    *model.Actor       `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.Question == nil && input.Options == nil && input.Multiple == nil && input.Urgency == nil {
		writeError(w, "INVALID_ASK", http.StatusBadRequest, "ask edit requires at least one field")
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	if _, err := uuid.Parse(r.PathValue("id")); err != nil {
		writeError(w, "ASK_ID_INPUT", http.StatusBadRequest, "ask id must be a UUID")
		return
	}
	if input.Question != nil {
		if err := validateAskQuestion(*input.Question); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if input.Options != nil {
		if err := validateAskOptions(*input.Options); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	var requestedUrgency string
	if input.Urgency != nil {
		var err error
		requestedUrgency, err = normalizeAskUrgency(*input.Urgency)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	unlockedAsk, err := s.loadAsk(r.Context(), tx, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := s.requireOpenOwner(r.Context(), tx, ownerOf(unlockedAsk.IssueKey, unlockedAsk.ArtifactID)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ask, err := s.loadAskForUpdate(r.Context(), tx, unlockedAsk.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if ask.State != "open" {
		writeError(w, "ASK_NOT_OPEN", http.StatusConflict, "only open asks may be edited")
		return
	}
	if ask.Kind == "approval" {
		writeError(w, "ASK_KIND_FIXED", http.StatusConflict, "an approval ask's question and options are fixed; retract it and request approval again")
		return
	}
	if actor.Kind == "session" && (ask.Author.Kind != actor.Kind || ask.Author.ID != actor.ID) {
		writeError(w, "NOT_AUTHOR", http.StatusForbidden, "only the asking session may edit an ask")
		return
	}

	previous := model.AskEditPrevious{
		Question: ask.Question,
		Options:  ask.Options,
		Multiple: ask.Multiple,
		Urgency:  ask.Urgency,
	}
	if input.Question != nil {
		ask.Question = *input.Question
	}
	if input.Options != nil {
		ask.Options = *input.Options
	}
	if input.Multiple != nil {
		ask.Multiple = *input.Multiple
	}
	if input.Urgency != nil {
		ask.Urgency = requestedUrgency
	}
	options, err := encodeJSON(ask.Options)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var editedAt time.Time
	if err := tx.QueryRow(r.Context(), `
		update asks
		set question = $2, options = $3, multiple = $4, urgency = $5, edited_at = now()
		where id = $1
		returning edited_at
	`, ask.ID, ask.Question, options, ask.Multiple, ask.Urgency).Scan(&editedAt); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ask.EditedAt = timestampPtr(&editedAt)
	if err := refs.Replace(r.Context(), tx, "ask", ask.ID, ask.Question, s.deps.ServerURL); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, ownerOf(ask.IssueKey, ask.ArtifactID).event(
		"ask.edited",
		actor,
		model.AskEditEventPayload{Ask: ask, Previous: previous, EditedBy: actor},
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	writeJSON(w, http.StatusOK, ask)
}

func timestampPtr(value *time.Time) *string {
	if value == nil {
		return nil
	}
	text := timestampValue(*value)
	return &text
}

func timestampValue(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func askTimestamp(value time.Time) *string {
	return timestampPtr(&value)
}

func askTimestampPtr(value *time.Time) *string {
	return timestampPtr(value)
}

type askTransition struct {
	EventType string
	Apply     func(context.Context, pgx.Tx, model.Ask) (model.Ask, error)
	// After runs once the transition's own event is appended, still inside the
	// transaction; the events it returns are published with it.
	After func(context.Context, pgx.Tx, model.Ask) ([]model.Event, error)
}

// answerTransition is the answer of an ask: option labels for a question, or
// exactly one of Approve / Request changes for an approval ask.
func answerTransition(actor model.Actor, selected []string, text *string) askTransition {
	return askTransition{
		EventType: "ask.answered",
		Apply: func(ctx context.Context, tx pgx.Tx, ask model.Ask) (model.Ask, error) {
			hasText := text != nil && strings.TrimSpace(*text) != ""
			if ask.Kind == "approval" {
				if _, _, err := reviewFromAnswer(selected, text); err != nil {
					return model.Ask{}, err
				}
			} else {
				if !ask.Multiple && len(selected) > 1 {
					return model.Ask{}, errorf(http.StatusBadRequest, "INVALID_ANSWER", "single-select asks accept at most one selected answer")
				}
				if len(selected) > 0 && !selectedOptions(ask.Options, selected) {
					return model.Ask{}, errorf(http.StatusBadRequest, "INVALID_ANSWER", "selected answers must be ask option labels")
				}
				if len(selected) == 0 && !hasText {
					return model.Ask{}, errorf(http.StatusBadRequest, "INVALID_ANSWER", "answer requires a selected option or free-text answer")
				}
			}
			answer := model.AskAnswer{User: actor.ID, Selected: selected, Text: text, At: time.Now().UTC()}
			answerJSON, err := encodeJSON(answer)
			if err != nil {
				return model.Ask{}, err
			}
			if _, err := tx.Exec(ctx, `update asks set state = 'answered', answer = $2 where id = $1`, ask.ID, answerJSON); err != nil {
				return model.Ask{}, err
			}
			ask.State = "answered"
			ask.Answer = &answer
			return ask, nil
		},
	}
}

// answerAskTx answers an ask inside the caller's transaction without appending
// or publishing its event; the header review path uses it to close an open
// approval ask alongside the review it writes.
func (s *server) answerAskTx(ctx context.Context, tx pgx.Tx, id string, actor model.Actor, selected []string, text *string) (model.Ask, error) {
	return s.transitionAskTx(ctx, tx, id, answerTransition(actor, selected, text))
}

func (s *server) answerAsk(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	var input struct {
		Selected []string     `json:"selected"`
		Text     *string      `json:"text"`
		Actor    *model.Actor `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	transition := answerTransition(actor, input.Selected, input.Text)
	// An approval ask's answer is a review of the document it names, pinned to
	// the document's latest settled version at answer time.
	transition.After = func(ctx context.Context, tx pgx.Tx, ask model.Ask) ([]model.Event, error) {
		if ask.Kind != "approval" || ask.Approval == nil {
			return nil, nil
		}
		state, reason, err := reviewFromAnswer(input.Selected, input.Text)
		if err != nil {
			return nil, err
		}
		artifact, err := s.loadArtifact(ctx, tx, ask.Approval.ArtifactID)
		if err != nil {
			return nil, err
		}
		version, err := latestVersionNumber(ctx, tx, artifact.ID)
		if err != nil {
			return nil, err
		}
		askID := ask.ID
		_, event, err := s.writeReview(ctx, tx, artifact, version, state, actor, reason, &askID)
		if err != nil {
			return nil, err
		}
		return []model.Event{event}, nil
	}
	ask, err := s.closeAsk(r.Context(), r.PathValue("id"), actor, transition)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ask)
}

func (s *server) resolveAsk(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input struct {
		Kind   string       `json:"kind"`
		Reason string       `json:"reason"`
		Actor  *model.Actor `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	kind := strings.TrimSpace(input.Kind)
	reason := strings.TrimSpace(input.Reason)
	if (kind != "retracted" && kind != "resolved") || reason == "" {
		writeError(w, "INVALID_RESOLUTION", http.StatusBadRequest, "resolution requires a kind and reason")
		return
	}
	ask, err := s.closeAsk(r.Context(), r.PathValue("id"), actor, askTransition{
		EventType: "ask.resolved",
		Apply: func(ctx context.Context, tx pgx.Tx, ask model.Ask) (model.Ask, error) {
			resolution := model.AskResolution{Kind: kind, Reason: reason, Actor: actor, At: time.Now().UTC()}
			resolutionJSON, err := encodeJSON(resolution)
			if err != nil {
				return model.Ask{}, err
			}
			if _, err := tx.Exec(ctx, `update asks set state = 'resolved', resolution = $2 where id = $1`, ask.ID, resolutionJSON); err != nil {
				return model.Ask{}, err
			}
			ask.State = "resolved"
			ask.Resolution = &resolution
			return ask, nil
		},
	})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ask)
}

func (s *server) closeAsk(ctx context.Context, id string, actor model.Actor, transition askTransition) (model.Ask, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Ask{}, err
	}
	defer tx.Rollback(ctx)
	ask, err := s.transitionAskTx(ctx, tx, id, transition)
	if err != nil {
		return model.Ask{}, err
	}
	event, err := s.appendEvent(ctx, tx, ownerOf(ask.IssueKey, ask.ArtifactID).event(transition.EventType, actor, ask))
	if err != nil {
		return model.Ask{}, err
	}
	events := []model.Event{event}
	if transition.After != nil {
		more, err := transition.After(ctx, tx, ask)
		if err != nil {
			return model.Ask{}, err
		}
		events = append(events, more...)
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Ask{}, err
	}
	s.publish(events...)
	return ask, nil
}

// transitionAskTx locks an open ask and applies a transition inside the caller's
// transaction; it appends no event.
func (s *server) transitionAskTx(ctx context.Context, tx pgx.Tx, id string, transition askTransition) (model.Ask, error) {
	unlockedAsk, err := s.loadAsk(ctx, tx, id)
	if err != nil {
		return model.Ask{}, err
	}
	if err := s.requireOpenOwner(ctx, tx, ownerOf(unlockedAsk.IssueKey, unlockedAsk.ArtifactID)); err != nil {
		return model.Ask{}, err
	}
	ask, err := s.loadAskForUpdate(ctx, tx, id)
	if err != nil {
		return model.Ask{}, err
	}
	switch ask.State {
	case "open":
	case "answered":
		if transition.EventType == "ask.answered" {
			return model.Ask{}, errorf(http.StatusConflict, "ASK_CLOSED", "ask is already answered")
		}
		return model.Ask{}, errorf(http.StatusConflict, "ASK_ANSWERED", "ask is already answered")
	case "resolved":
		return model.Ask{}, errorf(http.StatusConflict, "ASK_RESOLVED", "ask is already resolved")
	default:
		return model.Ask{}, errorf(http.StatusInternalServerError, "ASK_STATE_INVALID", "ask has an invalid state")
	}
	return transition.Apply(ctx, tx, ask)
}

// listIssueAsks returns every ask on an issue, filtered by state: "open" or
// "answered" match only that state; "all" (the default) returns every ask
// regardless of state, including resolved ones.
func (s *server) listIssueAsks(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	state, err := parseAskListState(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	asks, err := s.loadOwnerAsks(r.Context(), s.deps.Store.Pool, issueOwner(r.PathValue("key")), state)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, asks)
}

func parseAskListState(r *http.Request) (string, error) {
	state := r.URL.Query().Get("state")
	if state == "" {
		return "all", nil
	}
	if state != "all" && state != "open" && state != "answered" {
		return "", errorf(http.StatusBadRequest, "INVALID_ASK_STATE", "state must be all, open, or answered")
	}
	return state, nil
}

func (s *server) getAsk(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	ask, err := s.loadAsk(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	replies, err := s.loadReplyChain(r.Context(), s.deps.Store.Pool, "ask_id", ask.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	edits, err := s.loadAskEdits(r.Context(), s.deps.Store.Pool, ask.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Ask     model.Ask       `json:"ask"`
		Replies []model.Comment `json:"replies"`
		Edits   []model.AskEdit `json:"edits"`
	}{Ask: ask, Replies: replies, Edits: edits})
}

// loadAskEdits reads every rewording of an ask back from its ask.edited events,
// oldest first, so a reader can see each version the question went through.
func (s *server) loadAskEdits(ctx context.Context, q queryer, askID string) ([]model.AskEdit, error) {
	rows, err := q.Query(ctx, `
		select payload->'previous', payload->'edited_by', created_at
		from events
		where type = 'ask.edited' and payload->>'id' = $1
		order by id asc
	`, askID)
	if err != nil {
		return nil, fmt.Errorf("load ask edits: %w", err)
	}
	defer rows.Close()
	edits := []model.AskEdit{}
	for rows.Next() {
		var previous, editedBy []byte
		var at time.Time
		if err := rows.Scan(&previous, &editedBy, &at); err != nil {
			return nil, fmt.Errorf("scan ask edit: %w", err)
		}
		var edit model.AskEdit
		if err := json.Unmarshal(previous, &edit.Previous); err != nil {
			return nil, fmt.Errorf("decode ask edit previous: %w", err)
		}
		if edit.Previous.Options == nil {
			edit.Previous.Options = []model.AskOption{}
		}
		if err := json.Unmarshal(editedBy, &edit.EditedBy); err != nil {
			return nil, fmt.Errorf("decode ask edit editor: %w", err)
		}
		edit.At = timestampValue(at)
		edits = append(edits, edit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load ask edits: %w", err)
	}
	return edits, nil
}

func (s *server) loadAsk(ctx context.Context, q queryer, id string) (model.Ask, error) {
	ask, err := scanAsk(q.QueryRow(ctx, `
		select id::text, issue_key, artifact_id::text, author, question, options, multiple, urgency, anchor, state, answer, resolution, created_at, edited_at, kind, approval
		from asks where id = $1
	`, id))
	if err != nil {
		return model.Ask{}, err
	}
	if err := s.attachOpenedEventIDs(ctx, q, []*model.Ask{&ask}); err != nil {
		return model.Ask{}, err
	}
	return ask, nil
}

// listIssueAsksColumns are the columns every ask-listing query selects, in scan order.
const listIssueAsksColumns = `id::text, issue_key, artifact_id::text, author, question, options, multiple, urgency, anchor, state, answer, resolution, created_at, edited_at, kind, approval`

// pgx caches prepared plans by query text. State and ownership each have a fixed
// query so the partial open-ask index remains eligible under generic plans.
const (
	listIssueAsksQueryAll = `select ` + listIssueAsksColumns + `
		from asks where issue_key = $1 order by created_at, id`
	listIssueAsksQueryOpen = `select ` + listIssueAsksColumns + `
		from asks where issue_key = $1 and state = 'open' order by created_at, id`
	listIssueAsksQueryAnswered = `select ` + listIssueAsksColumns + `
		from asks where issue_key = $1 and state = 'answered' order by created_at, id`
	listArtifactAsksQueryAll = `select ` + listIssueAsksColumns + `
		from asks where artifact_id = $1 order by created_at, id`
	listArtifactAsksQueryOpen = `select ` + listIssueAsksColumns + `
		from asks where artifact_id = $1 and state = 'open' order by created_at, id`
	listArtifactAsksQueryAnswered = `select ` + listIssueAsksColumns + `
		from asks where artifact_id = $1 and state = 'answered' order by created_at, id`
)

// loadIssueAsks returns an issue's asks, oldest first, filtered by state ("all",
// "open", or "answered").
func (s *server) loadIssueAsks(ctx context.Context, q queryer, key, state string) ([]model.Ask, error) {
	return s.loadOwnerAsks(ctx, q, issueOwner(key), state)
}

func (s *server) loadOwnerAsks(ctx context.Context, q queryer, owner owner, state string) ([]model.Ask, error) {
	var query, value string
	switch {
	case owner.IssueKey != nil:
		value = *owner.IssueKey
		switch state {
		case "open":
			query = listIssueAsksQueryOpen
		case "answered":
			query = listIssueAsksQueryAnswered
		default:
			query = listIssueAsksQueryAll
		}
	case owner.ArtifactID != nil:
		value = *owner.ArtifactID
		switch state {
		case "open":
			query = listArtifactAsksQueryOpen
		case "answered":
			query = listArtifactAsksQueryAnswered
		default:
			query = listArtifactAsksQueryAll
		}
	default:
		return nil, errorf(http.StatusBadRequest, "OWNER_INVALID", "owner requires exactly one issue or artifact")
	}
	rows, err := q.Query(ctx, query, value)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	asks := []model.Ask{}
	for rows.Next() {
		ask, err := scanAsk(rows)
		if err != nil {
			return nil, err
		}
		asks = append(asks, ask)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	askPointers := make([]*model.Ask, len(asks))
	for index := range asks {
		askPointers[index] = &asks[index]
	}
	if err := s.attachOpenedEventIDs(ctx, q, askPointers); err != nil {
		return nil, err
	}
	return asks, nil
}

func (s *server) loadAskForUpdate(ctx context.Context, tx pgx.Tx, id string) (model.Ask, error) {
	ask, err := scanAsk(tx.QueryRow(ctx, `
		select id::text, issue_key, artifact_id::text, author, question, options, multiple, urgency, anchor, state, answer, resolution, created_at, edited_at, kind, approval
		from asks where id = $1 for update
	`, id))
	if err != nil {
		return model.Ask{}, err
	}
	if err := s.attachOpenedEventIDs(ctx, tx, []*model.Ask{&ask}); err != nil {
		return model.Ask{}, err
	}
	return ask, nil
}

func (s *server) attachOpenedEventIDs(
	ctx context.Context,
	q queryer,
	asks []*model.Ask,
) error {
	askIDs := make([]string, 0, len(asks))
	seen := make(map[string]struct{}, len(asks))
	for _, ask := range asks {
		if _, exists := seen[ask.ID]; !exists {
			seen[ask.ID] = struct{}{}
			askIDs = append(askIDs, ask.ID)
		}
	}
	if len(askIDs) == 0 {
		return nil
	}

	rows, err := q.Query(ctx, `
		select payload->>'id', min(id)
		from events
		where type in ('ask.opened', 'ask.answered', 'ask.resolved', 'ask.edited')
		  and payload->>'id' = any($1)
		group by payload->>'id'
	`, askIDs)
	if err != nil {
		return err
	}
	defer rows.Close()
	openedEventIDs := make(map[string]int64, len(askIDs))
	for rows.Next() {
		var askID string
		var eventID int64
		if err := rows.Scan(&askID, &eventID); err != nil {
			return err
		}
		openedEventIDs[askID] = eventID
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, ask := range asks {
		openedEventID, ok := openedEventIDs[ask.ID]
		if !ok {
			return fmt.Errorf("ask %q has no event", ask.ID)
		}
		ask.OpenedEventID = new(int64)
		*ask.OpenedEventID = openedEventID
	}
	return nil
}

func scanAsk(row pgx.Row) (model.Ask, error) {
	var ask model.Ask
	var author, options, anchor, answer, resolution, approval []byte
	var editedAt *time.Time
	if err := row.Scan(
		&ask.ID, &ask.IssueKey, &ask.ArtifactID, &author, &ask.Question, &options, &ask.Multiple, &ask.Urgency,
		&anchor, &ask.State, &answer, &resolution, &ask.CreatedAt, &editedAt, &ask.Kind, &approval,
	); err != nil {
		return model.Ask{}, err
	}
	if len(approval) > 0 {
		var value model.AskApproval
		if err := json.Unmarshal(approval, &value); err != nil {
			return model.Ask{}, fmt.Errorf("decode ask approval: %w", err)
		}
		ask.Approval = &value
	}
	if err := json.Unmarshal(author, &ask.Author); err != nil {
		return model.Ask{}, fmt.Errorf("decode ask author: %w", err)
	}
	if err := json.Unmarshal(options, &ask.Options); err != nil {
		return model.Ask{}, fmt.Errorf("decode ask options: %w", err)
	}
	if ask.Options == nil {
		ask.Options = []model.AskOption{}
	}

	if len(anchor) > 0 {
		var value model.Anchor
		if err := json.Unmarshal(anchor, &value); err != nil {
			return model.Ask{}, fmt.Errorf("decode ask anchor: %w", err)
		}
		ask.Anchor = &value
	}
	if len(answer) > 0 {
		var value model.AskAnswer
		if err := json.Unmarshal(answer, &value); err != nil {
			return model.Ask{}, fmt.Errorf("decode ask answer: %w", err)
		}
		ask.Answer = &value
	}
	if len(resolution) > 0 {
		var value model.AskResolution
		if err := json.Unmarshal(resolution, &value); err != nil {
			return model.Ask{}, fmt.Errorf("decode ask resolution: %w", err)
		}
		ask.Resolution = &value
	}
	ask.EditedAt = timestampPtr(editedAt)
	return ask, nil
}

func selectedOptions(options []model.AskOption, selected []string) bool {
	labels := make(map[string]struct{}, len(options))
	for _, option := range options {
		labels[option.Label] = struct{}{}
	}
	seen := make(map[string]struct{}, len(selected))
	for _, value := range selected {
		if _, exists := labels[value]; !exists {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validUrgency(value string) bool {
	switch value {
	case "low", "med", "high", "blocking":
		return true
	default:
		return false
	}
}
