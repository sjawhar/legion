// Package outbox publishes durable Dispatch events to NATS.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	batchSize           = 100
	retryInterval       = 5 * time.Second
	compactInterval     = 24 * time.Hour
	compactKeep         = 500
	documentTopicPrefix = "notifications.dispatch.document."
)

// Publisher is the NATS publication boundary used by the Dispatch outbox.
type Publisher interface {
	Publish(contracts.Envelope) error
}

// Deps are the durable state and delivery dependencies for the outbox.
type Deps struct {
	Store     *store.Store
	Publisher Publisher
	Broker    *events.Broker
	Docs      docs.API
}

// Run drains unpublished events at startup, after committed local events, and
// periodically so a dropped in-process notification cannot strand an event. It
// returns when ctx is cancelled.
func Run(ctx context.Context, deps Deps) {
	events, cancel := deps.Broker.Subscribe()
	defer cancel()

	scan(ctx, deps)
	retry := time.NewTicker(retryInterval)
	defer retry.Stop()
	compact := time.NewTicker(compactInterval)
	defer compact.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-events:
			scan(ctx, deps)
		case <-retry.C:
			scan(ctx, deps)
		case <-compact.C:
			if err := deps.Docs.CompactAll(ctx, compactKeep); err != nil {
				slog.Error("dispatch outbox: compact documents", "error", err)
			}
		}
	}
}

func scan(ctx context.Context, deps Deps) {
	for {
		count, retryLater, err := scanBatch(ctx, deps)
		if err != nil {
			slog.Error("dispatch outbox: scan", "error", err)
			return
		}
		if count < batchSize || retryLater {
			return
		}
	}
}

func scanBatch(ctx context.Context, deps Deps) (int, bool, error) {
	rows, err := deps.Store.Pool.Query(ctx, `
		select e.id, e.issue_key, e.artifact_id::text, coalesce(i.project_key, ar.project_key), e.seq,
		       e.type, e.actor, e.notify, e.created_at, e.payload, coalesce(ar.slug, ''), coalesce(i.route, ai.route)
		from events e
		left join issues i on i.key = e.issue_key
		left join artifacts ar on ar.id = e.artifact_id
		left join issues ai on ai.key = ar.issue_key
		where e.published_at is null
		order by e.id
		limit $1
	`, batchSize)
	if err != nil {
		return 0, false, fmt.Errorf("select unpublished events: %w", err)
	}
	defer rows.Close()

	count := 0
	retryLater := false
	for rows.Next() {
		count++
		var event model.Event
		var actor, payload []byte
		var slug string
		var route *string
		if err := rows.Scan(
			&event.ID,
			&event.IssueKey,
			&event.ArtifactID,
			&event.Project,
			&event.Seq,
			&event.Type,
			&actor,
			&event.Notify,
			&event.CreatedAt,
			&payload,
			&slug,
			&route,
		); err != nil {
			slog.Error("dispatch outbox: read event", "error", err)
			retryLater = true
			continue
		}
		if err := json.Unmarshal(actor, &event.Actor); err != nil {
			slog.Error("dispatch outbox: decode event actor", "event_id", event.ID, "error", err)
			retryLater = true
			continue
		}
		if err := json.Unmarshal(payload, &event.Payload); err != nil {
			slog.Error("dispatch outbox: decode event payload", "event_id", event.ID, "error", err)
			retryLater = true
			continue
		}
		if err := publish(ctx, deps, event, slug, route); err != nil {
			slog.Error("dispatch outbox: publish event", "event_id", event.ID, "error", err)
			retryLater = true
			continue
		}
		if _, err := deps.Store.Pool.Exec(ctx, `
			update events set published_at = now() where id = $1 and published_at is null
		`, event.ID); err != nil {
			slog.Error("dispatch outbox: mark event published", "event_id", event.ID, "error", err)
			retryLater = true
		}
	}
	if err := rows.Err(); err != nil {
		return count, retryLater, fmt.Errorf("iterate unpublished events: %w", err)
	}
	return count, retryLater, nil
}

func publish(ctx context.Context, deps Deps, event model.Event, slug string, route *string) error {
	item, err := envelope(event, slug)
	if err != nil {
		return err
	}
	if err := item.Validate(); err != nil {
		return fmt.Errorf("validate envelope: %w", err)
	}
	if err := deps.Publisher.Publish(item); err != nil {
		return err
	}
	if event.Notify {
		publishRoute(deps.Publisher, item, route)
		publishAuthorRoutes(ctx, deps, item, event)
	}
	return nil
}

// publishAuthorRoutes delivers a human comment-thread action or ask resolution directly to
// the involved sessions' own topics, regardless of the issue's route (which may point at an
// entirely different reviewer) and regardless of whether the event's owner is an issue or a
// project document (which has no route at all). Without this, the agent that asked the
// question, started the thread, or is the thread's own root author never learns about the
// human's reply, resolution, reopening, or edit.
func publishAuthorRoutes(ctx context.Context, deps Deps, item contracts.Envelope, event model.Event) {
	var askID, replyTo, commentID string
	switch event.Type {
	case "comment.created", "comment.resolved", "comment.reopened", "comment.edited":
		askID = payloadString(event.Payload, "ask_id")
		replyTo = payloadString(event.Payload, "reply_to")
		commentID = payloadString(event.Payload, "id")
	case "ask.resolved":
		askID = payloadString(event.Payload, "id")
	default:
		return
	}

	seen := map[string]bool{}
	var targets []model.Actor
	consider := func(author model.Actor, ok bool) {
		if !ok || author.Kind != "session" || seen[author.ID] {
			return
		}
		if event.Actor.Kind == author.Kind && event.Actor.ID == author.ID {
			return
		}
		seen[author.ID] = true
		targets = append(targets, author)
	}

	if askID != "" {
		consider(loadAskAuthor(ctx, deps, askID))
	}
	if replyTo != "" {
		// The root is what humans reply under; the parent (reply_to's own target) is
		// usually the same comment today since a reply must target a thread root, but
		// walking to the true root keeps this correct if nesting is ever allowed.
		consider(loadRootCommentAuthor(ctx, deps, replyTo))
		consider(loadCommentAuthor(ctx, deps, replyTo))
	} else if commentID != "" && (event.Type == "comment.resolved" || event.Type == "comment.reopened") {
		consider(loadRootCommentAuthor(ctx, deps, commentID))
	}

	for _, author := range targets {
		routed := item
		routed.Topic = contracts.AgentTopicPrefix + author.ID
		if err := deps.Publisher.Publish(routed); err != nil {
			slog.Error("dispatch outbox: publish author route", "session_id", author.ID, "error", err)
		}
	}
}

func loadAskAuthor(ctx context.Context, deps Deps, askID string) (model.Actor, bool) {
	var authorJSON []byte
	if err := deps.Store.Pool.QueryRow(ctx, `select author from asks where id = $1`, askID).Scan(&authorJSON); err != nil {
		slog.Error("dispatch outbox: load ask author", "ask_id", askID, "error", err)
		return model.Actor{}, false
	}
	return decodeAuthor(authorJSON, "ask", askID)
}

func loadCommentAuthor(ctx context.Context, deps Deps, commentID string) (model.Actor, bool) {
	var authorJSON []byte
	if err := deps.Store.Pool.QueryRow(ctx, `select author from comments where id = $1`, commentID).Scan(&authorJSON); err != nil {
		slog.Error("dispatch outbox: load comment author", "comment_id", commentID, "error", err)
		return model.Actor{}, false
	}
	return decodeAuthor(authorJSON, "comment", commentID)
}

// loadRootCommentAuthor walks a comment's reply_to chain up to its thread root - the comment
// humans reply under - and returns that root's author.
func loadRootCommentAuthor(ctx context.Context, deps Deps, commentID string) (model.Actor, bool) {
	var authorJSON []byte
	if err := deps.Store.Pool.QueryRow(ctx, `
		with recursive chain as (
			select id, reply_to, author from comments where id = $1
			union all
			select c.id, c.reply_to, c.author from comments c join chain h on c.id = h.reply_to
		)
		select author from chain where reply_to is null limit 1
	`, commentID).Scan(&authorJSON); err != nil {
		slog.Error("dispatch outbox: load root comment author", "comment_id", commentID, "error", err)
		return model.Actor{}, false
	}
	return decodeAuthor(authorJSON, "comment", commentID)
}

func decodeAuthor(raw []byte, kind, id string) (model.Actor, bool) {
	var author model.Actor
	if err := json.Unmarshal(raw, &author); err != nil {
		slog.Error("dispatch outbox: decode author", "kind", kind, "id", id, "error", err)
		return model.Actor{}, false
	}
	return author, true
}

func publishRoute(publisher Publisher, item contracts.Envelope, route *string) {
	if route == nil || *route == "" {
		return
	}
	parsed, err := model.ParseRoute(*route)
	if err != nil {
		slog.Error("dispatch outbox: parse route", "event_id", item.EventID, "route", *route, "error", err)
		return
	}
	switch parsed.Kind {
	case "role":
		item.Topic = contracts.RoleTopicPrefix + parsed.ID
	case "session":
		item.Topic = contracts.AgentTopicPrefix + parsed.ID
	default:
		slog.Error("dispatch outbox: unsupported route", "event_id", item.EventID, "route", *route)
		return
	}
	if err := publisher.Publish(item); err != nil {
		slog.Error("dispatch outbox: publish route", "event_id", item.EventID, "route", *route, "error", err)
	}
}

func envelope(event model.Event, slug string) (contracts.Envelope, error) {
	payload, err := json.Marshal(event)
	if err != nil {
		return contracts.Envelope{}, fmt.Errorf("encode event: %w", err)
	}
	var topic string
	if event.ArtifactID != nil {
		if event.Project == "" || slug == "" {
			return contracts.Envelope{}, fmt.Errorf("document event requires project and slug")
		}
		topic = documentTopicPrefix + event.Project + "." + slug + "." + event.Type
	} else if event.IssueKey != nil {
		topic = "notifications.dispatch.issue." + *event.IssueKey + "." + event.Type
	} else {
		return contracts.Envelope{}, fmt.Errorf("event requires exactly one owner")
	}
	eventID := fmt.Sprintf("dispatch-%d", event.ID)
	item := contracts.Envelope{
		EventID:        eventID,
		Source:         "dispatch",
		SourceEventID:  fmt.Sprint(event.ID),
		Topic:          topic,
		DedupeKey:      eventID,
		IssuedAt:       event.CreatedAt.UnixMilli(),
		PayloadSummary: payloadSummary(event, slug),
		Payload:        string(payload),
		TraceID:        eventID,
	}
	if event.Actor.Kind == "session" {
		item.SourceSession = event.Actor.ID
	}
	if event.Type == "ask.answered" || event.Type == "ask.resolved" {
		item.InReplyTo = payloadString(event.Payload, "id")
	}
	if event.Type == "comment.created" {
		// A comment replying directly to an ask (Comment.AskID) is a reply to the
		// asking session's question: correlate it the same way ask.answered
		// correlates to the ask, so the agent's TOON renders "re: <ask id>".
		if askID := payloadString(event.Payload, "ask_id"); askID != "" {
			item.InReplyTo = askID
		}
	}
	if strings.HasPrefix(event.Type, "ask.") {
		item.Urgency = payloadString(event.Payload, "urgency")
	}
	return item, nil
}

func payloadSummary(event model.Event, slug string) string {
	kind := strings.ReplaceAll(event.Type, ".", " ")
	text := ""
	switch {
	case strings.HasPrefix(event.Type, "ask."):
		text = truncate(payloadString(event.Payload, "question"), 120)
	case event.Type == "message.created":
		text = payloadString(event.Payload, "body")
	case strings.HasPrefix(event.Type, "comment."), strings.HasPrefix(event.Type, "suggestion."):
		text = payloadString(event.Payload, "body")
	}
	owner := ""
	if event.ArtifactID != nil {
		owner = event.Project + "/" + slug
	} else if event.IssueKey != nil {
		owner = *event.IssueKey
	}
	summary := owner + " " + kind
	if text != "" {
		summary += ": " + text
	}
	return truncate(strings.Join(strings.Fields(summary), " "), 160)
}

func payloadString(payload any, key string) string {
	values, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	value, _ := values[key].(string)
	return value
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
