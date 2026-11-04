package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
	dispatchstore "github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/roles"
)

// This file is the one proof that a role message Dispatch's outbox could not deliver reaches the
// role's next holder: Dispatch's own outbox keeps the copy, over one real NATS and one real
// Postgres, and the listener's drain - which reads a kept lane by stream sequence and not by who
// wrote it - hands that copy to the session that claims the role afterwards.

// outboxRoleIssue seeds the project and an issue routed to role, as Dispatch's API writes them.
func outboxRoleIssue(t *testing.T, database *dispatchstore.Store, key, role string) {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name) values ('TT', 'Test') on conflict do nothing`); err != nil {
		t.Fatalf("create the project: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, route, created_by, rank)
		values ($1, 'TT', 1, 'Routed to a role nobody holds', $2, '{"kind":"user","id":"alice"}', 'U')
	`, key, "role:"+role); err != nil {
		t.Fatalf("create the issue: %v", err)
	}
}

// outboxRoleEvent seeds an issue routed to role and appends one event on it, as Dispatch's API
// does, and returns the event.
func outboxRoleEvent(t *testing.T, database *dispatchstore.Store, broker *events.Broker, key, role string) model.Event {
	t.Helper()
	outboxRoleIssue(t, database, key, role)
	return outboxIssueEvent(t, database, broker, key, "3b2f4a6d-7c18-4a52-9e0b-1d5c7a9f2e34", "for whoever takes the role")
}

// outboxIssueEvent appends one message event on the issue key names, committed as Dispatch's API
// commits it, so the outbox reads it on its LISTEN wake.
func outboxIssueEvent(t *testing.T, database *dispatchstore.Store, broker *events.Broker, key, messageID, body string) model.Event {
	t.Helper()
	return outboxEvent(t, database, broker, model.Event{
		IssueKey: new(key), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: messageID, IssueKey: new(key), Body: body},
	})
}

// outboxEvent appends event, committed as Dispatch's API commits it, so the outbox reads it on
// its LISTEN wake, and returns it as the broker recorded it.
func outboxEvent(t *testing.T, database *dispatchstore.Store, broker *events.Broker, event model.Event) model.Event {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the event's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	appended, err := broker.Append(ctx, tx, event)
	if err != nil {
		t.Fatalf("append the event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the event: %v", err)
	}
	return appended
}

// runOutbox runs Dispatch's outbox over database and the listener's NATS client until the test
// ends, as cmd/dispatch runs it: role routes resolved through the role and session buckets on
// that client, and the kept lanes read through it too.
func runOutbox(t *testing.T, harness listenerDeliveryHarness, database *dispatchstore.Store, broker *events.Broker) {
	t.Helper()
	runOutboxThrough(t, harness, database, broker, harness.client)
}

// runOutboxThrough is runOutbox publishing through publisher, for a test that takes a hand in
// what one of the outbox's own publishes does (keptUnderAClaim).
func runOutboxThrough(t *testing.T, harness listenerDeliveryHarness, database *dispatchstore.Store, broker *events.Broker, publisher outbox.Publisher) {
	t.Helper()
	holders := roles.NewBuckets(harness.client)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		outbox.New(outbox.Deps{
			Store: database, Publisher: publisher, Broker: broker, Listen: database.Pool.Listener,
			RoleClaims: holders, Sessions: holders, KeptLanes: harness.client,
		}).Run(ctx)
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Error("Dispatch's outbox did not stop")
		}
	})
}

// publishedEventAt is when the outbox marked event published, and nil while it has not.
func publishedEventAt(t *testing.T, database *dispatchstore.Store, eventID int64) *time.Time {
	t.Helper()
	var published *time.Time
	if err := database.Pool.QueryRow(context.Background(),
		`select published_at from events where id = $1`, eventID).Scan(&published); err != nil {
		t.Fatalf("read event %d: %v", eventID, err)
	}
	return published
}

// An event Dispatch routes to a role no session holds reaches the role's next holder, once: the
// outbox keeps the copy on that role's kept lane rather than counting the route reached with
// nothing sent, and the claim that follows hands it to the claimant, which is the one path such
// an event has to a session. The copy carries the envelope Dispatch published, on its own role
// topic, so the holder is handed the message as Dispatch sent it.
func TestAnEventDispatchRoutedToAnUnheldRoleReachesTheRolesNextHolder(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const (
		role   = "kept-from-the-outbox"
		holder = "ses_kept_from_the_outbox"
		key    = "T-1"
	)
	lane := newKeptLaneUnderClaim(t, harness, func(int, string, contracts.Envelope) error { return nil })
	database := migratedDispatchDatabase(t)
	broker := events.NewBroker()
	event := outboxRoleEvent(t, database, broker, key, role)
	runOutbox(t, harness, database, broker)

	waitFor(t, 30*time.Second, "Dispatch's outbox to publish the event of an unheld role", func() bool {
		return publishedEventAt(t, database, event.ID) != nil
	})
	kept := waitForKeptRoleMessage(t, harness.client, role, fmt.Sprintf("dispatch-%d", event.ID))
	if kept.Topic != contracts.RoleTopicPrefix+role {
		t.Fatalf("the kept copy's topic is %q, want the role topic Dispatch published it on", kept.Topic)
	}

	lane.register(t, holder)
	lane.claim(t, holder, role)
	handed := lane.await(t, 1, 30*time.Second)

	if handed[0].holder != holder {
		t.Fatalf("the copy reached %s, want the role's next holder %s", handed[0].holder, holder)
	}
	if want := roleForwardDedupePrefix + kept.DedupeKey; handed[0].key != want {
		t.Fatalf("the copy was forwarded under %q, want %q", handed[0].key, want)
	}
	// The lane is walked again, for the same holder, so a copy handed over twice would show here:
	// the cursor the first walk wrote is what keeps the handover to exactly one.
	if err := drainKeptRole(context.Background(), *lane.deps.delivery, role, time.Now); err != nil {
		t.Fatalf("walk role %s's kept lane again: %v", role, err)
	}
	if keys := lane.keys(); len(keys) != 1 {
		t.Fatalf("the drain forwarded %d copies, want the one Dispatch kept: %v", len(keys), keys)
	}
}

// outboxHeldRoleComment writes a comment on the issue key names that mentions sessionID - the
// session the issue's route resolved to when the comment was written, so the comment suppresses
// that route for it - with the attempt its sender died with in flight: pending under a claim
// that lapsed, which Dispatch's delivery sweep resends and the outbox waits no longer on
// (model.MentionResumedSQL), so the outbox holds the route's copy as a comment_fallback_copies
// row rather than publishing it. It appends the comment.created event as Dispatch's API appends
// it, and returns the comment's id, that event and the dedupe key the mention's send goes under,
// which is the key the held copy carries (contracts.AgentSendDedupeKey).
func outboxHeldRoleComment(t *testing.T, database *dispatchstore.Store, broker *events.Broker, key, role, sessionID string) (string, model.Event, string) {
	t.Helper()
	ctx := context.Background()
	body := "@" + sessionID + " are you there?"
	var commentID string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body)
		values ($1, '{"kind":"user","id":"alice"}', $2) returning id::text
	`, key, body).Scan(&commentID); err != nil {
		t.Fatalf("create the comment: %v", err)
	}
	target := "session:" + sessionID
	if _, err := database.Pool.Exec(ctx, `
		insert into comment_mentions (comment_id, target, delivery) values ($1, $2, 'steer')
	`, commentID, target); err != nil {
		t.Fatalf("create the mention of %s: %v", target, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state, created_at, claimed_at)
		values ($1, $2, 1, 'steer', $3, 'pending', now() - interval '2 minutes', now() - interval '2 minutes')
	`, commentID, target, sessionID); err != nil {
		t.Fatalf("create the mention's attempt: %v", err)
	}
	event := outboxEvent(t, database, broker, model.Event{
		IssueKey: new(key), Type: "comment.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.CommentEventPayload{
			Comment: model.Comment{
				ID: commentID, IssueKey: new(key), Body: body,
				Mentions: []model.Mention{{Target: target, Delivery: "steer", SessionID: new(sessionID)}},
			},
			SuppressRoute: true, SuppressedRoute: "role:" + role, SuppressedRouteSessionID: new(sessionID),
		},
	})
	return commentID, event, contracts.AgentSendDedupeKey(sessionID, model.CommentMentionIdempotencyKey(commentID, target, "steer"))
}

// heldFallbackCopies is how many comment_fallback_copies rows the comment has at destination.
func heldFallbackCopies(t *testing.T, database *dispatchstore.Store, commentID, destination string) int {
	t.Helper()
	var rows int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from comment_fallback_copies where comment_id = $1 and destination = $2
	`, commentID, destination).Scan(&rows); err != nil {
		t.Fatalf("read the comment's held copies: %v", err)
	}
	return rows
}

// A copy Dispatch held for a role route, whose mention of a session failed, reaches that very
// session through the listener's drain under the mention's own key, with no role forward's
// prefix. The outbox's fallback pass finds the role unclaimed and keeps the copy on that role's
// kept lane, under the key of the failed mention of the session the route resolved to
// (publishFallbackCopies, roleForward.keep); the session that claims the role next is that same
// session, and the host it runs on claimed that key when the mention's send landed before the
// attempt failed. So the drain forwards the copy to it as the pass's own forward would have
// done, under that key alone (contracts.RoleForwardDedupeKey, roleForward.forwarded): the host
// drops it and the session is shown the comment once, where the prefix - a key no host has seen
// - would show it the comment a second time.
//
// The forward's Nats-Msg-Id is what makes the two writers' copies one at the stream: Dispatch's
// outbox forwards a kept copy of its own behind a keep (handOverKept) and keys it by the same
// rule, so both name the copy bus.ForwardMsgID of that key and the holder's agent subject, and
// the stream stores one copy of it for that holder however often it is forwarded.
//
// TestAnEventDispatchRoutedToAnUnheldRoleReachesTheRolesNextHolder is the other side of the
// rule: a copy whose key names no session keeps the prefix, whoever its next holder is.
func TestAKeptCopyOfAFailedMentionReachesThatSessionUnderTheMentionsKey(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const (
		role   = "kept-for-the-mentioned-session"
		holder = "ses_mentioned_by_the_held_comment"
		key    = "T-1"
	)
	roleTopic := contracts.RoleTopicPrefix + role
	lane := newKeptLaneUnderClaim(t, harness, func(int, string, contracts.Envelope) error { return nil })
	database := migratedDispatchDatabase(t)
	broker := events.NewBroker()
	outboxRoleIssue(t, database, key, role)
	commentID, event, mentionKey := outboxHeldRoleComment(t, database, broker, key, role, holder)
	runOutbox(t, harness, database, broker)

	// The outbox publishes the comment's event with the route's copy held back for the mention
	// still being resent; the resend then fails for good, so the next fallback pass publishes
	// that copy, which for a role route means keeping it for the role's next holder.
	waitFor(t, 30*time.Second, "Dispatch's outbox to hold the route's copy of the comment", func() bool {
		return heldFallbackCopies(t, database, commentID, roleTopic) == 1
	})
	if _, err := database.Pool.Exec(context.Background(),
		`update comment_deliveries set state = 'failed' where comment_id = $1`, commentID); err != nil {
		t.Fatalf("fail the mention's resend: %v", err)
	}
	var kept contracts.Envelope
	waitFor(t, 30*time.Second, "the fallback pass to keep the held copy for the role's next holder", func() bool {
		item, ok := readKeptRoleMessage(t, harness.client, role)
		kept = item
		return ok && item.DedupeKey == mentionKey
	})
	if kept.Topic != roleTopic || kept.EventID != fmt.Sprintf("dispatch-%d", event.ID) {
		t.Fatalf("the kept copy is event %q on topic %q, want the comment's event on the role topic Dispatch held it for",
			kept.EventID, kept.Topic)
	}

	lane.register(t, holder)
	lane.claim(t, holder, role)
	handed := lane.await(t, 1, 30*time.Second)

	if handed[0].holder != holder {
		t.Fatalf("the copy reached %s, want the mentioned session %s, which claimed the role", handed[0].holder, holder)
	}
	if handed[0].key != mentionKey {
		t.Fatalf("the copy was forwarded under %q, want the mention's own key %q, which that session's host drops",
			handed[0].key, mentionKey)
	}
	if strings.HasPrefix(handed[0].key, roleForwardDedupePrefix) {
		t.Fatalf("the copy forwarded to the session its own mention names carries %q, want no role forward's prefix",
			handed[0].key)
	}
	subject := contracts.AgentSubject(holder)
	if want := bus.ForwardMsgID(mentionKey, subject); handed[0].msgID != want {
		t.Fatalf("the forward was made under %q, want %q: the outbox's own handover of a kept copy names it that, and the stream stores the two as one",
			handed[0].msgID, want)
	}
}

// keptUnderAClaim is the listener's own NATS client with one hand in the outbox's keeps: claim is
// run before the nth kept publish the outbox makes and before nothing else, so a claim lands in
// the window the keep's own race leaves open - after the batch resolved the role unheld, and
// before the copy is on its lane (internal/dispatch/outbox/role.go, handOverKept). Every other
// send is the client's own.
type keptUnderAClaim struct {
	*bus.Client
	nth   int32
	keeps atomic.Int32
	claim func()
}

func (p *keptUnderAClaim) WindowUntil(deadline func() time.Time) bus.Sender {
	return keptUnderAClaimWindow{Sender: p.Client.WindowUntil(deadline), publisher: p}
}

type keptUnderAClaimWindow struct {
	bus.Sender
	publisher *keptUnderAClaim
}

func (w keptUnderAClaimWindow) PublishStoredAsync(subject string, item contracts.Envelope, msgID string) <-chan error {
	if w.publisher.keeps.Add(1) == w.publisher.nth {
		w.publisher.claim()
	}
	return w.Sender.PublishStoredAsync(subject, item, msgID)
}

// answeredForwards subscribes to sessionID's agent subject as a self-subscribed session does and
// answers every forward with the empty receipt the sender waits for, reporting each envelope it
// took.
func answeredForwards(t *testing.T, harness listenerDeliveryHarness, sessionID string) <-chan contracts.Envelope {
	t.Helper()
	taken := make(chan contracts.Envelope, 16)
	sub, err := harness.client.Conn.Subscribe(contracts.AgentSubject(sessionID), func(msg *natsgo.Msg) {
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err != nil {
			return
		}
		taken <- item
		if msg.Reply != "" {
			_ = msg.Respond(nil)
		}
	})
	if err != nil {
		t.Fatalf("subscribe to %s's agent subject: %v", sessionID, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	return taken
}

// keptRoleCopies is every copy the stream holds on role's kept lane, in the order a drain of
// that lane walks them: from the lane's first sequence on, one NextStoredMessage per copy, which
// is the read the walk itself makes (walkKeptRole). A lane's order is the order its copies reach
// the role's next holder in, so a test about what the two writers of a lane leave on it reads
// the whole of it rather than its last copy (readKeptRoleMessage).
func keptRoleCopies(t *testing.T, client *bus.Client, role string) []contracts.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var copies []contracts.Envelope
	for seq := uint64(1); ; {
		stored, err := client.NextStoredMessage(ctx, bus.KeptRoleSubject(role), seq)
		if errors.Is(err, natsgo.ErrMsgNotFound) {
			return copies
		}
		if err != nil {
			t.Fatalf("read role %s's kept lane after %d: %v", role, seq, err)
		}
		var item contracts.Envelope
		if err := json.Unmarshal(stored.Data, &item); err != nil {
			t.Fatalf("decode role %s's kept copy at %d: %v", role, stored.Sequence, err)
		}
		copies = append(copies, item)
		seq = stored.Sequence + 1
	}
}

// A copy Dispatch's outbox keeps on a lane the listener's drain has already walked is handed over
// past the cursor that drain wrote. The two writers of a role's kept lane read one cursor body
// (bus.KeptRoleCursor, bus.Client.ReadKeptRoleCursor), and this is where the agreement is
// exercised rather than asserted in a comment: the listener's drain writes the cursor, over one
// real NATS, and the outbox's own handover reads it, finds the copy it has just kept to be the
// first past it, and forwards it to the session holding the role.
//
// A cursor the outbox read as zero - a tag that drifted, a body of its own - would find the
// drain's own first copy at the head of the lane instead, whose dedupe key is another's, and the
// handover would decline and leave this copy to the listener's sweep, minutes later. The claim
// here is made on the buckets rather than through POST /v1/roles/set, so no drain of this
// listener's starts and the outbox's handover is the one path the copy has to the holder.
func TestTheOutboxHandsACopyOverPastTheCursorTheListenersDrainWrote(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const (
		role   = "kept-past-the-drains-cursor"
		holder = "ses_past_the_drains_cursor"
		key    = "T-1"
	)
	lane := newKeptLaneUnderClaim(t, harness, func(int, string, contracts.Envelope) error { return nil })
	taken := answeredForwards(t, harness, holder)
	database := migratedDispatchDatabase(t)
	broker := events.NewBroker()
	ctx := context.Background()

	// The role is claimed again under the second keep the outbox makes, on the buckets alone, as
	// a claim that lands between the batch's resolution of the role and its keep does.
	publisher := &keptUnderAClaim{Client: harness.client, nth: 2, claim: func() {
		lane.register(t, holder)
		if _, err := harness.registry.SetRole(holder, "test-machine", role, false); err != nil {
			t.Errorf("claim %s for %s under the keep: %v", role, holder, err)
		}
	}}

	// The first event is kept while nobody holds the role, and the claim that follows hands it
	// over: that walk is what writes the role's drain cursor.
	first := outboxRoleEvent(t, database, broker, key, role)
	runOutboxThrough(t, harness, database, broker, publisher)
	waitFor(t, 30*time.Second, "Dispatch's outbox to publish the first event of an unheld role", func() bool {
		return publishedEventAt(t, database, first.ID) != nil
	})
	firstKept := waitForKeptRoleMessage(t, harness.client, role, fmt.Sprintf("dispatch-%d", first.ID))
	lane.register(t, holder)
	lane.claim(t, holder, role)
	lane.await(t, 1, 30*time.Second)
	var drained bus.StoredKeptRoleCursor
	waitFor(t, 30*time.Second, "the listener's drain to write role "+role+"'s cursor", func() bool {
		var err error
		drained, err = harness.client.ReadKeptRoleCursor(ctx, role)
		if err != nil {
			t.Fatalf("read role %s's drain cursor: %v", role, err)
		}
		return drained.Cursor.Kept > 0 && drained.Cursor.Claiming == 0
	})

	// The role is given up again, so the outbox resolves it unheld and keeps the issue's next
	// event rather than forwarding it, and the claim comes back under that keep.
	if err := harness.registry.Remove(ctx, holder, []string{contracts.RoleTopicPrefix + role}); err != nil {
		t.Fatalf("release %s's claim of %s: %v", holder, role, err)
	}
	second := outboxIssueEvent(t, database, broker, key, "9f2c6e18-40b7-4d35-8a91-5e07c3b8df42", "the issue's next event")

	waitFor(t, 30*time.Second, "Dispatch's outbox to publish the issue's next event", func() bool {
		return publishedEventAt(t, database, second.ID) != nil
	})
	secondKey := fmt.Sprintf("dispatch-%d", second.ID)
	var handed contracts.Envelope
	select {
	case handed = <-taken:
	case <-time.After(30 * time.Second):
		t.Fatalf("the holder was handed nothing on its agent subject within 30s; the listener log says what the handover read:\n%s", harness.logs.String())
	}
	if handed.EventID != secondKey || handed.DedupeKey != roleForwardDedupePrefix+secondKey {
		t.Fatalf("the holder was handed event %q under dedupe key %q, want the role forward of the kept %q",
			handed.EventID, handed.DedupeKey, secondKey)
	}
	if handed.KeptAt == nil {
		t.Fatal("the copy reached the holder with no kept_at, want the time the outbox kept it")
	}
	// The handover reads the cursor and leaves it alone: the drain stays its only writer, and
	// both copies stay on the lane for a holder that claims later.
	after, err := harness.client.ReadKeptRoleCursor(ctx, role)
	if err != nil {
		t.Fatalf("read role %s's drain cursor after the handover: %v", role, err)
	}
	if after != drained {
		t.Fatalf("role %s's drain cursor is %+v after the handover, want the %+v the drain left: the cursor is the listener's to write",
			role, after, drained)
	}
	// The lane itself: the copy the drain walked, and behind it the one the handover forwarded,
	// each the envelope as its sender sent it and neither carrying the time of its keep, which
	// the handover stamps on the forward alone. The order is what a session claiming the role
	// after this one would be handed them in.
	waitForKeptRoleMessage(t, harness.client, role, secondKey)
	onTheLane := keptRoleCopies(t, harness.client, role)
	if len(onTheLane) != 2 ||
		onTheLane[0].EventID != firstKept.EventID || onTheLane[0].DedupeKey != firstKept.DedupeKey || onTheLane[0].KeptAt != nil ||
		onTheLane[1].EventID != secondKey || onTheLane[1].DedupeKey != secondKey || onTheLane[1].KeptAt != nil {
		t.Fatalf("role %s's kept lane holds %+v, want the copy the drain walked (%s) and then the copy the outbox kept for %s, each as its sender sent it with no kept_at of its own",
			role, onTheLane, firstKept.EventID, secondKey)
	}
	// Only the first copy went through the listener's drain: nothing walked the lane for the
	// second, so the handover is the one path it took.
	if keys := lane.keys(); len(keys) != 1 || keys[0] != roleForwardDedupePrefix+firstKept.DedupeKey {
		t.Fatalf("the listener's drain forwarded %v, want the first copy alone: the second reached the holder from the outbox's own handover", keys)
	}
}
