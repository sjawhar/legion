// Package events owns Dispatch's durable event log and its in-process fan-out.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// commitOrderAdvisoryLock serializes event inserts until their transaction commits.
// PostgreSQL allocates a bigserial value before commit, so this makes the event feed's resume
// cursor's ID order match the only order a page that resubscribes can observe.
const commitOrderAdvisoryLock int64 = 0x4449535041544348

// Broker appends owner-scoped events transactionally and fans committed events
// to local subscribers. NATS publication is an outbox concern owned by A5.
//
// Its fan-out is a Stream of its own (stream.go), so the one implementation of the overflow
// contract serves both it and the reader's live feed. A Broker's subscribers are the ones that
// want this process's own writes as it commits them: the outbox's local wake
// (outbox/publisher.go) and the delivery hub's intake (api/delivery_live_hub_intake.go). An
// events subscription is not one of them; it reads the relay's stream, which carries the other
// process's events too.
type Broker struct {
	stream *Stream
}

// NewBroker creates an empty in-process broker.
func NewBroker() *Broker {
	return &Broker{stream: NewStream()}
}

// LockOwners locks all event owners in a deterministic order. A transaction
// that appends events for more than one owner must call this before its first
// Append, so no owner lock can be acquired after the global commit-order lock.
func (b *Broker) LockOwners(ctx context.Context, tx pgx.Tx, owners ...model.Event) error {
	if b == nil {
		return fmt.Errorf("event broker required")
	}
	type sortableOwner struct {
		key   string
		event model.Event
	}
	sorted := make([]sortableOwner, 0, len(owners))
	for _, owner := range owners {
		key, err := eventOwnerKey(owner)
		if err != nil {
			return err
		}
		sorted = append(sorted, sortableOwner{key: key, event: owner})
	}
	sort.Slice(sorted, func(left, right int) bool {
		return sorted[left].key < sorted[right].key
	})
	for index, owner := range sorted {
		if index > 0 && owner.key == sorted[index-1].key {
			continue
		}
		if _, err := lockEventOwner(ctx, tx, &owner.event); err != nil {
			return err
		}
	}
	return nil
}

// Append assigns the next sequence of the event's owner, writes e in tx, and
// returns the database-assigned event ID. The caller must call Publish only
// after tx commits.
func (b *Broker) Append(ctx context.Context, tx pgx.Tx, e model.Event) (model.Event, error) {
	if b == nil {
		return model.Event{}, fmt.Errorf("event broker required")
	}
	if _, err := eventOwnerKey(e); err != nil {
		return model.Event{}, err
	}
	lastSeq, err := lockEventOwner(ctx, tx, &e)
	if err != nil {
		return model.Event{}, err
	}
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, commitOrderAdvisoryLock); err != nil {
		return model.Event{}, fmt.Errorf("acquire event commit order lock: %w", err)
	}

	if e.IssueKey == nil && e.ArtifactID == nil && e.ProjectKey == nil {
		e.Seq = 0
	} else {
		e.Seq = lastSeq + 1
	}
	e.Notify = b.Notify(e)

	actor, err := json.Marshal(e.Actor)
	if err != nil {
		return model.Event{}, fmt.Errorf("encode event actor: %w", err)
	}
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return model.Event{}, fmt.Errorf("encode event payload: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		insert into events (issue_key, artifact_id, project_key, seq, type, actor, payload, notify)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
		returning id, created_at
	`, e.IssueKey, e.ArtifactID, e.ProjectKey, e.Seq, e.Type, actor, payload, e.Notify).Scan(&e.ID, &e.CreatedAt); err != nil {
		return model.Event{}, fmt.Errorf("insert event: %w", err)
	}
	if e.Type == "ask.opened" {
		opened, ok := e.Payload.(model.AskEventPayload)
		if !ok {
			return model.Event{}, fmt.Errorf("ask.opened payload must be model.AskEventPayload")
		}
		opened.OpenedEventID = &e.ID
		payload, err = json.Marshal(opened)
		if err != nil {
			return model.Event{}, fmt.Errorf("encode ask.opened payload: %w", err)
		}
		if _, err := tx.Exec(ctx, `update events set payload = $2 where id = $1`, e.ID, payload); err != nil {
			return model.Event{}, fmt.Errorf("record ask.opened event identity: %w", err)
		}
		e.Payload = opened
	}
	if e.IssueKey != nil {
		if _, err := tx.Exec(ctx, `
			update issues set last_seq = $2, updated_at = greatest(updated_at, $3) where key = $1
		`, *e.IssueKey, e.Seq, e.CreatedAt); err != nil {
			return model.Event{}, fmt.Errorf("advance issue event sequence: %w", err)
		}
	} else if e.ArtifactID != nil {
		if _, err := tx.Exec(ctx, `
			update artifacts set last_seq = $2 where id = $1
		`, *e.ArtifactID, e.Seq); err != nil {
			return model.Event{}, fmt.Errorf("advance artifact event sequence: %w", err)
		}
	} else if e.ProjectKey != nil {
		if _, err := tx.Exec(ctx, `
			update projects set last_seq = $2 where key = $1
		`, *e.ProjectKey, e.Seq); err != nil {
			return model.Event{}, fmt.Errorf("advance project event sequence: %w", err)
		}
	}
	return e, nil
}

// eventOwnerKey requires exactly one of IssueKey/ArtifactID/ProjectKey (or a session-addressed
// message payload, handled by its caller before this is reached) -- there is no "ownerless"
// event for a plain configuration change. A cross-project singleton settings table (no project,
// issue, or artifact of its own) has nothing valid to pass here: see
// api/settings_delivery.go's putDeliverySettings doc comment for a worked example of a route
// that checked this directly and deliberately skips appending an event rather than inventing an
// owner.
func eventOwnerKey(e model.Event) (string, error) {
	owners := 0
	if e.IssueKey != nil {
		owners++
	}
	if e.ArtifactID != nil {
		owners++
	}
	if e.ProjectKey != nil {
		owners++
	}
	if owners == 0 {
		sessionID, err := agentEventSessionID(e)
		if err != nil {
			return "", err
		}
		return "agent:" + sessionID, nil
	}
	if owners != 1 {
		return "", fmt.Errorf("event requires at most one owner")
	}
	if e.IssueKey != nil {
		return "issue:" + *e.IssueKey, nil
	}
	if e.ArtifactID != nil {
		return "artifact:" + *e.ArtifactID, nil
	}
	return "project:" + *e.ProjectKey, nil
}

func agentEventSessionID(e model.Event) (string, error) {
	var target string
	switch payload := e.Payload.(type) {
	case model.Message:
		if payload.Target != nil {
			target = *payload.Target
		}
	case model.MessageEventPayload:
		if payload.Target != nil {
			target = *payload.Target
		}
	case model.MessageDeliveryEventPayload:
		target = payload.Target
	case model.MessageAcceptedEventPayload:
		target = payload.Target
	case model.UserAgentStateEventPayload:
		target = "session:" + payload.SessionID
	}
	route, err := model.ParseRoute(target)
	if err != nil || route.Kind != "session" {
		return "", fmt.Errorf("ownerless event requires session target")
	}
	return route.ID, nil
}

func lockEventOwner(ctx context.Context, tx pgx.Tx, e *model.Event) (int, error) {
	var lastSeq int
	var err error
	switch {
	case e.IssueKey != nil:
		err = tx.QueryRow(ctx, `
			select last_seq, project_key from issues where key = $1 for no key update
		`, *e.IssueKey).Scan(&lastSeq, &e.Project)
		if err != nil {
			return 0, fmt.Errorf("lock issue for event: %w", err)
		}
	case e.ArtifactID != nil:
		err = tx.QueryRow(ctx, `
			select last_seq, project_key
			from artifacts
			where id = $1 and issue_key is null
			for no key update
		`, *e.ArtifactID).Scan(&lastSeq, &e.Project)
		if err != nil {
			return 0, fmt.Errorf("lock unlinked artifact for event: %w", err)
		}
	case e.ProjectKey != nil:
		err = tx.QueryRow(ctx, `select last_seq from projects where key = $1 for no key update`, *e.ProjectKey).Scan(&lastSeq)
		if err != nil {
			return 0, fmt.Errorf("lock project for event: %w", err)
		}
		e.Project = *e.ProjectKey
	default:
		return 0, nil
	}
	return lastSeq, nil
}

// Notify reports whether an event should wake agents and receive routed delivery.
// The issue topic, event log, and event feed carry every event regardless of this value.
// A message reply (`message.answered`) follows the same rule as a fresh message: a targeted
// one reached its session through the synchronous listener send and stays quiet; a human's
// reply on a plain agent message must wake the route and the message's author.
func (b *Broker) Notify(e model.Event) bool {
	if e.Type == "message.delivery" || e.Type == "comment.delivery" {
		return false
	}
	if e.Type == "message.created" || e.Type == "message.answered" {
		if message, ok := e.Payload.(model.MessageEventPayload); ok && message.Target != nil {
			return false
		}
	}
	switch e.Type {
	case "comment.anchor_refreshed", "ask.anchor_refreshed":
		// Refresh is a document mutation side effect, not a human addressing its
		// author. Retain it for every stream consumer without waking anyone.
		return false
	case "project.created", "settings.repo_project.updated", "settings.architecture_source.updated",
		"settings.project_default_assignee.updated",
		"architecture.synced", "architecture.sync_failed", "user_state.updated", "user_agent_state.updated",
		"user_thread_state.updated":
		// Settings, importer and per-viewer bookkeeping: retained on their log and carried by
		// the event feed, but nobody is woken by them.
		return false
	case "child.status", "child.added", "child.removed":
		// A child status flip or a reparent changes the parent's Children set whoever
		// the actor is; the parent's watchers always hear about it.
		return true
	case "ask.edited":
		// A version move of an approval request that an earlier version already moved since it was
		// opened or last handed back changes only the version it names: quiet, as a human's unnamed
		// version is below.
		if edit, ok := e.Payload.(model.AskEditEventPayload); ok && edit.Quiet {
			return false
		}
	}
	if e.Actor.Kind != "user" {
		return false
	}
	if e.Type != "artifact.version" {
		return true
	}
	return namedVersion(e.Payload)
}

func namedVersion(payload any) bool {
	values, ok := payload.(map[string]any)
	if !ok {
		return false
	}
	switch version := values["version"].(type) {
	case model.Version:
		return version.Named
	case *model.Version:
		return version != nil && version.Named
	case map[string]any:
		named, _ := version["named"].(bool)
		return named
	default:
		return false
	}
}

// Subscribe returns a committed-event stream and a cancellation function.
func (b *Broker) Subscribe() (<-chan model.Event, func()) { return b.stream.Subscribe() }

// SubscriberCount reports the active local fan-out subscriptions.
func (b *Broker) SubscriberCount() int { return b.stream.SubscriberCount() }

// Publish fans a committed event out to local subscribers, which the caller does only once the
// appending transaction has committed. An overflowed subscriber is dropped and its channel
// closed (Stream.Publish).
func (b *Broker) Publish(e model.Event) { b.stream.Publish(e) }

// CloseAll disconnects every active local subscriber. Test-only.
func (b *Broker) CloseAll() { b.stream.CloseAll() }
