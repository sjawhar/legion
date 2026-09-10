// Package events owns Dispatch's durable event log and in-process SSE fan-out.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Broker appends issue events transactionally and fans committed events to
// local subscribers. NATS publication is an outbox concern owned by A5.
type Broker struct {
	mu          sync.Mutex
	nextID      uint64
	subscribers map[uint64]chan model.Event
}

// NewBroker creates an empty in-process broker.
func NewBroker() *Broker {
	return &Broker{subscribers: make(map[uint64]chan model.Event)}
}

// Append assigns the next per-issue sequence, writes e in tx, and returns the
// database-assigned event ID. The caller must call Publish only after tx commits.
func (b *Broker) Append(ctx context.Context, tx pgx.Tx, e model.Event) (model.Event, error) {
	if b == nil {
		return model.Event{}, fmt.Errorf("event broker required")
	}
	var lastSeq int
	if err := tx.QueryRow(ctx, `select last_seq from issues where key = $1 for update`, e.IssueKey).Scan(&lastSeq); err != nil {
		return model.Event{}, fmt.Errorf("lock issue for event: %w", err)
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
		insert into events (issue_key, seq, type, actor, payload, notify)
		values ($1, $2, $3, $4, $5, $6)
		returning id, created_at
	`, e.IssueKey, e.Seq, e.Type, actor, payload, e.Notify).Scan(&e.ID, &e.CreatedAt); err != nil {
		return model.Event{}, fmt.Errorf("insert event: %w", err)
	}
	if _, err := tx.Exec(ctx, `update issues set last_seq = $2 where key = $1`, e.IssueKey, e.Seq); err != nil {
		return model.Event{}, fmt.Errorf("advance issue event sequence: %w", err)
	}
	return e, nil
}

// Notify reports whether an event should wake agents and receive routed delivery.
// The issue topic, event log, and SSE carry every event regardless of this value.
func (b *Broker) Notify(e model.Event) bool {
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
