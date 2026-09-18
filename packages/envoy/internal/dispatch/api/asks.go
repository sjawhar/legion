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
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

const (
	maxAskQuestion16 = 800
	maxAskOptions    = 8
)

const (
	askHintLimit         = 8
	askQuestionHintLimit = 120
)

func (s *server) askIDInputForOwner(ctx context.Context, q queryer, owner owner) error {
	asks, _, err := s.queryOwnerAsks(ctx, q, owner, "open")
	if err != nil {
		return err
	}
	scope := "this document's"
	if owner.IssueKey != nil {
		scope = "this issue's"
	}
	hints := make([]string, 0, min(len(asks), askHintLimit))
	for index, ask := range asks {
		if index == askHintLimit {
			break
		}
		hints = append(hints, ask.ID[:8]+"… "+askQuestionHint(ask.Question))
	}
	if len(hints) == 0 {
		hints = append(hints, "none")
	}
	return errorf(http.StatusBadRequest, "ASK_ID_INPUT", "ask IDs are UUIDs; use the full ask ID; %s open asks: %s", scope, strings.Join(hints, ", "))
}

func askQuestionHint(question string) string {
	oneLine := strings.Join(strings.Fields(question), " ")
	runes := []rune(oneLine)
	if len(runes) <= askQuestionHintLimit {
		return oneLine
	}
	return string(runes[:askQuestionHintLimit]) + "…"
}

func validateAskQuestion(question string) error {
	if strings.TrimSpace(question) == "" {
		return errorf(http.StatusBadRequest, "INVALID_ASK", "ask question is required")
	}
	if length := len16(question); length > maxAskQuestion16 {
		return capExceededError("question", length, maxAskQuestion16)
	}
	return nil
}

func validateAskOptions(options []model.AskOption) error {
	if len(options) > maxAskOptions {
		return countExceededError("CAP_EXCEEDED", "options", len(options), maxAskOptions)
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

func normalizeAskKind(value string) (string, error) {
	switch kind := strings.TrimSpace(value); kind {
	case "", "question":
		return "question", nil
	case "approval":
		return "", errorf(http.StatusBadRequest, "ASK_KIND_INPUT", "approval asks are server-created only")
	case "action":
		return "", errorf(http.StatusBadRequest, "ASK_KIND_INPUT", "the action ask kind was removed; open a question with the options you want, such as Done / Can't")
	default:
		return "", errorf(http.StatusBadRequest, "ASK_KIND_INPUT", "ask kind must be question")
	}
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
		Kind     string             `json:"kind"`
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
	kind, err := normalizeAskKind(input.Kind)
	if err != nil {
		s.writeHandlerError(w, err)
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
	documentCtx, documentEvents := documentMutationContext(r.Context(), tx)
	if err := s.requireOpenOwner(r.Context(), tx, owner); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var rowID string
	if err := tx.QueryRow(r.Context(), `select gen_random_uuid()::text`).Scan(&rowID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	anchor, artifactName, snapshot, err := s.resolveAnchor(documentCtx, tx, owner, input.Anchor, docs.MarkAsk, rowID, actor)
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
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		returning created_at
	`, rowID, owner.IssueKey, owner.ArtifactID, author, input.Question, options, multiple, urgency, anchorJSON, kind).Scan(&ask.CreatedAt); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := asks.FollowAuthor(r.Context(), tx, rowID, actor); err != nil {
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
	ask.Kind = kind
	if anchor != nil {
		ask.AnchorArtifact, err = s.loadAskAnchorArtifact(r.Context(), tx, anchor.ArtifactID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
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
		if err := refs.Stamp(r.Context(), tx, "artifact", anchor.ArtifactID, snapshotEvent.ID); err != nil {
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
	if err := refs.Stamp(r.Context(), tx, "ask", ask.ID, event.ID); err != nil {
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
	s.publishDocumentEvents(documentEvents, events...)
	WriteJSON(w, http.StatusCreated, ask)
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
	if !requireUUIDPath(w, r, "ask") {
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
	if err := refs.Stamp(r.Context(), tx, "ask", ask.ID, event.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusOK, ask)
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

type openAskOwner struct {
	Issue    *inboxIssue    `json:"issue,omitempty"`
	Document *inboxDocument `json:"document,omitempty"`
}

type openAsk struct {
	ID           string              `json:"id"`
	Ref          string              `json:"ref"`
	Question     string              `json:"question"`
	Kind         string              `json:"kind"`
	Urgency      string              `json:"urgency"`
	CreatedAt    time.Time           `json:"created_at"`
	AgeSeconds   int64               `json:"age_seconds"`
	Priority     *int                `json:"priority"`
	Owner        openAskOwner        `json:"owner"`
	HumanReplied bool                `json:"human_replied"`
	LastReply    *model.AskLastReply `json:"last_reply"`
	WaitingOn    string              `json:"waiting_on"`
}

type openAsksResponse struct {
	SessionID      string    `json:"session_id"`
	AsOf           string    `json:"as_of"`
	OpenedSince    bool      `json:"opened_since"`
	Count          int       `json:"count"`
	WaitingOnHuman int       `json:"waiting_on_human"`
	WaitingOnAgent int       `json:"waiting_on_agent"`
	Asks           []openAsk `json:"asks"`
}

// listOpenAsks returns every active open ask in one scope, oldest first: authored by one host
// session (?author_session=) across issues and unlinked project documents, or every open ask on
// one project's issues and documents (?project=) regardless of author -- issue-owned via
// issues.project_key, document-owned via artifacts.project_key. Exactly one of the two query
// parameters selects the scope. A human clarification leaves an ask open but makes the author
// responsible for the next reply.
func (s *server) listOpenAsks(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	sessionID := strings.TrimSpace(r.URL.Query().Get("author_session"))
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	if sessionID == "" && project == "" {
		writeError(w, "AUTHOR_SESSION_OR_PROJECT_REQUIRED", http.StatusBadRequest, "exactly one of author_session or project is required")
		return
	}
	if sessionID != "" && project != "" {
		writeError(w, "AUTHOR_SESSION_OR_PROJECT_CONFLICT", http.StatusBadRequest, "author_session and project are mutually exclusive; supply exactly one")
		return
	}
	var since *time.Time
	if values, ok := r.URL.Query()["since"]; ok {
		if len(values) != 1 {
			writeError(w, "INVALID_SINCE", http.StatusBadRequest, "since must be one RFC3339 timestamp")
			return
		}
		value, err := time.Parse(time.RFC3339, values[0])
		if err != nil {
			writeError(w, "INVALID_SINCE", http.StatusBadRequest, "since must be an RFC3339 timestamp")
			return
		}
		since = &value
	}

	var asOf time.Time
	var openedSince bool
	var rawAsks []byte
	err := s.deps.Store.Pool.QueryRow(r.Context(), `
		with mine as (
			select a.* from asks a
			left join issues i on i.key = a.issue_key
			left join artifacts ar on ar.id = a.artifact_id
			where
				($1 <> '' and a.author->>'kind' = 'session' and a.author->>'id' = $1)
				or ($3 <> '' and coalesce(i.project_key, ar.project_key) = $3)
		), active as (
			select
				a.id::text,
				case
					when i.key is not null then '/issues/' || i.key || '?ask=' || a.id::text
					else '/projects/' || ar.project_key || '/documents/' || ar.slug || '?ask=' || a.id::text
				end as ref,
				a.question,
				a.kind,
				a.urgency,
				a.created_at,
				greatest(0, floor(extract(epoch from (statement_timestamp() - a.created_at))))::bigint as age_seconds,
				i.priority,
				case
					when i.key is not null then jsonb_build_object('issue', jsonb_build_object('key', i.key, 'title', i.title))
					else jsonb_build_object('document', jsonb_build_object('project', ar.project_key, 'slug', ar.slug, 'name', ar.name))
				end as owner,
				exists (
					select 1 from comments c
					where c.ask_id = a.id and c.author->>'kind' = 'user'
				) as human_replied,
				case when lr.created_at is null then null else jsonb_build_object(
					'author', lr.author,
					'created_at', lr.created_at
				) end as last_reply,
				coalesce(lr.turn, 'human') as waiting_on
			from mine a
			left join issues i on i.key = a.issue_key
			left join artifacts ar on ar.id = a.artifact_id`+lastReplyJoin+`
			where a.state = 'open' and (i.key is null or i.closed_at is null)
		)
		select
			statement_timestamp(),
			exists (select 1 from mine where $2::timestamptz is not null and created_at >= $2),
			coalesce((select jsonb_agg(to_jsonb(active) order by priority asc nulls last, created_at asc, id) from active), '[]'::jsonb)
	`, sessionID, since, project).Scan(&asOf, &openedSince, &rawAsks)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

	asks := []openAsk{}
	if err := json.Unmarshal(rawAsks, &asks); err != nil {
		s.writeHandlerError(w, fmt.Errorf("decode open asks: %w", err))
		return
	}
	response := openAsksResponse{
		SessionID:   sessionID,
		AsOf:        timestampValue(asOf),
		OpenedSince: openedSince,
		Asks:        asks,
	}
	for _, ask := range asks {
		switch ask.WaitingOn {
		case "human":
			response.WaitingOnHuman++
		case "agent":
			response.WaitingOnAgent++
		default:
			s.writeHandlerError(w, fmt.Errorf("open ask %q has invalid waiting_on %q", ask.ID, ask.WaitingOn))
			return
		}
	}
	response.Count = len(response.Asks)
	WriteJSON(w, http.StatusOK, response)
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
	asks, _, err := s.queryOwnerAsks(r.Context(), s.deps.Store.Pool, issueOwner(r.PathValue("key")), state)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, asks)
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
	if !s.requireAuthenticated(w, r) || !requireUUIDPath(w, r, "ask") {
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
	followers, err := asks.Followers(r.Context(), s.deps.Store.Pool, ask.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, struct {
		Ask       model.Ask           `json:"ask"`
		Replies   []model.Comment     `json:"replies"`
		Edits     []model.AskEdit     `json:"edits"`
		Followers []model.AskFollower `json:"followers"`
	}{Ask: ask, Replies: replies, Edits: edits, Followers: followers})
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

// loadAsk reads one ask for a response: unlike loadAskForUpdate, which feeds the
// ask.* event payloads, it carries WaitingOn for an open ask.
func (s *server) loadAsk(ctx context.Context, q queryer, id string) (model.Ask, error) {
	ask, _, err := scanAskRead(q.QueryRow(ctx, `
		select `+askReadColumns+`
		`+askReadFrom+`
		where a.id = $1
	`, id))
	if err != nil {
		return model.Ask{}, err
	}
	if err := s.attachOpenedEventIDs(ctx, q, []*model.Ask{&ask}); err != nil {
		return model.Ask{}, err
	}
	return ask, nil
}

// pgx caches prepared plans by query text. State and ownership each have a fixed
// query so the partial open-ask index remains eligible under generic plans.
const (
	listIssueAsksQueryAll = `select ` + askReadColumns + ` ` + askReadFrom + `
		where a.issue_key = $1 order by a.created_at, a.id`
	listIssueAsksQueryOpen = `select ` + askReadColumns + ` ` + askReadFrom + `
		where a.issue_key = $1 and a.state = 'open' order by a.created_at, a.id`
	listIssueAsksQueryAnswered = `select ` + askReadColumns + ` ` + askReadFrom + `
		where a.issue_key = $1 and a.state = 'answered' order by a.created_at, a.id`
	listArtifactAsksQueryAll = `select ` + askReadColumns + ` ` + askReadFrom + `
		where a.artifact_id = $1 order by a.created_at, a.id`
	listArtifactAsksQueryOpen = `select ` + askReadColumns + ` ` + askReadFrom + `
		where a.artifact_id = $1 and a.state = 'open' order by a.created_at, a.id`
	listArtifactAsksQueryAnswered = `select ` + askReadColumns + ` ` + askReadFrom + `
		where a.artifact_id = $1 and a.state = 'answered' order by a.created_at, a.id`
)

// queryOwnerAsks returns an owner's asks, oldest first, filtered by state ("all",
// "open", or "answered"), each with its opening event id, its block document, and
// WaitingOn when open. The newest reply in each ask's thread is returned keyed by
// ask id; an ask nobody has replied to has no entry.
func (s *server) queryOwnerAsks(ctx context.Context, q queryer, owner owner, state string) ([]model.Ask, map[string]model.AskLastReply, error) {
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
		return nil, nil, errorf(http.StatusBadRequest, "OWNER_INVALID", "owner requires exactly one issue or artifact")
	}
	rows, err := q.Query(ctx, query, value)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	asks := []model.Ask{}
	replies := map[string]model.AskLastReply{}
	for rows.Next() {
		ask, reply, err := scanAskRead(rows)
		if err != nil {
			return nil, nil, err
		}
		if reply != nil {
			replies[ask.ID] = *reply
		}
		asks = append(asks, ask)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	askPointers := make([]*model.Ask, len(asks))
	for index := range asks {
		askPointers[index] = &asks[index]
	}
	if err := s.attachOpenedEventIDs(ctx, q, askPointers); err != nil {
		return nil, nil, err
	}
	return asks, replies, nil
}

func (s *server) loadAskForUpdate(ctx context.Context, tx pgx.Tx, id string) (model.Ask, error) {
	ask, err := scanAskRow(tx.QueryRow(ctx, `
		select `+askRowColumns+`
		`+askRowFrom+`
		where a.id = $1 for update of a
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

	openedEventIDs, err := events.OpenedEventIDs(ctx, q, askIDs)
	if err != nil {
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

func validUrgency(value string) bool {
	switch value {
	case "low", "med", "high", "blocking":
		return true
	default:
		return false
	}
}
