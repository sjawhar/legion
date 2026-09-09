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
	batchSize       = 100
	retryInterval   = 5 * time.Second
	compactInterval = 24 * time.Hour
	compactKeep     = 500
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

// Run drains unpublished notifying events at startup, after committed local
// events, and periodically so a dropped in-process notification cannot strand
// an event. It returns when ctx is cancelled.
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
		select e.id, e.issue_key, e.seq, e.type, e.actor, e.notify, e.created_at, e.payload, i.route
		from events e
		join issues i on i.key = e.issue_key
		where e.notify and e.published_at is null
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
		var route *string
		if err := rows.Scan(
			&event.ID,
			&event.IssueKey,
			&event.Seq,
			&event.Type,
			&actor,
			&event.Notify,
			&event.CreatedAt,
			&payload,
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
		if err := publish(deps, event, route); err != nil {
			slog.Error("dispatch outbox: publish issue event", "event_id", event.ID, "error", err)
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

func publish(deps Deps, event model.Event, route *string) error {
	item, err := envelope(event)
	if err != nil {
		return err
	}
	if err := item.Validate(); err != nil {
		return fmt.Errorf("validate envelope: %w", err)
	}
	if err := deps.Publisher.Publish(item); err != nil {
		return err
	}
	publishRoute(deps.Publisher, item, route)
	return nil
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

func envelope(event model.Event) (contracts.Envelope, error) {
	payload, err := json.Marshal(event)
	if err != nil {
		return contracts.Envelope{}, fmt.Errorf("encode event: %w", err)
	}
	eventID := fmt.Sprintf("dispatch-%d", event.ID)
	item := contracts.Envelope{
		EventID:        eventID,
		Source:         "dispatch",
		SourceEventID:  fmt.Sprint(event.ID),
		Topic:          "notifications.dispatch.issue." + event.IssueKey + "." + event.Type,
		DedupeKey:      eventID,
		IssuedAt:       event.CreatedAt.UnixMilli(),
		PayloadSummary: payloadSummary(event),
		Payload:        string(payload),
		TraceID:        eventID,
	}
	if event.Actor.Kind == "session" {
		item.SourceSession = event.Actor.ID
	}
	if event.Type == "ask.answered" {
		item.InReplyTo = payloadString(event.Payload, "id")
	}
	if strings.HasPrefix(event.Type, "ask.") {
		item.Urgency = payloadString(event.Payload, "urgency")
	}
	return item, nil
}

func payloadSummary(event model.Event) string {
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
	summary := event.IssueKey + " " + kind
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
