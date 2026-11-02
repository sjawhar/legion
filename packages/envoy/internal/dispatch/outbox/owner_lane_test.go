package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/session"
)

// laterEvent appends another human message on an issue routedEvent already seeded, so that an
// owner's second event commits while its first is still being sent.
func (r *roleRig) laterEvent(t *testing.T, key string, actor model.Actor) model.Event {
	t.Helper()
	return appendEvent(t, r.database, r.broker, model.Event{
		IssueKey: new(key), Type: "message.created", Actor: actor,
		Payload: model.Message{ID: uuid.NewString(), IssueKey: new(key), Body: "the later message"},
	})
}

// An owner is given back as its own sends settle, not when its batch ends: an issue read in the
// same batch as a silent holder's, whose own forward was answered, has its later event - one
// committed while the silent holder's receipt wait is still running - published at once.
//
// TestAHolderThatNeverAnswersDoesNotDelayAnotherIssuesEvents covers two owners in two batches;
// this covers two owners in one, which only a per-owner release reaches. Both issues' events are
// committed before the outbox runs, so its first batch reads both owners.
func TestAnOwnerOfTheSilentHoldersBatchIsReleasedBeforeTheBatchEnds(t *testing.T) {
	const (
		silentRole, silentHolder = "silent-reviewer", "ses_never_answers"
		liveRole, liveHolder     = "live-reviewer", "ses_answers"
		within                   = time.Second
	)
	alice := model.Actor{Kind: "user", ID: "alice"}
	rig := newRoleRig(t)
	rig.claim(t, silentRole, silentHolder, session.SessionEntry{SelfSubscribed: true})
	rig.claim(t, liveRole, liveHolder, session.SessionEntry{SelfSubscribed: true})
	silent := rig.receiver(t, silentHolder, false)
	live := rig.receiver(t, liveHolder, true)
	rig.routedEvent(t, "T-1", silentRole, alice)
	rig.routedEvent(t, "T-2", liveRole, alice)

	deps := rig.deps()
	deps.Listen = rig.database.Pool.Listener
	stop := runDeps(t, deps, 15*time.Second)
	defer stop()

	select {
	case <-silent.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the silent holder never received its forward")
	}
	select {
	case <-live.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the answering holder never received its issue's first forward")
	}

	// The silent holder's receipt wait is running, and every send of the other owner of its batch
	// has been answered. That owner's next event is committed in it.
	time.Sleep(200 * time.Millisecond)
	committed := time.Now()
	rig.laterEvent(t, "T-2", alice)
	select {
	case <-live.arrived:
		if waited := time.Since(committed); waited > within {
			t.Fatalf("the answering holder received its issue's later forward %s after its commit, want within %s: "+
				"the owner was held in flight until its batch ended", waited.Round(time.Millisecond), within)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the answering holder never received its issue's later forward")
	}
}

// One owner's order holds across the batches running side by side: while its first event waits on
// a holder that never answers, the batch keeps that owner in flight, so no batch beside it reads
// the owner's second event and the holder receives the first forward alone.
func TestAnOwnersSecondEventDoesNotOvertakeItsFirstWhileItsHolderIsSilent(t *testing.T) {
	const silentRole, silentHolder = "silent-reviewer", "ses_never_answers"
	alice := model.Actor{Kind: "user", ID: "alice"}
	rig := newRoleRig(t)
	rig.claim(t, silentRole, silentHolder, session.SessionEntry{SelfSubscribed: true})
	silent := rig.receiver(t, silentHolder, false)

	deps := rig.deps()
	deps.Listen = rig.database.Pool.Listener
	stop := runDeps(t, deps, 15*time.Second)
	defer stop()

	first := rig.routedEvent(t, "T-1", silentRole, alice)
	var forward contracts.Envelope
	select {
	case forward = <-silent.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the silent holder never received its forward")
	}
	if forward.SourceEventID != fmt.Sprint(first.ID) {
		t.Fatalf("the silent holder received event %s first, want event %d", forward.SourceEventID, first.ID)
	}

	// Committed on the same owner while the first event's receipt wait runs, and well inside
	// roleReceiptTimeout, so the first event has not been settled when the wait below ends.
	time.Sleep(200 * time.Millisecond)
	second := rig.laterEvent(t, "T-1", alice)
	select {
	case next := <-silent.arrived:
		t.Fatalf("the silent holder received event %s (its issue's second is %d) while event %d had not reached it",
			next.SourceEventID, second.ID, first.ID)
	case <-time.After(2 * time.Second):
	}
}

// backOffIn backs event id off as a failed attempt leaves it: attempts counted, and due again
// wait from now on the database's clock. attempts is what decides how long its next failure
// holds it (retry.Delay), so a test that must not read a prompt retry as its fix counts a few.
func backOffIn(t *testing.T, database *store.Store, id int64, attempts int, wait time.Duration) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update events set attempt_count = $2, next_attempt_at = now() + ($3 * interval '1 millisecond') where id = $1
	`, id, attempts, wait.Milliseconds()); err != nil {
		t.Fatalf("back event %d off: %v", id, err)
	}
}

// An event committed while its owner is in flight is read as soon as that owner is given back,
// not at the next back-off: the scan reads again once every batch whose read left owners out has
// returned.
//
// The wake the second event gives is spent on a batch that skips the owner in flight and reads
// another issue, which it sends and marks nothing of, so neither a batch that published nor one
// that read nothing tells the scan to read once more. What does is that the read left an owner
// out (batchResult.excluded). Both issues are routed to a holder that never answers, so neither
// owner is settled before its batch ends and neither release wakes the scan; with the second
// event unread, the scan's retry timer is armed to the two back-offs alone, minutes out.
func TestAnEventCommittedWhileItsOwnerIsInFlightIsReadWhenThatOwnerIsBack(t *testing.T) {
	const (
		silentRole, silentHolder = "silent-reviewer", "ses_never_answers"
		// The attempt count both events carry: their next back-off is retry.Delay(7), 64 s, so a
		// scan that waits for one misses the bound below by a minute.
		attempts   = 6
		issueTopic = "notifications.dispatch.issue.T-1.message.created"
	)
	alice := model.Actor{Kind: "user", ID: "alice"}
	rig := newRoleRig(t)
	rig.claim(t, silentRole, silentHolder, session.SessionEntry{SelfSubscribed: true})
	silent := rig.receiver(t, silentHolder, false)
	topic := subscribe(t, rig.conn, issueTopic)

	// Neither event is due when the outbox starts, so the batch that reads the first issue is the
	// one its retry timer starts, with the LISTEN connection's catch-up wake long spent and no
	// batch beside it. The second issue waits until that batch is on its receipt.
	first := rig.routedEvent(t, "T-1", silentRole, alice)
	other := rig.routedEvent(t, "T-2", silentRole, alice)
	backOffIn(t, rig.database, first.ID, attempts, time.Second)
	backOffIn(t, rig.database, other.ID, attempts, time.Hour)

	deps := rig.deps()
	deps.Listen = rig.database.Pool.Listener
	stop := runDeps(t, deps, 15*time.Second)
	defer stop()

	select {
	case <-silent.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("the silent holder never received the first issue's forward")
	}
	// The first issue is in flight on its receipt wait. The second issue falls due, which wakes
	// nothing - an update notifies no listener - and the first issue's second event commits: its
	// one wake is spent on the batch that reads the second issue and skips the owner in flight.
	dueNow(t, rig.database, other.ID)
	committed := time.Now()
	second := rig.laterEvent(t, "T-1", alice)

	// That batch waits out its own forward, so the second event's copy follows the later of the
	// two receipt timeouts, both of which began at or before this commit.
	within := roleReceiptTimeout + 3*time.Second
	deadline := time.After(within)
	for {
		select {
		case message := <-topic:
			var item contracts.Envelope
			if err := json.Unmarshal(message.Data, &item); err != nil {
				t.Fatalf("decode %s: %v", issueTopic, err)
			}
			if item.SourceEventID == fmt.Sprint(second.ID) {
				return
			}
		case <-deadline:
			t.Fatalf("%s did not carry event %d within %s of its commit (%s ago): the scan read past "+
				"its owner and then waited for a back-off",
				issueTopic, second.ID, within, time.Since(committed).Round(time.Millisecond))
		}
	}
}

// answeringTopics is a test publisher that answers each topic with the error answers holds for
// it, and every other topic with nothing. The map is written before the batch runs and only read
// while it does. It serves no Request: these batches route to no role.
type answeringTopics struct {
	publishOnly
	answers map[string]error
}

func (p *answeringTopics) Publish(item contracts.Envelope) error { return p.answers[item.Topic] }

func (p *answeringTopics) PublishAsync(item contracts.Envelope) <-chan error {
	return answered(p.Publish(item))
}

func (p *answeringTopics) WindowUntil(deadline func() time.Time) bus.Sender {
	return deadlineWindow{Sender: p, deadline: deadline}
}

// An event is settled once, so its failure is logged once. The owner here has every answer of its
// own in - one send denied, one reached - while another owner's unreachable NATS has stopped the
// batch, so the send its later event was owed is never made: that owner is left for the batch's
// end, where the connection decides what its sends came to, rather than settled now and settled
// again there.
func TestAnOwnerLeftForTheBatchsEndLogsItsFailureOnce(t *testing.T) {
	var logged lockedBuffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))

	alice := model.Actor{Kind: "user", ID: "alice"}
	writer := model.Actor{Kind: "session", ID: "writer"}
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('T-2', 'TT', 2, 'The denied issue', '{"kind":"user","id":"alice"}', 'V')
	`); err != nil {
		t.Fatalf("create the second issue: %v", err)
	}
	// The owner whose unreachable send stops the batch is read first, so it is settled first.
	unreachable := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: alice,
		Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: new("T-1"), Body: "Unreachable"},
	})
	root := seedComment(t, database, "T-2", writer, "Draft", nil)
	denied := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-2"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: "reply", IssueKey: new("T-2"), Body: "Reviewed", ReplyTo: &root,
		}},
	})
	// T-2's own topic is owed this event once the denied event's copy of it is answered, and
	// that is the send the batch's stop leaves unmade.
	appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-2"), Type: "message.created", Actor: alice,
		Payload: model.Message{ID: "c6f1bd4b-6d0e-4a53-9a2e-0c3d4fbb1b4a", IssueKey: new("T-2"), Body: "Next"},
	})

	publisher := &answeringTopics{answers: map[string]error{
		"notifications.dispatch.issue.T-1.message.created": fmt.Errorf("bus: %w", bus.ErrUnavailable),
		contracts.AgentSubject("writer"):                   fmt.Errorf("bus: %w", bus.ErrPublishDenied),
	}}
	deps := Deps{Store: database, Publisher: publisher}
	if _, err := scanBatch(context.Background(), deps, unfenced{}, holding(t, deps).instance); err != nil {
		t.Fatalf("scan the batch: %v", err)
	}

	lines := 0
	for line := range strings.SplitSeq(logged.String(), "\n") {
		if strings.Contains(line, "dispatch outbox: publish event") {
			lines++
			if !strings.Contains(line, fmt.Sprintf("event_id=%d ", denied.ID)) {
				t.Fatalf("the publish line %q names no denied event; event %d was denied", line, denied.ID)
			}
		}
	}
	if lines != 1 {
		t.Fatalf("logged %d publish lines for one attempt at event %d, want one:\n%s", lines, denied.ID, logged.String())
	}
	if attempts, failure, _ := eventState(t, database, denied.ID); attempts != 1 || failure == nil {
		t.Fatalf("event %d has attempt_count %d and publish_error %v, want the one attempt its denial counts",
			denied.ID, attempts, failure)
	}
	if attempts, _, _ := eventState(t, database, unreachable.ID); attempts != 0 {
		t.Fatalf("event %d has attempt_count %d, want it released with no attempt counted", unreachable.ID, attempts)
	}
}

// A settle that waits on the comment owner's lock neither fails the scan nor the batch running
// beside it. mentionComment.settleAbandoned opens a transaction and takes that lock before any
// attempt row, so it waits for as long as whoever holds the lock takes; the batches of one scan
// share one context, and so one mark of the pool's one-connection-per-caller rule
// (store.ErrNestedAcquire), so a second batch reading while that transaction is open would be
// refused its connection - failing that batch, and with it the scan, which ends every batch still
// running - unless both run inside the scan's statement gate (lanes.statement). The other owner's
// event is published, and the settled mention's own event with it.
func TestASettleWaitingOnTheOwnerLockDoesNotFailTheBatchBesideIt(t *testing.T) {
	ctx := store.WithTransactionTracking(context.Background())
	database := storetest.Open(t)
	publisher := &recordingPublisher{}
	deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker()}
	route := "session:s1"
	seedIssue(t, database, "T-1", &route)
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, route, created_by, rank)
		values ('T-2', 'TT', 2, 'Another owner', null, '{"kind":"user","id":"alice"}', 'V')
	`); err != nil {
		t.Fatalf("create the other owner's issue: %v", err)
	}
	alice := model.Actor{Kind: "user", ID: "alice"}
	commentID := seedComment(t, database, "T-1", alice, "@s1 are you there?", nil)
	mentioning := appendEvent(t, database, deps.Broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{
				ID: commentID, IssueKey: new("T-1"), Body: "@s1 are you there?", Mentions: mentionOf("s1"),
			},
			SuppressRoute: true, SuppressedRoute: route, SuppressedRouteSessionID: new("s1"),
		},
	})
	seedMentionDelivery(t, database, commentID, "s1", "abandoned")

	// The comment owner's lock, held as the API's own claim of that attempt holds it, so the
	// outbox's settle of the abandoned attempt waits on it.
	claim, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the transaction holding the owner's lock: %v", err)
	}
	defer claim.Rollback(context.Background())
	if _, err := claim.Exec(context.Background(), `select 1 from issues where key = 'T-1' for no key update`); err != nil {
		t.Fatalf("take the owner's lock: %v", err)
	}

	outbox := holding(t, deps)
	ended := make(chan scanEnd, 1)
	go func() { ended <- outbox.scan(ctx, unfenced{}) }()
	waitFor(t, 10*time.Second, "the settle waiting on the comment owner's lock", func() bool {
		var waiting int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock'
		`).Scan(&waiting); err != nil {
			t.Fatalf("read the lock waiters: %v", err)
		}
		return waiting > 0
	})

	// Another owner's event, which a second batch of the same scan reads while the settle waits.
	otherComment := seedComment(t, database, "T-2", alice, "a comment of another owner", nil)
	other := appendEvent(t, database, deps.Broker, model.Event{
		IssueKey: new("T-2"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: otherComment, IssueKey: new("T-2"), Body: "a comment of another owner"},
		},
	})
	outbox.signal()
	waitFor(t, 10*time.Second, "a second batch beside the settle", func() bool { return outbox.claims.Load() >= 2 })
	// Long enough for that batch to reach its read, which the gate holds back and the pool's rule
	// would refuse.
	time.Sleep(500 * time.Millisecond)
	if err := claim.Commit(context.Background()); err != nil {
		t.Fatalf("release the owner's lock: %v", err)
	}

	var end scanEnd
	select {
	case end = <-ended:
	case <-time.After(30 * time.Second):
		t.Fatal("the scan did not return")
	}
	if end.failed || end.fenced || end.natsLost {
		t.Fatalf("the scan ended %+v, want it to end with nothing failed", end)
	}
	for _, event := range []model.Event{other, mentioning} {
		if publishedAt(t, database, event.ID) == nil {
			attempts, failure, _ := eventState(t, database, event.ID)
			t.Fatalf("event %d was not published: attempt_count %d, publish_error %v", event.ID, attempts, failure)
		}
		if attempts, failure, _ := eventState(t, database, event.ID); attempts != 0 || failure != nil {
			t.Fatalf("event %d was published with attempt_count %d and publish_error %v, want neither", event.ID, attempts, failure)
		}
	}
}
