package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

const (
	maxAskQuestion16 = 800
	maxAskOptions    = 8
)

func (s *server) createAsk(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Question string             `json:"question"`
		Options  []model.AskOption  `json:"options"`
		Multiple *bool              `json:"multiple"`
		Custom   *bool              `json:"custom"`
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
	if length := text.Len16(input.Question); length > maxAskQuestion16 {
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
	custom := true
	if input.Custom != nil {
		custom = *input.Custom
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
	defer tx.Rollback(r.Context())
	issueKey := r.PathValue("key")
	if err := s.requireOpenIssue(r.Context(), tx, issueKey); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	anchor, artifactName, snapshot, err := s.resolveAnchor(r.Context(), tx, issueKey, input.Anchor, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.Options == nil {
		input.Options = []model.AskOption{}
	}

	options, err := encodeJSON(input.Options)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	author, err := jsonActor(actor)
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
		insert into asks (issue_key, author, question, options, multiple, custom, urgency, anchor)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
		returning id::text, created_at
	`, issueKey, author, input.Question, options, multiple, custom, urgency, anchorJSON).Scan(&ask.ID, &ask.CreatedAt); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ask.IssueKey = issueKey
	ask.Author = actor
	ask.Question = input.Question
	ask.Options = input.Options
	ask.Multiple = multiple
	ask.Custom = custom
	ask.Urgency = urgency
	ask.Anchor = anchor
	ask.State = "open"
	if err := s.replaceRefs(r.Context(), tx, "ask", ask.ID, ask.Question); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events := []model.Event{}
	if snapshot != nil {
		snapshotEvent, err := s.appendEvent(r.Context(), tx, model.Event{
			IssueKey: issueKey,
			Type:     "artifact.version",
			Actor:    actor,
			Payload:  map[string]any{"artifact_id": anchor.ArtifactID, "name": artifactName, "version": *snapshot},
		})
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		events = append(events, snapshotEvent)
	}
	event, err := s.appendEvent(r.Context(), tx, model.Event{IssueKey: issueKey, Type: "ask.opened", Actor: actor, Payload: ask})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events = append(events, event)
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if snapshot != nil {
		s.deps.Docs.CommitVersion(anchor.ArtifactID, *snapshot)
	}
	s.publish(events...)
	writeJSON(w, http.StatusCreated, ask)
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

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	ask, err := s.loadAskForUpdate(r.Context(), tx, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if ask.State != "open" {
		writeError(w, "ASK_CLOSED", http.StatusConflict, "ask is already answered")
		return
	}
	if err := s.requireOpenIssue(r.Context(), tx, ask.IssueKey); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !ask.Multiple && len(input.Selected) > 1 {
		writeError(w, "INVALID_ANSWER", http.StatusBadRequest, "single-select asks accept at most one selected answer")
		return
	}
	if !ask.Custom && !selectedOptions(ask.Options, input.Selected) {
		writeError(w, "INVALID_ANSWER", http.StatusBadRequest, "selected answers must be ask option labels")
		return
	}
	answer := model.AskAnswer{User: actor.ID, Selected: input.Selected, Text: input.Text, At: time.Now().UTC()}
	answerJSON, err := encodeJSON(answer)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `update asks set state = 'answered', answer = $2 where id = $1`, ask.ID, answerJSON); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	ask.State = "answered"
	ask.Answer = &answer
	event, err := s.appendEvent(r.Context(), tx, model.Event{IssueKey: ask.IssueKey, Type: "ask.answered", Actor: actor, Payload: ask})
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

func (s *server) getAsk(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	ask, err := s.loadAsk(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ask)
}

type askQueryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *server) loadAsk(ctx context.Context, q askQueryer, id string) (model.Ask, error) {
	return s.loadAskRow(ctx, q, `
		select id::text, issue_key, author, question, options, multiple, custom, urgency, anchor, state, answer, created_at
		from asks where id = $1
	`, id)
}

func (s *server) loadAskForUpdate(ctx context.Context, tx pgx.Tx, id string) (model.Ask, error) {
	return s.loadAskRow(ctx, tx, `
		select id::text, issue_key, author, question, options, multiple, custom, urgency, anchor, state, answer, created_at
		from asks where id = $1 for update
	`, id)
}

func (s *server) loadAskRow(ctx context.Context, q askQueryer, query, id string) (model.Ask, error) {
	return scanAsk(q.QueryRow(ctx, query, id))
}

type askRow interface {
	Scan(dest ...any) error
}

func scanAsk(row askRow) (model.Ask, error) {
	var ask model.Ask
	var author, options, anchor, answer []byte
	if err := row.Scan(
		&ask.ID, &ask.IssueKey, &author, &ask.Question, &options, &ask.Multiple, &ask.Custom, &ask.Urgency, &anchor, &ask.State, &answer, &ask.CreatedAt,
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
