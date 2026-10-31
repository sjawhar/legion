// Package events owns Dispatch's durable event log and its in-process fan-out.
package events

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

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

// LockOwners takes the owner locks of every event owner (store.LockOwners), sorted by lock key. A
// transaction that appends events for more than one owner must call this before its first Append.
// Append takes each owner's lock as it reaches it, so two transactions that share owners and
// reached them in opposite orders (a child patch locks the child and then appends the parent's
// child.status; a parent patch locks the parent and then the child) would each hold the lock the
// other waits for, until Postgres killed one. Taking every owner up front in one order leaves no
// cycle to wait in: each Append then takes its owner's lock again, which returns at once, and its
// owner's row, which no other writer holds without that lock. An ownerless event addressed to a
// session has no owner lock and adds none; its Append takes the transaction id itself. LockOwners
// locks no row, so the transaction holds no transaction id on its account.
func (b *Broker) LockOwners(ctx context.Context, tx pgx.Tx, owners ...model.Event) error {
	if b == nil {
		return fmt.Errorf("event broker required")
	}
	locks := make([]store.Owner, 0, len(owners))
	for _, owner := range owners {
		if _, err := eventOwnerKey(owner); err != nil {
			return err
		}
		if lock, ok := eventOwnerLock(owner); ok {
			locks = append(locks, lock)
		}
	}
	return store.LockOwners(ctx, tx, locks...)
}

// Append assigns the next sequence of the event's owner, writes e in tx, and returns the appended
// event with its database-assigned ID, sequence and time. Its statements go to the server as one
// batch (QueueAppend). The caller must call Publish only after tx commits.
//
// The transaction holds its transaction id before the insert draws e's id from events_id_seq:
// lockEventOwner takes it with pg_current_xact_id in the statement that locks the owner row,
// queued before the insert, or in a statement of its own for an ownerless event. That is the
// writer invariant the safe horizon's proof rests on (horizon.go): a transaction that drew an id
// g holds an xid below that of any observer that saw a later id commit.
//
// Append takes no lock beyond its owner's: the owner lock (store.LockOwner) and the owner row,
// and those are all the order event ids carry:
//
//   - One owner's events commit in id order. The owner lock is taken and the owner row locked
//     for no key update before the id is drawn, and both stay held until the transaction ends,
//     so a second write on the same owner allocates its id only after it gets the owner lock,
//     which the first holds until it commits: its id and its seq are above the first's.
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
	var appended model.Event
	if err := store.RunWrite(ctx, tx, func(batch *store.WriteBatch) error {
		return b.QueueAppend(batch, e, func(written model.Event) { appended = written })
	}); err != nil {
		return model.Event{}, err
	}
	return appended, nil
}

// QueueAppend queues e's append on a write phase's batch: its owner's lock and row
// (lockEventOwner), which take the transaction's id, and the insert, which takes the next sequence
// from the owner row as it stands when the insert runs and advances the row's last_seq (and an
// issue's updated_at) to the row it inserted, in the same statement. An ask.opened event's payload
// then records the event's own id (opened_event_id) through the id sequence's currval, the id the
// insert just took. So no value of one statement is read in Go before the next one is sent.
// appended receives the event as written - its id, sequence, time, the owner's project and the
// payload as stored - once the batch has read those statements' results, before any statement
// queued after them is read. A statement queued right after these may name the event's id as
// store.LatestEventID.
func (b *Broker) QueueAppend(batch *store.WriteBatch, e model.Event, appended func(model.Event)) error {
	return b.QueueAppendWithLogins(batch, e, nil, appended)
}

// QueueAppendWithLogins is QueueAppend for an event whose payload names thread_logins: the insert
// itself reads them (logins) and writes them into the payload it stores, so they are what the
// transaction's earlier statements, and the user_threads triggers those fired, left - what the
// writer would read after its write - with no read in Go between the write and the event. The
// appended event carries them too. A nil logins is QueueAppend.
func (b *Broker) QueueAppendWithLogins(batch *store.WriteBatch, e model.Event, logins *Logins, appended func(model.Event)) error {
	if b == nil {
		return fmt.Errorf("event broker required")
	}
	if _, err := eventOwnerKey(e); err != nil {
		return err
	}
	if logins != nil {
		if _, err := withThreadLogins(e.Payload, nil); err != nil {
			return err
		}
	}
	e.Notify = b.Notify(e)
	actor, err := json.Marshal(e.Actor)
	if err != nil {
		return fmt.Errorf("encode event actor: %w", err)
	}
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("encode event payload: %w", err)
	}
	var opened model.AskEventPayload
	if e.Type == "ask.opened" {
		var ok bool
		if opened, ok = e.Payload.(model.AskEventPayload); !ok {
			return fmt.Errorf("ask.opened payload must be model.AskEventPayload")
		}
	}
	event := &e
	lockEventOwner(batch, event)
	// One statement takes the next sequence from the owner row and advances the row to the event
	// it inserted. Every statement of a data-modifying CTE reads one snapshot, so the insert reads
	// last_seq as the statements before it left it, and the owner's update writes the very
	// sequence the insert took. Only the owner the event names matches a row: the other two keys
	// are null, and an ownerless event (a session-addressed message) takes sequence 0 and advances
	// nothing. The thread_logins the payload names are read in the same snapshot, which holds
	// every write the transaction made before it.
	storedPayload, storedLogins := `$6`, `null::jsonb`
	args := []any{e.IssueKey, e.ArtifactID, e.ProjectKey, e.Type, actor, payload, e.Notify}
	if logins != nil {
		storedPayload = `$6::jsonb || jsonb_build_object('thread_logins', (` + logins.query("$8") + `))`
		storedLogins = `payload->'thread_logins'`
		args = append(args, logins.key)
	}
	batch.Queue("", `
		with appended as (
			insert into events (issue_key, artifact_id, project_key, seq, type, actor, payload, notify)
			values ($1, $2, $3, coalesce(
				(select last_seq from issues where key = $1) + 1,
				(select last_seq from artifacts where id = $2) + 1,
				(select last_seq from projects where key = $3) + 1,
				0), $4, $5, `+storedPayload+`, $7)
			returning id, seq, created_at, `+storedLogins+` as thread_logins
		), issue as (
			update issues set last_seq = appended.seq, updated_at = greatest(issues.updated_at, appended.created_at)
			from appended where issues.key = $1
		), artifact as (
			update artifacts set last_seq = appended.seq from appended where artifacts.id = $2
		), project as (
			update projects set last_seq = appended.seq from appended where projects.key = $3
		)
		select id, seq, created_at, thread_logins from appended
	`, args...).QueryRow(func(row pgx.Row) error {
		var threadLogins []string
		if err := row.Scan(&event.ID, &event.Seq, &event.CreatedAt, &threadLogins); err != nil {
			return fmt.Errorf("insert event: %w", err)
		}
		if logins != nil {
			withLogins, err := withThreadLogins(event.Payload, threadLogins)
			if err != nil {
				return err
			}
			event.Payload = withLogins
			opened.ThreadLogins = threadLogins
		}
		if event.Type != "ask.opened" {
			appended(*event)
		}
		return nil
	})
	if e.Type == "ask.opened" {
		batch.Queue("record ask.opened event identity", `
			update events set payload = jsonb_set(payload, '{opened_event_id}', to_jsonb(id))
			where id = `+store.LatestEventID+`
		`).Exec(func(pgconn.CommandTag) error {
			opened.OpenedEventID = &event.ID
			event.Payload = opened
			appended(*event)
			return nil
		})
	}
	return nil
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

// eventOwnerLock is the owner lock of e's owner, false for an ownerless event (a session-addressed
// message), which has no owner row and no lock.
func eventOwnerLock(e model.Event) (store.Owner, bool) {
	switch {
	case e.IssueKey != nil:
		return store.IssueOwner(*e.IssueKey), true
	case e.ArtifactID != nil:
		return store.ArtifactOwner(*e.ArtifactID), true
	case e.ProjectKey != nil:
		return store.ProjectOwner(*e.ProjectKey), true
	}
	return store.Owner{}, false
}

// lockEventOwner queues e's owner lock (store.QueueLockOwner), which returns at once when the
// transaction took it in its read phase, as every writer does, and then its owner's row `for no
// key update`, whose callback records the owner's project on e. The row lock is a write-phase lock:
// every Dispatch writer of an owner row takes the owner lock first, so the row is never contended
// among them. The two stay two statements: the row's read takes its snapshot once the lock is
// granted.
//
// Every branch also takes the transaction's id (pg_current_xact_id, scanned and discarded) in a
// statement queued before the insert draws an event id: the writer invariant Append documents.
// Inside a savepoint pg_current_xact_id assigns and returns the top-level transaction's id, which
// is the one snapshots list. An ownerless event has no lock and no row and takes only the id.
func lockEventOwner(batch *store.WriteBatch, e *model.Event) {
	owner, ok := eventOwnerLock(*e)
	if !ok {
		batch.Queue("take transaction id for event", `select pg_current_xact_id()`)
		return
	}
	var what, row string
	switch owner.Kind {
	case store.OwnerIssue:
		what = "issue"
		row = `select project_key, pg_current_xact_id() from issues where key = $1 for no key update`
	case store.OwnerArtifact:
		what = "unlinked artifact"
		row = `
			select project_key, pg_current_xact_id()
			from artifacts
			where id = $1 and issue_key is null
			for no key update
		`
	default:
		what = "project"
		row = `select key, pg_current_xact_id() from projects where key = $1 for no key update`
	}
	store.QueueLockOwner(batch, owner)
	batch.Queue("", row, owner.Key).QueryRow(func(row pgx.Row) error {
		if err := row.Scan(&e.Project, nil); err != nil {
			return fmt.Errorf("lock %s for event: %w", what, err)
		}
		return nil
	})
}

// Quiet reports whether an event carries nothing for a reader that follows its owner topic: a
// delivery receipt, an anchor refresh, settings or per-viewer bookkeeping, a targeted message the
// session it names already had through the synchronous listener send, an ask edit that only moved
// the version it names, or a document version nobody named.
//
// It is the half of Notify that does not depend on who acted, and it is what the outbox puts on
// the wire (`quiet`), because the two answer different questions. Notify answers whether to wake
// the route, the claimant and the authors, so an agent's own comment is not a wake. Quiet answers
// whether the event is worth showing at all, so an agent's comment, document or ask is not quiet:
// a session that subscribed to `notifications.dispatch.issue.<KEY>.>` asked for exactly that
// activity (skills/dispatch/SKILL.md), and a reader that skipped every non-notifying frame saw
// none of it.
func Quiet(e model.Event) bool {
	if e.Type == "message.delivery" || e.Type == "comment.delivery" {
		return true
	}
	if e.Type == "message.created" || e.Type == "message.answered" {
		if targetedMessage(e.Payload) {
			return true
		}
	}
	switch e.Type {
	case "comment.anchor_refreshed", "ask.anchor_refreshed":
		// Refresh is a document mutation side effect, not a human addressing its
		// author. Retain it for every stream consumer without waking anyone.
		return true
	case "project.created", "settings.repo_project.updated", "settings.architecture_source.updated",
		"settings.project_default_assignee.updated",
		"architecture.synced", "architecture.sync_failed", "user_state.updated", "user_agent_state.updated",
		"user_thread_state.updated":
		// Settings, importer and per-viewer bookkeeping: retained on their log and carried by
		// the event feed, but nobody is woken by them.
		return true
	case "ask.edited":
		// A version move of an approval request that an earlier version already moved since it was
		// opened or last handed back changes only the version it names.
		if quietAskEdit(e.Payload) {
			return true
		}
	case "artifact.version":
		// An unnamed version is a document's autosave: every writer makes them constantly, and
		// neither a wake nor a card is owed for one, whoever wrote it.
		return !namedVersion(e.Payload)
	}
	return false
}

// Notify reports whether an event should wake agents and receive routed delivery.
// The issue topic, event log, and event feed carry every event regardless of this value.
// A message reply (`message.answered`) follows the same rule as a fresh message: a targeted
// one reached its session through the synchronous listener send and stays quiet; a human's
// reply on a plain agent message must wake the route and the message's author.
func (b *Broker) Notify(e model.Event) bool {
	if Quiet(e) {
		return false
	}
	switch e.Type {
	case "child.status", "child.added", "child.removed":
		// A child status flip or a reparent changes the parent's Children set whoever
		// the actor is; the parent's watchers always hear about it.
		return true
	}
	return e.Actor.Kind == "user"
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

// targetedMessage reports whether a message event names the session the message was aimed at.
//
// Quiet is asked of one event from two payload shapes, and must answer the same for both: at
// append time (Broker.Notify) the payload is the typed value the caller built, and at publish
// time it is the event the outbox scanned out of Postgres, whose payload JSON decodes into a
// map[string]any (outbox/publisher.go, scanPendingEvents). quietAskEdit and namedVersion read
// their field the same way.
func targetedMessage(payload any) bool {
	switch message := payload.(type) {
	case model.MessageEventPayload:
		return message.Target != nil
	case map[string]any:
		// Target is a *string, so the decoded field is a string on a targeted message and
		// nil ("target": null) on every other.
		_, targeted := message["target"].(string)
		return targeted
	default:
		return false
	}
}

// quietAskEdit reports whether an ask edit marks itself a version move that changes only the
// version the request names (model.AskEditEventPayload.Quiet), from either payload shape.
func quietAskEdit(payload any) bool {
	switch edit := payload.(type) {
	case model.AskEditEventPayload:
		return edit.Quiet
	case map[string]any:
		// Quiet is omitempty, so the decoded field is absent unless the edit set it.
		quiet, _ := edit["quiet"].(bool)
		return quiet
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
