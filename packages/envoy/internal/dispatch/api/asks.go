package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

const (
	maxAskQuestion16 = 800
	maxAskOptions    = 8
)

func (s *server) createAsk(w http.ResponseWriter, r *http.Request) {
	s.createAskFor(w, r, issueOwner(r.PathValue("key")))
}

func (s *server) createAskFor(w http.ResponseWriter, r *http.Request, owner owner) {
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
	if strings.TrimSpace(input.Question) == "" {
		writeError(w, "INVALID_ASK", http.StatusBadRequest, "ask question is required")
		return
	}
	if length := len16(input.Question); length > maxAskQuestion16 {
		capExceeded(w, "question", length, maxAskQuestion16)
		return
	}
	if len(input.Options) > maxAskOptions {
		capExceeded(w, "options", len(input.Options), maxAskOptions)
		return
	}
	multiple := false
	if input.Multiple != nil {
		multiple = *input.Multiple
	}
	if input.Options == nil {
		input.Options = []model.AskOption{}
	}
	seenOptions := make(map[string]struct{}, len(input.Options))
	for index := range input.Options {
		label := strings.TrimSpace(input.Options[index].Label)
		if label == "" {
			writeError(w, "INVALID_ASK", http.StatusBadRequest, "ask option labels are required")
			return
		}
		if _, duplicate := seenOptions[label]; duplicate {
			writeError(w, "INVALID_ASK", http.StatusBadRequest, "ask option labels must be unique")
			return
		}
		seenOptions[label] = struct{}{}
		input.Options[index].Label = label
	}
	urgency := strings.TrimSpace(input.Urgency)
	if urgency == "" {
		urgency = "med"
	}
	if !validUrgency(urgency) {
		writeError(w, "INVALID_ASK", http.StatusBadRequest, "ask urgency must be low, med, high, or blocking")
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
		insert into asks (id, issue_key, artifact_id, author, question, options, multiple, urgency, anchor)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
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
	if err := s.replaceRefs(r.Context(), tx, "ask", ask.ID, ask.Question); err != nil {
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

type askTransition struct {
	EventType string
	Apply     func(context.Context, pgx.Tx, model.Ask) (model.Ask, error)
}

func (s *server) answerAsk(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Selected []string     `json:"selected"`
		Text     *string      `json:"text"`
		Actor    *model.Actor `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	ask, err := s.closeAsk(r.Context(), r.PathValue("id"), actor, askTransition{
		EventType: "ask.answered",
		Apply: func(ctx context.Context, tx pgx.Tx, ask model.Ask) (model.Ask, error) {
			hasText := input.Text != nil && strings.TrimSpace(*input.Text) != ""
			if !ask.Multiple && len(input.Selected) > 1 {
				return model.Ask{}, errorf(http.StatusBadRequest, "INVALID_ANSWER", "single-select asks accept at most one selected answer")
			}
			if len(input.Selected) > 0 && !selectedOptions(ask.Options, input.Selected) {
				return model.Ask{}, errorf(http.StatusBadRequest, "INVALID_ANSWER", "selected answers must be ask option labels")
			}
			if len(input.Selected) == 0 && !hasText {
				return model.Ask{}, errorf(http.StatusBadRequest, "INVALID_ANSWER", "answer requires a selected option or free-text answer")
			}
			answer := model.AskAnswer{User: actor.ID, Selected: input.Selected, Text: input.Text, At: time.Now().UTC()}
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
	})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ask)
}

func (s *server) resolveAsk(w http.ResponseWriter, r *http.Request) {
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
	ask, err = transition.Apply(ctx, tx, ask)
	if err != nil {
		return model.Ask{}, err
	}
	event, err := s.appendEvent(ctx, tx, ownerOf(ask.IssueKey, ask.ArtifactID).event(transition.EventType, actor, ask))
	if err != nil {
		return model.Ask{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Ask{}, err
	}
	s.publish(event)
	return ask, nil
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
	writeJSON(w, http.StatusOK, struct {
		Ask     model.Ask       `json:"ask"`
		Replies []model.Comment `json:"replies"`
	}{Ask: ask, Replies: replies})
}

func (s *server) loadAsk(ctx context.Context, q queryer, id string) (model.Ask, error) {
	return scanAsk(q.QueryRow(ctx, `
		select id::text, issue_key, artifact_id::text, author, question, options, multiple, urgency, anchor, state, answer, resolution, created_at
		from asks where id = $1
	`, id))
}

// listIssueAsksColumns are the columns every ask-listing query selects, in scan order.
const listIssueAsksColumns = `id::text, issue_key, artifact_id::text, author, question, options, multiple, urgency, anchor, state, answer, resolution, created_at`

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
	return asks, rows.Err()
}

func (s *server) loadAskForUpdate(ctx context.Context, tx pgx.Tx, id string) (model.Ask, error) {
	return scanAsk(tx.QueryRow(ctx, `
		select id::text, issue_key, artifact_id::text, author, question, options, multiple, urgency, anchor, state, answer, resolution, created_at
		from asks where id = $1 for update
	`, id))
}

func scanAsk(row pgx.Row) (model.Ask, error) {
	var ask model.Ask
	var author, options, anchor, answer, resolution []byte
	if err := row.Scan(
		&ask.ID, &ask.IssueKey, &ask.ArtifactID, &author, &ask.Question, &options, &ask.Multiple, &ask.Urgency,
		&anchor, &ask.State, &answer, &resolution, &ask.CreatedAt,
	); err != nil {
		return model.Ask{}, err
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
