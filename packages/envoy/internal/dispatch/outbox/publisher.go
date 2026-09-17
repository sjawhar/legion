// Package outbox publishes durable Dispatch events to NATS.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

const (
	batchSize           = 100
	retryInterval       = 5 * time.Second
	retryBaseDelay      = time.Second
	retryMaxDelay       = 5 * time.Minute
	deadLetterAttempts  = 10
	compactInterval     = 24 * time.Hour
	compactKeep         = 500
	documentTopicPrefix = "notifications.dispatch.document."
	// maxCommentThreadDepth bounds the reply_to walk in loadRootCommentAuthor so a
	// malformed cycle (comments.reply_to has no acyclicity constraint) cannot spin the
	// recursive query and stall the outbox scan.
	maxCommentThreadDepth = 32
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
	events, unsubscribe := deps.Broker.Subscribe()
	defer func() { unsubscribe() }()

	scan(ctx, deps)
	retry := time.NewTicker(retryInterval)
	defer retry.Stop()
	compact := time.NewTicker(compactInterval)
	defer compact.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-events:
			if !open {
				unsubscribe()
				events, unsubscribe = deps.Broker.Subscribe()
				continue
			}
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
		count, blocked, err := scanBatch(ctx, deps)
		if err != nil {
			slog.Error("dispatch outbox: scan", "error", err)
			return
		}
		if count < batchSize || blocked {
			return
		}
	}
}

func scanBatch(ctx context.Context, deps Deps) (int, bool, error) {
	rows, err := deps.Store.Pool.Query(ctx, `
		select e.id, e.issue_key, e.artifact_id::text, coalesce(e.project_key, i.project_key, ar.project_key, ''), e.seq,
		       e.type, e.actor, e.notify, e.created_at, e.payload, e.attempt_count, e.published_destinations,
		       coalesce(ar.slug, ''), coalesce(i.route, ai.route)
		from events e
		left join issues i on i.key = e.issue_key
		left join artifacts ar on ar.id = e.artifact_id
		left join issues ai on ai.key = ar.issue_key
		where e.published_at is null
		  and (e.next_attempt_at is null or e.next_attempt_at <= now())
		  and coalesce(e.payload ->> 'pending', 'false') <> 'true'
		order by e.id
		limit $1
	`, batchSize)
	if err != nil {
		return 0, false, fmt.Errorf("select unpublished events: %w", err)
	}
	defer rows.Close()

	count := 0
	blocked := false
	for rows.Next() {
		count++
		var event model.Event
		var actor, payload []byte
		var slug string
		var route *string
		var attempts int
		var destinations []string
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
			&attempts,
			&destinations,
			&slug,
			&route,
		); err != nil {
			slog.Error("dispatch outbox: read event", "error", err)
			blocked = true
			continue
		}
		if err := json.Unmarshal(actor, &event.Actor); err != nil {
			slog.Error("dispatch outbox: decode event actor", "event_id", event.ID, "error", err)
			if err := scheduleRetry(ctx, deps, event.ID, attempts); err != nil {
				slog.Error("dispatch outbox: schedule retry", "event_id", event.ID, "error", err)
				blocked = true
			}
			continue
		}
		if err := json.Unmarshal(payload, &event.Payload); err != nil {
			slog.Error("dispatch outbox: decode event payload", "event_id", event.ID, "error", err)
			if err := scheduleRetry(ctx, deps, event.ID, attempts); err != nil {
				slog.Error("dispatch outbox: schedule retry", "event_id", event.ID, "error", err)
				blocked = true
			}
			continue
		}
		if err := publish(ctx, deps, event, slug, route, publishedDestinationSet(destinations)); err != nil {
			slog.Error("dispatch outbox: publish event", "event_id", event.ID, "error", err)
			if err := scheduleRetry(ctx, deps, event.ID, attempts); err != nil {
				slog.Error("dispatch outbox: schedule retry", "event_id", event.ID, "error", err)
				blocked = true
			}
			continue
		}
		if _, err := deps.Store.Pool.Exec(ctx, `
			update events set published_at = now(), next_attempt_at = null where id = $1 and published_at is null
		`, event.ID); err != nil {
			slog.Error("dispatch outbox: mark event published", "event_id", event.ID, "error", err)
			if err := scheduleRetry(ctx, deps, event.ID, attempts); err != nil {
				slog.Error("dispatch outbox: schedule retry", "event_id", event.ID, "error", err)
			}
			blocked = true
		}
	}
	if err := rows.Err(); err != nil {
		return count, blocked, fmt.Errorf("iterate unpublished events: %w", err)
	}
	return count, blocked, nil
}

func scheduleRetry(ctx context.Context, deps Deps, eventID int64, attempts int) error {
	attempts++
	if attempts == deadLetterAttempts {
		slog.Error("dispatch outbox: event reached dead-letter threshold", "event_id", eventID, "attempts", attempts)
	}
	_, err := deps.Store.Pool.Exec(ctx, `
		update events
		set attempt_count = $2, next_attempt_at = $3
		where id = $1 and published_at is null
	`, eventID, attempts, time.Now().Add(retryDelay(attempts)))
	if err != nil {
		return fmt.Errorf("schedule event retry: %w", err)
	}
	return nil
}

func retryDelay(attempts int) time.Duration {
	delay := retryBaseDelay
	for attempt := 1; attempt < attempts && delay < retryMaxDelay; attempt++ {
		delay *= 2
	}
	if delay > retryMaxDelay {
		return retryMaxDelay
	}
	return delay
}

func publish(ctx context.Context, deps Deps, event model.Event, slug string, route *string, delivered map[string]struct{}) error {
	if event.IssueKey == nil && event.ArtifactID == nil && event.Project == "" {
		return nil
	}
	item, err := envelope(event, slug)
	if err != nil {
		return err
	}
	if err := item.Validate(); err != nil {
		return fmt.Errorf("validate envelope: %w", err)
	}
	if err := publishDestination(ctx, deps, event.ID, item, delivered); err != nil {
		return err
	}
	targeted := (event.Type == "message.created" || event.Type == "message.answered") &&
		payloadString(event.Payload, "target") != ""
	if event.Notify && !targeted {
		if err := publishRoute(ctx, deps, event.ID, item, delivered, route); err != nil {
			return err
		}
		if err := publishAuthorRoutes(ctx, deps, event.ID, item, event, delivered); err != nil {
			return err
		}
	}
	return publishFollowerRoutes(ctx, deps, event.ID, item, event, delivered)
}

// publishFollowerRoutes delivers an ask's answer, edit, resolution, and every reply on it
// to each session following the ask (the asker, every session that replied, and any
// session a human added), on the session's own topic. Unlike the route and author routes
// this ignores Notify: an agent's reply on an ask must still reach the other followers,
// who otherwise learn of it only by subscribing to the whole issue. The event's own actor
// is skipped, and a topic the route already reached is not published twice.
func publishFollowerRoutes(ctx context.Context, deps Deps, eventID int64, item contracts.Envelope, event model.Event, delivered map[string]struct{}) error {
	var askID string
	switch event.Type {
	case "ask.answered", "ask.edited", "ask.resolved":
		askID = payloadString(event.Payload, "id")
	case "comment.created", "comment.resolved", "comment.reopened", "comment.edited":
		askID = payloadString(event.Payload, "ask_id")
	}
	if askID == "" {
		return nil
	}
	followers, err := asks.Followers(ctx, deps.Store.Pool, askID)
	if err != nil {
		return err
	}
	for _, follower := range followers {
		if event.Actor.Kind == "session" && event.Actor.ID == follower.SessionID {
			continue
		}
		routed := item
		routed.Topic = contracts.AgentTopicPrefix + follower.SessionID
		if err := publishDestination(ctx, deps, eventID, routed, delivered); err != nil {
			return fmt.Errorf("publish follower route to %q: %w", follower.SessionID, err)
		}
	}
	return nil
}

// publishAuthorRoutes delivers a human comment-thread action directly to the involved
// sessions' own topics, regardless of the issue's route (which may point at an entirely
// different reviewer) and regardless of whether the event's owner is an issue or a project
// document (which has no route at all). Without this, the agent that started the thread
// or is the thread's own root author never learns about the human's reply, resolution,
// reopening, or edit. Ask threads are not walked here: their asker and repliers are the
// ask's followers (publishFollowerRoutes).
func publishAuthorRoutes(ctx context.Context, deps Deps, eventID int64, item contracts.Envelope, event model.Event, delivered map[string]struct{}) error {
	seen := map[string]bool{}
	var targets []model.Actor
	consider := func(author model.Actor, found bool) {
		if !found || author.Kind != "session" || seen[author.ID] {
			return
		}
		if event.Actor.Kind == author.Kind && event.Actor.ID == author.ID {
			return
		}
		seen[author.ID] = true
		targets = append(targets, author)
	}
	considerLoaded := func(author model.Actor, found bool, err error) error {
		if err != nil {
			return err
		}
		consider(author, found)
		return nil
	}

	var inReplyTo, commentID, messageInReplyTo string
	switch event.Type {
	case "comment.created", "comment.resolved", "comment.reopened", "comment.edited":
		inReplyTo = payloadString(event.Payload, "reply_to")
		commentID = payloadString(event.Payload, "id")
	case "message.created", "message.answered":
		messageInReplyTo = payloadString(event.Payload, "in_reply_to")
	case "subscription.removed", "ask.follower_added", "ask.follower_removed":
		// The target is the unsubscribed, added, or removed session itself, carried directly
		// in the payload — there is no thread or ask to walk to find it.
		if sessionID := payloadString(event.Payload, "session_id"); sessionID != "" {
			consider(model.Actor{Kind: "session", ID: sessionID}, true)
		}
	default:
		return nil
	}

	if inReplyTo != "" {
		// New comment threads persist ReplyTo as their root ID. Walking still keeps
		// routing correct for legacy nested rows and finds the root author.
		if err := considerLoaded(loadRootCommentAuthor(ctx, deps, inReplyTo)); err != nil {
			return err
		}
		if err := considerLoaded(loadCommentAuthor(ctx, deps, inReplyTo)); err != nil {
			return err
		}
	} else if commentID != "" && (event.Type == "comment.resolved" || event.Type == "comment.reopened") {
		if err := considerLoaded(loadRootCommentAuthor(ctx, deps, commentID)); err != nil {
			return err
		}
	}
	if messageInReplyTo != "" {
		if err := considerLoaded(loadMessageAuthor(ctx, deps, messageInReplyTo)); err != nil {
			return err
		}
	}

	for _, author := range targets {
		routed := item
		routed.Topic = contracts.AgentTopicPrefix + author.ID
		if err := publishDestination(ctx, deps, eventID, routed, delivered); err != nil {
			return fmt.Errorf("publish author route to %q: %w", author.ID, err)
		}
	}
	return nil
}

func loadMessageAuthor(ctx context.Context, deps Deps, messageID string) (model.Actor, bool, error) {
	var authorJSON []byte
	if err := deps.Store.Pool.QueryRow(ctx, `select author from messages where id = $1`, messageID).Scan(&authorJSON); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Actor{}, false, nil
		}
		return model.Actor{}, false, fmt.Errorf("load message author %q: %w", messageID, err)
	}
	return decodeAuthor(authorJSON, "message", messageID)
}

func loadCommentAuthor(ctx context.Context, deps Deps, commentID string) (model.Actor, bool, error) {
	var authorJSON []byte
	if err := deps.Store.Pool.QueryRow(ctx, `select author from comments where id = $1`, commentID).Scan(&authorJSON); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Actor{}, false, nil
		}
		return model.Actor{}, false, fmt.Errorf("load comment author %q: %w", commentID, err)
	}
	return decodeAuthor(authorJSON, "comment", commentID)
}

// loadRootCommentAuthor walks a comment's reply_to chain up to its thread root - the comment
// humans reply under - and returns that root's author. comments.reply_to has no acyclicity
// constraint, so the walk unions on the visited row and stops at maxCommentThreadDepth: a
// cycle then finds no reply_to-is-null row and reports no root rather than spinning.
func loadRootCommentAuthor(ctx context.Context, deps Deps, commentID string) (model.Actor, bool, error) {
	var authorJSON []byte
	if err := deps.Store.Pool.QueryRow(ctx, `
		with recursive chain as (
			select id, reply_to, author, 0 as depth from comments where id = $1
			union
			select c.id, c.reply_to, c.author, h.depth + 1
			from comments c join chain h on c.id = h.reply_to
			where h.depth < $2
		)
		select author from chain where reply_to is null limit 1
	`, commentID, maxCommentThreadDepth).Scan(&authorJSON); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Actor{}, false, nil
		}
		return model.Actor{}, false, fmt.Errorf("load root comment author %q: %w", commentID, err)
	}
	return decodeAuthor(authorJSON, "comment", commentID)
}

func decodeAuthor(raw []byte, kind, id string) (model.Actor, bool, error) {
	var author model.Actor
	if err := json.Unmarshal(raw, &author); err != nil {
		return model.Actor{}, false, fmt.Errorf("decode %s author %q: %w", kind, id, err)
	}
	return author, true, nil
}

func publishedDestinationSet(subjects []string) map[string]struct{} {
	delivered := make(map[string]struct{}, len(subjects))
	for _, subject := range subjects {
		delivered[subject] = struct{}{}
	}
	return delivered
}

func publishDestination(ctx context.Context, deps Deps, eventID int64, item contracts.Envelope, delivered map[string]struct{}) error {
	if _, ok := delivered[item.Topic]; ok {
		return nil
	}
	if err := deps.Publisher.Publish(item); err != nil {
		return err
	}
	if _, err := deps.Store.Pool.Exec(ctx, `
		update events
		set published_destinations = array_append(published_destinations, $2)
		where id = $1 and published_at is null and not ($2 = any(published_destinations))
	`, eventID, item.Topic); err != nil {
		return fmt.Errorf("record published destination %q: %w", item.Topic, err)
	}
	delivered[item.Topic] = struct{}{}
	return nil
}

func publishRoute(ctx context.Context, deps Deps, eventID int64, item contracts.Envelope, delivered map[string]struct{}, route *string) error {
	if route == nil || *route == "" {
		return nil
	}
	parsed, err := model.ParseRoute(*route)
	if err != nil {
		return fmt.Errorf("parse route %q: %w", *route, err)
	}
	switch parsed.Kind {
	case "role":
		item.Topic = contracts.RoleTopicPrefix + parsed.ID
	case "session":
		item.Topic = contracts.AgentTopicPrefix + parsed.ID
	default:
		return fmt.Errorf("unsupported route %q", *route)
	}
	if err := publishDestination(ctx, deps, eventID, item, delivered); err != nil {
		return fmt.Errorf("publish route %q: %w", *route, err)
	}
	return nil
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
	} else if event.Project != "" {
		topic = "notifications.dispatch.project." + event.Project + "." + event.Type
	} else {
		return contracts.Envelope{}, fmt.Errorf("event requires an owner")
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
		// correlates to the ask, so the agent's TOON renders "re: <ask ref>".
		if askID := payloadString(event.Payload, "ask_id"); askID != "" {
			item.InReplyTo = askID
		}
	}
	if event.Type == "message.created" || event.Type == "message.answered" {
		// A message reply correlates to the original message so the recipient's
		// TOON renders "re: <message ref>" and the payload carries reply_body.
		if inReplyTo := payloadString(event.Payload, "in_reply_to"); inReplyTo != "" {
			item.InReplyTo = inReplyTo
		}
	}
	if strings.HasPrefix(event.Type, "ask.") {
		item.Urgency = payloadString(event.Payload, "urgency")
	}
	return item, nil
}

func payloadSummary(event model.Event, slug string) string {
	kind := strings.ReplaceAll(event.Type, ".", " ")
	detail := ""
	switch {
	case event.Type == "ask.answered":
		// The answer, not the question: it is the first thing the asker should read.
		detail = text.HeadRunes(askAnswerText(event.Payload), 120)
	case event.Type == "subscription.removed", event.Type == "ask.follower_added", event.Type == "ask.follower_removed":
		detail = payloadString(event.Payload, "session_id")
	case strings.HasPrefix(event.Type, "ask."):
		detail = text.HeadRunes(payloadString(event.Payload, "question"), 120)
	case event.Type == "message.created", event.Type == "message.answered":
		detail = payloadString(event.Payload, "body")
	case strings.HasPrefix(event.Type, "comment."), strings.HasPrefix(event.Type, "suggestion."):
		detail = payloadString(event.Payload, "body")
	}
	owner := ""
	if event.ArtifactID != nil {
		owner = event.Project + "/" + slug
	} else if event.IssueKey != nil {
		owner = *event.IssueKey
	}
	summary := owner + " " + kind
	if detail != "" {
		summary += ": " + detail
	}
	return text.HeadRunes(strings.Join(strings.Fields(summary), " "), 160)
}

func payloadString(payload any, key string) string {
	values, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	value, _ := values[key].(string)
	return value
}

// askAnswerText renders an ask's answer as one line: the text alone, the selected options
// alone, or "<options> - <text>" when a human did both (the same rendering the TS delivery uses).
func askAnswerText(payload any) string {
	values, ok := payload.(map[string]any)
	if !ok {
		return ""
	}
	answer, ok := values["answer"].(map[string]any)
	if !ok {
		return ""
	}
	var selected []string
	if options, ok := answer["selected"].([]any); ok {
		for _, option := range options {
			if label, ok := option.(string); ok {
				selected = append(selected, label)
			}
		}
	}
	text, _ := answer["text"].(string)
	joined := strings.Join(selected, ", ")
	switch {
	case text == "":
		return joined
	case joined == "":
		return text
	default:
		return joined + " - " + text
	}
}
