package outbox

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// rendezvousPublisher is one process's publisher in a two-process test: it records every event it
// is asked to publish, and holds its own first publish until the other process has started a
// publish of its own. Without that hold the two scans could run one after the other and the test
// would prove nothing about a claim; with it, each process is demonstrably mid-batch while the
// other claims. The wait has a deadline so a process that never publishes fails the assertions
// below rather than hanging the package.
type rendezvousPublisher struct {
	mu      sync.Mutex
	ids     []string
	once    sync.Once
	started chan struct{}
	peer    chan struct{}
}

// rendezvousDeadline bounds that hold: long enough that a loaded box still gets both processes
// into a batch together, short enough to fail as a test rather than as a package timeout.
const rendezvousDeadline = 10 * time.Second

func newRendezvousPair() (*rendezvousPublisher, *rendezvousPublisher) {
	first := &rendezvousPublisher{started: make(chan struct{})}
	second := &rendezvousPublisher{started: make(chan struct{})}
	first.peer, second.peer = second.started, first.started
	return first, second
}

func (p *rendezvousPublisher) Publish(item contracts.Envelope) error {
	p.mu.Lock()
	p.ids = append(p.ids, item.SourceEventID)
	p.mu.Unlock()
	p.once.Do(func() {
		close(p.started)
		select {
		case <-p.peer:
		case <-time.After(rendezvousDeadline):
		}
	})
	return nil
}

func (p *rendezvousPublisher) PublishAsync(item contracts.Envelope) <-chan error {
	return answered(p.Publish(item))
}

func (p *rendezvousPublisher) published() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.ids...)
}

// Two envoy-dispatch processes serve one Postgres and one NATS, and an event published twice is
// delivered twice on the role lanes, which carry no nats.MsgId. scanPendingEvents claims each row
// before it publishes it, so this drives two processes - each on its own store, as each process
// has its own pool - over one backlog at once: every seeded event is published exactly once, by
// exactly one of them, and every row ends marked published with its claim cleared.
func TestTwoProcessesPublishEveryEventExactlyOnce(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	// Two full batches, so neither process can take the whole backlog in one claim.
	seeded := seedUnpublishedEvents(t, database, broker, 2*batchSize)

	first, other := newRendezvousPair()
	start := make(chan struct{})
	var scanning sync.WaitGroup
	for _, process := range []struct {
		database  *store.Store
		publisher *rendezvousPublisher
	}{{database, first}, {second, other}} {
		scanning.Add(1)
		go func() {
			defer scanning.Done()
			<-start
			scan(ctx, Deps{Store: process.database, Publisher: process.publisher, Broker: broker})
		}()
	}
	close(start)
	scanning.Wait()

	mine, theirs := first.published(), other.published()
	if len(mine) == 0 || len(theirs) == 0 {
		t.Fatalf("the two processes published %d and %d events: one of them did no work at all", len(mine), len(theirs))
	}
	times := map[string]int{}
	for _, id := range append(append([]string(nil), mine...), theirs...) {
		times[id]++
	}
	for _, id := range seeded {
		if count := times[fmt.Sprint(id)]; count != 1 {
			t.Fatalf("event %d was published %d times across the two processes, want exactly once", id, count)
		}
	}
	if len(times) != len(seeded) {
		t.Fatalf("the processes published %d distinct events, want the %d seeded", len(times), len(seeded))
	}
	taken := map[string]struct{}{}
	for _, id := range mine {
		taken[id] = struct{}{}
	}
	for _, id := range theirs {
		if _, both := taken[id]; both {
			t.Fatalf("both processes published event %s", id)
		}
	}

	var unpublished, claimed, retried int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) filter (where published_at is null),
		       count(*) filter (where next_attempt_at is not null),
		       count(*) filter (where attempt_count <> 0)
		from events
	`).Scan(&unpublished, &claimed, &retried); err != nil {
		t.Fatalf("read the events' final state: %v", err)
	}
	if unpublished != 0 || claimed != 0 || retried != 0 {
		t.Fatalf("after both scans: %d events unpublished, %d still claimed, %d backed off, want 0, 0 and 0",
			unpublished, claimed, retried)
	}
}

// A claimed event is the process that claimed it to publish: no other scan reads it again until
// the claim lapses. The lapse is made by moving next_attempt_at back, the way embedqueue's own
// claim tests do, rather than by waiting out claimWindow's minute.
func TestAClaimedEventIsReadAgainOnlyOnceItsClaimLapses(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	seeded := seedUnpublishedEvents(t, database, broker, 1)
	before := time.Now()

	claimed, blocked, err := scanPendingEvents(ctx, Deps{Store: database})
	if err != nil || blocked {
		t.Fatalf("claim the pending event: blocked=%v err=%v", blocked, err)
	}
	if len(claimed) != 1 || claimed[0].event.ID != seeded[0] {
		t.Fatalf("first process claimed %d events, want event %d alone", len(claimed), seeded[0])
	}

	held, _, err := scanPendingEvents(ctx, Deps{Store: second})
	if err != nil {
		t.Fatalf("second process's scan: %v", err)
	}
	if len(held) != 0 {
		t.Fatalf("second process read %d of the first's claimed events, want none", len(held))
	}
	var nextAttempt time.Time
	if err := database.Pool.QueryRow(ctx,
		`select next_attempt_at from events where id = $1`, seeded[0]).Scan(&nextAttempt); err != nil {
		t.Fatalf("read the claimed event's next_attempt_at: %v", err)
	}
	if nextAttempt.Before(before.Add(claimWindow - time.Second)) {
		t.Fatalf("claimed next_attempt_at = %v, want at least claimWindow (%v) past %v", nextAttempt, claimWindow, before)
	}

	// What a crashed claimer leaves behind once its window passes.
	if _, err := database.Pool.Exec(ctx,
		`update events set next_attempt_at = now() - interval '1 second' where id = $1`, seeded[0]); err != nil {
		t.Fatalf("lapse the claim: %v", err)
	}
	lapsed, _, err := scanPendingEvents(ctx, Deps{Store: second})
	if err != nil {
		t.Fatalf("second process's scan after the claim lapsed: %v", err)
	}
	if len(lapsed) != 1 || lapsed[0].event.ID != seeded[0] {
		t.Fatalf("after the claim lapsed the second process read %d events, want the one the first never published", len(lapsed))
	}
}

// A subscription removal is written pending until the listener removal succeeds, and the outbox
// must not publish it before then (subscriptionRemovedPayload). The claim must not reach it
// either: a claim moves next_attempt_at, which is the row's retry schedule, and a row the scan
// may never publish is a row the scan may never touch.
func TestAPendingEventIsNeverClaimed(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	pending := appendEvent(t, database, broker, model.Event{
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

	claimed, blocked, err := scanPendingEvents(ctx, Deps{Store: database})
	if err != nil || blocked {
		t.Fatalf("claim past the pending command: blocked=%v err=%v", blocked, err)
	}
	if len(claimed) != 1 || claimed[0].event.ID != ready[0] {
		t.Fatalf("scan claimed %d events, want only the ready event %d", len(claimed), ready[0])
	}
	var nextAttempt *time.Time
	if err := database.Pool.QueryRow(ctx,
		`select next_attempt_at from events where id = $1`, pending.ID).Scan(&nextAttempt); err != nil {
		t.Fatalf("read the pending command's retry state: %v", err)
	}
	if nextAttempt != nil {
		t.Fatalf("the claim moved the pending command's next_attempt_at to %v, want it untouched", *nextAttempt)
	}
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
				ID: fmt.Sprintf("message-%d", index), IssueKey: new("T-1"), Body: "claim me",
			},
		})
		ids = append(ids, event.ID)
	}
	return ids
}
