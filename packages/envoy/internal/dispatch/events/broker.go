// Package events owns Dispatch's durable event log and in-process SSE fan-out.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Broker appends owner-scoped events transactionally and fans committed events
// to local subscribers. NATS publication is an outbox concern owned by A5.
//
// Its fan-out is a Stream of its own (stream.go), so the one implementation of the overflow
// contract serves both it and the reader's live feed. A Broker's subscribers are the ones that
// want this process's own writes as it commits them: the outbox's local wake
// (outbox/publisher.go) and the delivery hub's intake (api/delivery_live_hub_intake.go). An open
// SSE connection is not one of them; it reads the relay's stream, which carries the other
// process's events too.
type Broker struct {
	stream *Stream
}

// NewBroker creates an empty in-process broker.
func NewBroker() *Broker {
	return &Broker{stream: NewStream()}
}

// LockOwners locks all event owners in a deterministic order. A transaction that appends
// events for more than one owner must call this before its first Append. Append locks each
// owner's row as it reaches it, so two transactions that share owners and reached them in
// opposite orders (a child patch locks the child and then appends the parent's child.status; a
// parent patch locks the parent and then the child) would each hold the row the other waits
// for, until Postgres killed one. Taking every owner up front in one order (sorted by owner
// key) leaves no cycle to wait in.
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
		// An ownerless (session-addressed) event has no row to lock; its Append takes the
		// transaction id itself.
		if (index > 0 && owner.key == sorted[index-1].key) || strings.HasPrefix(owner.key, "agent:") {
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
//
// The transaction holds its transaction id before the insert draws e's id from events_id_seq:
// lockEventOwner takes it with pg_current_xact_id in the same statement that locks the owner
// row, or on its own for an ownerless event. That is the writer invariant the safe horizon's
// proof rests on (horizon.go): a transaction that drew an id g holds an xid below that of any
// observer that saw a later id commit.
//
// Append takes no lock beyond its owner's row, and that row lock is all the order event ids
// carry:
//
//   - One owner's events commit in id order. The owner row is locked for no key update before
//     the id is drawn and stays locked until the transaction ends, so a second write on the same
//     owner allocates its id only after it gets the owner row, which the first holds until it
//     commits: its id and its seq are above the first's.
//   - Different owners' writes do not wait for each other and may commit out of id order: the
//     write that drew id 7 can still be open after the write that drew id 8 has committed. A
//     reader that pages the log by a global id cursor reads through a Horizon, only up to the
//     safe horizon, never up to max(id).
//   - An ownerless (session-addressed) event takes no lock of its own: its seq is 0, so there
//     is no sequence to allocate, and two transactions that touch no common row have no defined
//     order, so either commit order is a valid serialization. Order matters only between writes
//     that conflict, and every ownerless append follows its transaction's own lock on the row
//     it changes, which orders it against every write that changes that row. A message create
//     (api/messages.go createStoredMessage) and a broadcast (api/broadcasts.go writeBroadcast)
//     append after inserting the new message row, a reply's insert taking its parent message
//     row for key share through the foreign key, and a broadcast's new broadcasts and
//     idempotency-key rows before it. An accept (api/message_delivery.go acceptDelivery) and a
//     pending delivery's supersede receipt (recordPendingMessageDelivery) append after locking
//     the message row for no key update and writing the attempt's message_deliveries row. The
//     reply route's failure receipt and a session's reply (replyMessage, insertSessionReply)
//     append after locking the attempt row for update. A viewer's state (api/state.go
//     putUserAgentState) appends only after it wrote a user_agent_state, user_agent_read or
//     user_agent_reply_read row. A delivery settle (api/envoy_resolve.go settleDeliveryAttempt)
//     appends after its update writes the attempt row it claimed; on the branch where that
//     update finds the row already settled by the session's committed reply, it only reads the
//     row, which no transaction writes a receipt for again. A per-session lock would add no
//     order these row locks do not give, only deadlock cycles: the accept path would take it
//     before the message row and a pending delivery after, and a broadcast one per recipient
//     in no sorted order.
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

// lockEventOwner locks e's owner row for no key update and returns its last sequence, filling
// e.Project from the owner. Every branch also takes the transaction's id (pg_current_xact_id,
// scanned and discarded) before Append draws an event id: the writer invariant Append documents.
// Inside a savepoint pg_current_xact_id assigns and returns the top-level transaction's id,
// which is the one snapshots list. An ownerless event has no row to lock and takes only the id.
func lockEventOwner(ctx context.Context, tx pgx.Tx, e *model.Event) (int, error) {
	var lastSeq int
	var err error
	switch {
	case e.IssueKey != nil:
		err = tx.QueryRow(ctx, `
			select last_seq, project_key, pg_current_xact_id() from issues where key = $1 for no key update
		`, *e.IssueKey).Scan(&lastSeq, &e.Project, nil)
		if err != nil {
			return 0, fmt.Errorf("lock issue for event: %w", err)
		}
	case e.ArtifactID != nil:
		err = tx.QueryRow(ctx, `
			select last_seq, project_key, pg_current_xact_id()
			from artifacts
			where id = $1 and issue_key is null
			for no key update
		`, *e.ArtifactID).Scan(&lastSeq, &e.Project, nil)
		if err != nil {
			return 0, fmt.Errorf("lock unlinked artifact for event: %w", err)
		}
	case e.ProjectKey != nil:
		err = tx.QueryRow(ctx, `
			select last_seq, pg_current_xact_id() from projects where key = $1 for no key update
		`, *e.ProjectKey).Scan(&lastSeq, nil)
		if err != nil {
			return 0, fmt.Errorf("lock project for event: %w", err)
		}
		e.Project = *e.ProjectKey
	default:
		if _, err := tx.Exec(ctx, `select pg_current_xact_id()`); err != nil {
			return 0, fmt.Errorf("take transaction id for event: %w", err)
		}
		return 0, nil
	}
	return lastSeq, nil
}

// Notify reports whether an event should wake agents and receive routed delivery.
// The issue topic, event log, and SSE carry every event regardless of this value.
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
		// SSE, but nobody is woken by them.
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
