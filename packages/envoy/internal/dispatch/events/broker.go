// Package events owns Dispatch's durable event log and its in-process fan-out.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

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
	_, _, err := lockOwners(ctx, tx, owners)
	return err
}

// lockedOwner is what lockEventOwner read for one owner: its last sequence and its project.
type lockedOwner struct {
	lastSeq int
	project string
}

// lockOwners validates every event's owner, then locks each distinct owner once, in the order
// of their owner keys, so two transactions that share owners always take them in one order.
// It returns each event's owner key, by index, and what each owner's lock read, by owner key.
// LockOwners and AppendAll share it, so the one lock order serves both.
func lockOwners(ctx context.Context, tx pgx.Tx, events []model.Event) ([]string, map[string]*lockedOwner, error) {
	keys := make([]string, len(events))
	order := make([]int, len(events))
	for index, event := range events {
		key, err := eventOwnerKey(event)
		if err != nil {
			return nil, nil, err
		}
		keys[index] = key
		order[index] = index
	}
	slices.SortStableFunc(order, func(left, right int) int {
		return strings.Compare(keys[left], keys[right])
	})
	locked := make(map[string]*lockedOwner, len(events))
	for _, index := range order {
		// An ownerless (session-addressed) event has no row to lock; AppendAll takes the
		// transaction id itself.
		if _, done := locked[keys[index]]; done || ownerless(events[index]) {
			continue
		}
		owner := events[index]
		lastSeq, err := lockEventOwner(ctx, tx, &owner)
		if err != nil {
			return nil, nil, err
		}
		locked[keys[index]] = &lockedOwner{lastSeq: lastSeq, project: owner.Project}
	}
	return keys, locked, nil
}

// Append assigns the next sequence of the event's owner, writes e in tx, and
// returns the database-assigned event ID. The caller must call Publish only
// after tx commits. It is AppendAll of e alone.
func (b *Broker) Append(ctx context.Context, tx pgx.Tx, e model.Event) (model.Event, error) {
	appended, err := b.AppendAll(ctx, tx, []model.Event{e})
	if err != nil {
		return model.Event{}, err
	}
	return appended[0], nil
}

// AppendAll writes events in tx as if each were appended in turn, and returns them, in the order
// given, with their database-assigned IDs. Each owner's events take that owner's next sequences
// in slice order; an ownerless (session-addressed) event's sequence is 0. The caller must call
// Publish only after tx commits.
//
// What the events cost does not grow with how many there are, only with how many owners they
// have: every owner is validated before any is locked, each distinct owner is locked once, in
// LockOwners' order, every row is inserted by one statement, and what follows the insert - each
// ask.opened event's payload naming its own id, and each owner's last_seq advancing once, to its
// last event's sequence - goes in one pipeline. Every round trip after an owner's row is locked
// makes every other writer of that owner wait, so a caller with several events to write hands
// them over together.
//
// The transaction holds its transaction id before the insert draws the events' ids from
// events_id_seq: lockEventOwner takes it with pg_current_xact_id in the same statement that locks
// an owner row, and AppendAll takes it on its own for a batch that locked no owner row. That is
// the writer invariant the safe horizon's proof rests on (horizon.go): a transaction that drew an
// id g holds an xid below that of any observer that saw a later id commit.
//
// AppendAll takes no lock beyond its owners' rows, and those row locks are all the order event
// ids carry:
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
func (b *Broker) AppendAll(ctx context.Context, tx pgx.Tx, events []model.Event) ([]model.Event, error) {
	if b == nil {
		return nil, fmt.Errorf("event broker required")
	}
	if len(events) == 0 {
		return nil, nil
	}
	keys, owners, err := lockOwners(ctx, tx, events)
	if err != nil {
		return nil, err
	}
	// The transaction must hold its transaction id before the insert below draws ids from
	// events_id_seq, the writer invariant above. Each owner's lock took it in the statement
	// that locked the row (lockEventOwner); a batch of none but ownerless events locked no
	// row, so it takes the id on its own here.
	if len(owners) == 0 {
		if _, err := tx.Exec(ctx, `select pg_current_xact_id()`); err != nil {
			return nil, fmt.Errorf("take transaction id for event: %w", err)
		}
	}

	appended := make([]model.Event, len(events))
	issueKeys := make([]*string, len(events))
	artifactIDs := make([]*string, len(events))
	projectKeys := make([]*string, len(events))
	seqs := make([]int, len(events))
	types := make([]string, len(events))
	actors := make([][]byte, len(events))
	payloads := make([][]byte, len(events))
	notifies := make([]bool, len(events))
	for index, e := range events {
		if ownerless(e) {
			e.Seq = 0
		} else {
			owner := owners[keys[index]]
			owner.lastSeq++
			e.Seq = owner.lastSeq
			e.Project = owner.project
		}
		e.Notify = b.Notify(e)
		actor, err := json.Marshal(e.Actor)
		if err != nil {
			return nil, fmt.Errorf("encode event actor: %w", err)
		}
		payload, err := json.Marshal(e.Payload)
		if err != nil {
			return nil, fmt.Errorf("encode event payload: %w", err)
		}
		appended[index] = e
		issueKeys[index], artifactIDs[index], projectKeys[index] = e.IssueKey, e.ArtifactID, e.ProjectKey
		seqs[index], types[index], actors[index], payloads[index], notifies[index] = e.Seq, e.Type, actor, payload, e.Notify
	}
	// The insert takes its rows in input order (with ordinality, order by position), and the
	// bigserial default draws each row's id as the row is inserted, so ids rise in input order:
	// reading them back ordered by id pairs each with the event it was drawn for.
	rows, err := tx.Query(ctx, `
		with inserted as (
			insert into events (issue_key, artifact_id, project_key, seq, type, actor, payload, notify)
			select issue_key, artifact_id, project_key, seq, type, actor, payload, notify
			from unnest($1::text[], $2::uuid[], $3::text[], $4::integer[], $5::text[], $6::jsonb[], $7::jsonb[], $8::boolean[])
				with ordinality as input(issue_key, artifact_id, project_key, seq, type, actor, payload, notify, position)
			order by position
			returning id, created_at
		)
		select id, created_at from inserted order by id
	`, issueKeys, artifactIDs, projectKeys, seqs, types, actors, payloads, notifies)
	if err != nil {
		return nil, fmt.Errorf("insert event: %w", err)
	}
	inserted := 0
	for rows.Next() {
		if inserted == len(appended) {
			rows.Close()
			return nil, fmt.Errorf("insert event: more rows returned than events given")
		}
		if err := rows.Scan(&appended[inserted].ID, &appended[inserted].CreatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("insert event: %w", err)
		}
		inserted++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("insert event: %w", err)
	}
	if inserted != len(appended) {
		return nil, fmt.Errorf("insert event: %d rows returned for %d events", inserted, len(appended))
	}
	// The ask.opened payloads that name their own event's id, then each owner's row advancing once
	// to its last event, in the order the owners were locked, go in one pipeline: each statement's
	// error is wrapped as it would be sent alone, and the batch reports the first to fail.
	var batch pgx.Batch
	for index := range appended {
		e := &appended[index]
		if e.Type != "ask.opened" {
			continue
		}
		opened, ok := e.Payload.(model.AskEventPayload)
		if !ok {
			return nil, fmt.Errorf("ask.opened payload must be model.AskEventPayload")
		}
		opened.OpenedEventID = &e.ID
		payload, err := json.Marshal(opened)
		if err != nil {
			return nil, fmt.Errorf("encode ask.opened payload: %w", err)
		}
		store.QueueExec(&batch, "record ask.opened event identity", `update events set payload = $2 where id = $1`, e.ID, payload)
		e.Payload = opened
	}
	last := make(map[string]int, len(owners))
	for index, e := range appended {
		if !ownerless(e) {
			last[keys[index]] = index
		}
	}
	for _, key := range slices.Sorted(maps.Keys(last)) {
		queueOwnerSeq(&batch, appended[last[key]])
	}
	if batch.Len() > 0 {
		if err := tx.SendBatch(ctx, &batch).Close(); err != nil {
			return nil, err
		}
	}
	return appended, nil
}

// queueOwnerSeq queues into b the write recording e, its owner's last appended event, as that
// owner's last sequence.
func queueOwnerSeq(b *pgx.Batch, e model.Event) {
	switch {
	case e.IssueKey != nil:
		store.QueueExec(b, "advance issue event sequence", `
			update issues set last_seq = $2, updated_at = greatest(updated_at, $3) where key = $1
		`, *e.IssueKey, e.Seq, e.CreatedAt)
	case e.ArtifactID != nil:
		store.QueueExec(b, "advance artifact event sequence", `
			update artifacts set last_seq = $2 where id = $1
		`, *e.ArtifactID, e.Seq)
	case e.ProjectKey != nil:
		store.QueueExec(b, "advance project event sequence", `
			update projects set last_seq = $2 where key = $1
		`, *e.ProjectKey, e.Seq)
	}
}

// ownerless reports whether e names no owner row: a session-addressed event, which has no
// sequence and no row to lock.
func ownerless(e model.Event) bool {
	return e.IssueKey == nil && e.ArtifactID == nil && e.ProjectKey == nil
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
		"user_thread_state.updated", "decision.kept", "artifact.grants_changed":
		// Settings, importer and per-viewer bookkeeping: retained on their log and carried by
		// the event feed, but nobody is woken by them. A Keep records that a person read a
		// decision and agreed with it, which asks its agent for nothing. A page file's grants
		// are the same: a person decides which data files a view block's page may read, and no
		// agent acts on it.
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
