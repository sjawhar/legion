package outbox

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Reading a batch writes no row: one process publishes at a time (the publisher hold), so nothing
// is claimed, and a second read finds the same events, each with its retry state untouched.
func TestReadingABatchWritesNoRow(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	seeded := seedUnpublishedEvents(t, database, broker, 3)

	for read := range 2 {
		pending, stopped, _, err := readBatch(ctx, Deps{Store: database})
		if err != nil || stopped {
			t.Fatalf("read %d: stopped=%v err=%v", read, stopped, err)
		}
		if ids := pendingIDs(pending); !slices.Equal(ids, seeded) {
			t.Fatalf("read %d found %v, want the seeded %v", read, ids, seeded)
		}
	}
	var written int
	if err := database.Pool.QueryRow(ctx,
		`select count(*) from events where next_attempt_at is not null or attempt_count <> 0`).Scan(&written); err != nil {
		t.Fatalf("read the events' retry state: %v", err)
	}
	if written != 0 {
		t.Fatalf("%d events had their retry state written by a read", written)
	}
}

// A subscription removal is written pending until the listener removal succeeds, and the outbox
// must not publish it before then (subscriptionRemovedPayload). No read of a batch reaches it: it
// is in none of the indexes the read goes through, and holds back none of its issue's events.
func TestAPendingEventIsNeverRead(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"),
		Type:     "subscription.remove_requested",
		Actor:    model.Actor{Kind: "user", ID: "alice"},
		Payload: map[string]any{
			"session_id": "session-removed",
			"by":         model.Actor{Kind: "user", ID: "alice"},
			"topics":     []string{"notifications.dispatch.issue.T-1"},
			"pending":    true,
		},
	})
	ready := seedUnpublishedEvents(t, database, broker, 1)

	pending, stopped, _, err := readBatch(ctx, Deps{Store: database})
	if err != nil || stopped {
		t.Fatalf("read past the pending command: stopped=%v err=%v", stopped, err)
	}
	if ids := pendingIDs(pending); !slices.Equal(ids, ready) {
		t.Fatalf("the batch read %v, want only the ready event %v", ids, ready)
	}
}

// An owner's event that is not due is read beside its owner's due events, and only as a blocker:
// it is not sent, and the later event is held back from the destinations it has not reached while
// still sent to the rest. An owner with no due event is not read at all.
func TestAnEventThatIsNotDueIsReadOnlyAsABlockerOfItsOwner(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	route := "session:reviewer"
	seedIssue(t, database, "T-1", &route)
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('T-2', 'TT', 2, 'Other issue', '{"kind":"user","id":"alice"}', 'V')
	`); err != nil {
		t.Fatalf("create the other issue: %v", err)
	}
	message := func(issue, body string) model.Event {
		return appendEvent(t, database, broker, model.Event{
			IssueKey: new(issue), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
			Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: new(issue), Body: body},
		})
	}
	waiting := message("T-1", "waiting")
	otherWaiting := message("T-2", "other waiting")
	// The first event reached its issue's topic and still owes the reviewer's.
	if _, err := database.Pool.Exec(ctx, `
		update events set next_attempt_at = now() + interval '1 hour', attempt_count = 1,
		       published_destinations = array['notifications.dispatch.issue.T-1.message.created']
		where id = $1
	`, waiting.ID); err != nil {
		t.Fatalf("back the first event off: %v", err)
	}
	if _, err := database.Pool.Exec(ctx,
		`update events set next_attempt_at = now() + interval '1 hour', attempt_count = 1 where id = $1`, otherWaiting.ID); err != nil {
		t.Fatalf("back the other issue's event off: %v", err)
	}
	fresh := message("T-1", "fresh")

	pending, _, _, err := readBatch(ctx, Deps{Store: database})
	if err != nil {
		t.Fatalf("read the batch: %v", err)
	}
	if ids := pendingIDs(pending); !slices.Equal(ids, []int64{waiting.ID, fresh.ID}) {
		t.Fatalf("the batch read %v, want T-1's waiting event and its fresh one, and nothing of T-2", ids)
	}
	if pending[0].due || !pending[1].due {
		t.Fatalf("due = %v, %v, want the waiting event not due and the fresh one due", pending[0].due, pending[1].due)
	}

	publisher := &recordingPublisher{}
	holding(t, Deps{Store: database, Publisher: publisher}).scan(ctx, unfenced{})
	if got := topicsOf(publisher.all()); !slices.Equal(got, []string{"notifications.dispatch.issue.T-1.message.created"}) {
		t.Fatalf("sent %v, want the fresh event's issue topic alone: its route waits on the earlier event", got)
	}
	var waitingUntil, freshUntil time.Time
	var freshAttempts int
	if err := database.Pool.QueryRow(ctx, `
		select w.next_attempt_at, f.next_attempt_at, f.attempt_count
		from events w, events f where w.id = $1 and f.id = $2
	`, waiting.ID, fresh.ID).Scan(&waitingUntil, &freshUntil, &freshAttempts); err != nil {
		t.Fatalf("read the two events: %v", err)
	}
	if !freshUntil.Equal(waitingUntil) || freshAttempts != 0 {
		t.Fatalf("the fresh event waits until %v with %d attempts, want the earlier event's %v and none", freshUntil, freshAttempts, waitingUntil)
	}
	if destinations, _ := publishedDestinations(t, database, fresh.ID); !slices.Equal(destinations, []string{"notifications.dispatch.issue.T-1.message.created"}) {
		t.Fatalf("the fresh event recorded %v, want the issue topic it reached", destinations)
	}
}

// An owner whose first batchSize unpublished events all wait out a back-off cannot send the due
// event behind them, which the batch never reads: that event waits on the earliest of their
// back-offs, rather than leave the owner chosen, and read again, at every wake.
func TestADueEventPastItsOwnersWindowWaitsOnTheWindow(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	waiting := seedUnpublishedEvents(t, database, broker, batchSize)
	if _, err := database.Pool.Exec(ctx, `
		update events set next_attempt_at = now() + interval '1 hour' + make_interval(secs => id), attempt_count = 1
		where id = any($1)
	`, waiting); err != nil {
		t.Fatalf("back the window off: %v", err)
	}
	past := seedUnpublishedEvents(t, database, broker, 1)

	publisher := &recordingPublisher{}
	outbox := holding(t, Deps{Store: database, Publisher: publisher})
	outbox.scan(ctx, unfenced{})
	if sent := publisher.all(); len(sent) != 0 {
		t.Fatalf("sent %d envelopes, want none: the due event is past its owner's window", len(sent))
	}
	var pastUntil, earliest time.Time
	if err := database.Pool.QueryRow(ctx, `
		select (select next_attempt_at from events where id = $1),
		       (select min(next_attempt_at) from events where id = any($2))
	`, past[0], waiting).Scan(&pastUntil, &earliest); err != nil {
		t.Fatalf("read the events: %v", err)
	}
	if !pastUntil.Equal(earliest) {
		t.Fatalf("the event past the window waits until %v, want the window's earliest back-off %v", pastUntil, earliest)
	}
	if wait, ok := outbox.nextRetry(ctx, false); !ok || wait < 50*time.Minute {
		t.Fatalf("the next retry is due in %v (%v), want the window's back-off, about an hour away", wait, ok)
	}
}

func pendingIDs(pending []pendingEvent) []int64 {
	ids := make([]int64, 0, len(pending))
	for _, item := range pending {
		ids = append(ids, item.event.ID)
	}
	return ids
}

// seedUnpublishedEvents appends count unpublished message events on T-1, lowest id first. Each
// has exactly one destination - the issue's own topic, since the issue carries no route and a
// message with no reply reaches no author - so one envelope is one publication of one event.
func seedUnpublishedEvents(t *testing.T, database *store.Store, broker *events.Broker, count int) []int64 {
	t.Helper()
	ids := make([]int64, 0, count)
	for index := range count {
		event := appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"),
			Type:     "message.created",
			Actor:    model.Actor{Kind: "user", ID: "alice"},
			Payload: model.Message{
				ID: fmt.Sprintf("message-%d", index), IssueKey: new("T-1"), Body: "read me",
			},
		})
		ids = append(ids, event.ID)
	}
	return ids
}
