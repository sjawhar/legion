// Package events owns Dispatch's durable event log and in-process SSE fan-out.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// commitOrderAdvisoryLock serializes event inserts until their transaction commits.
// PostgreSQL allocates a bigserial value before commit, so this makes the SSE resume
// cursor's ID order match the only order a disconnected client can observe.
const commitOrderAdvisoryLock int64 = 0x4449535041544348

// Broker appends owner-scoped events transactionally and fans committed events
// to local subscribers. NATS publication is an outbox concern owned by A5.
type Broker struct {
	mu          sync.Mutex
	nextID      uint64
	subscribers map[uint64]chan model.Event
}

// NewBroker creates an empty in-process broker.
func NewBroker() *Broker {
	return &Broker{subscribers: make(map[uint64]chan model.Event)}
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

	e.Seq = lastSeq + 1
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
		ask, ok := e.Payload.(model.Ask)
		if !ok {
			return model.Event{}, fmt.Errorf("ask.opened payload must be model.Ask")
		}
		ask.OpenedEventID = &e.ID
		payload, err = json.Marshal(ask)
		if err != nil {
			return model.Event{}, fmt.Errorf("encode ask.opened payload: %w", err)
		}
		if _, err := tx.Exec(ctx, `update events set payload = $2 where id = $1`, e.ID, payload); err != nil {
			return model.Event{}, fmt.Errorf("record ask.opened event identity: %w", err)
		}
		e.Payload = ask
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
	} else if _, err := tx.Exec(ctx, `
		update projects set last_seq = $2 where key = $1
	`, *e.ProjectKey, e.Seq); err != nil {
		return model.Event{}, fmt.Errorf("advance project event sequence: %w", err)
	}
	return e, nil
}

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
	if owners != 1 {
		return "", fmt.Errorf("event requires exactly one owner")
	}
	if e.IssueKey != nil {
		return "issue:" + *e.IssueKey, nil
	}
	if e.ArtifactID != nil {
		return "artifact:" + *e.ArtifactID, nil
	}
	return "project:" + *e.ProjectKey, nil
}

func lockEventOwner(ctx context.Context, tx pgx.Tx, e *model.Event) (int, error) {
	var lastSeq int
	var err error
	switch {
	case e.IssueKey != nil:
		err = tx.QueryRow(ctx, `
			select last_seq, project_key from issues where key = $1 for update
		`, *e.IssueKey).Scan(&lastSeq, &e.Project)
		if err != nil {
			return 0, fmt.Errorf("lock issue for event: %w", err)
		}
	case e.ArtifactID != nil:
		err = tx.QueryRow(ctx, `
			select last_seq, project_key
			from artifacts
			where id = $1 and issue_key is null
			for update
		`, *e.ArtifactID).Scan(&lastSeq, &e.Project)
		if err != nil {
			return 0, fmt.Errorf("lock unlinked artifact for event: %w", err)
		}
	default:
		err = tx.QueryRow(ctx, `select last_seq from projects where key = $1 for update`, *e.ProjectKey).Scan(&lastSeq)
		if err != nil {
			return 0, fmt.Errorf("lock project for event: %w", err)
		}
		e.Project = *e.ProjectKey
	}
	return lastSeq, nil
}

// Notify reports whether an event should wake agents and receive routed delivery.
// The issue topic, event log, and SSE carry every event regardless of this value.
func (b *Broker) Notify(e model.Event) bool {
	if e.Type == "message.delivery" || e.Type == "message.answered" {
		return false
	}
	if e.Type == "message.created" {
		if message, ok := e.Payload.(model.MessageEventPayload); ok && message.Target != nil {
			return false
		}
	}
	switch e.Type {
	case "project.created", "settings.repo_project.updated", "user_state.updated":
		return false
	}
	if e.Type == "child.status" {
		return true
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
func (b *Broker) Subscribe() (<-chan model.Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	ch := make(chan model.Event, 64)
	b.subscribers[id] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if existing, ok := b.subscribers[id]; ok {
				delete(b.subscribers, id)
				close(existing)
			}
		})
	}
}

// SubscriberCount reports the active local fan-out subscriptions.
func (b *Broker) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers)
}

// Publish fans a committed event out to local subscribers. An overflowed SSE
// client is disconnected before it can advance its replay cursor past an event.
func (b *Broker) Publish(e model.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ch := range b.subscribers {
		select {
		case ch <- e:
		default:
			delete(b.subscribers, id)
			close(ch)
		}
	}
}

// CloseAll disconnects every active local subscriber, ending their SSE response
// bodies as if the server had restarted. Test-only: exercises a client's
// reconnect-from-lastId path without seeding enough events to trip the replay cap.
func (b *Broker) CloseAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ch := range b.subscribers {
		delete(b.subscribers, id)
		close(ch)
	}
}
