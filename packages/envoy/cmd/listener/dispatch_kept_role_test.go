package main

import (
	"context"
	"fmt"
	"testing"
	"time"

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

// outboxRoleEvent seeds an issue routed to role and appends one event on it, as Dispatch's API
// does, and returns the event.
func outboxRoleEvent(t *testing.T, database *dispatchstore.Store, broker *events.Broker, key, role string) model.Event {
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
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the event's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	event, err := broker.Append(ctx, tx, model.Event{
		IssueKey: new(key), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "3b2f4a6d-7c18-4a52-9e0b-1d5c7a9f2e34", IssueKey: new(key), Body: "for whoever takes the role"},
	})
	if err != nil {
		t.Fatalf("append the event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the event: %v", err)
	}
	return event
}

// runOutbox runs Dispatch's outbox over database and the listener's NATS client until the test
// ends, as cmd/dispatch runs it: role routes resolved through the role and session buckets on
// that client.
func runOutbox(t *testing.T, harness listenerDeliveryHarness, database *dispatchstore.Store, broker *events.Broker) {
	t.Helper()
	holders := roles.NewBuckets(harness.client)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		outbox.New(outbox.Deps{
			Store: database, Publisher: harness.client, Broker: broker, Listen: database.Pool.Listener,
			RoleClaims: holders, Sessions: holders,
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
	harness := newListenerDeliveryHarness(t, nil)
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
