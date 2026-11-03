package outbox

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// heldPublisher answers no publish until the test answers it, as NATS answers a publish a round
// trip after it was sent.
type heldPublisher struct {
	publishOnly
	mu      sync.Mutex
	topics  []string
	answers []chan error
}

func (p *heldPublisher) WindowUntil(deadline func() time.Time) bus.Sender {
	return deadlineWindow{Sender: p, deadline: deadline}
}

func (p *heldPublisher) PublishAsync(item contracts.Envelope) <-chan error {
	p.mu.Lock()
	defer p.mu.Unlock()
	answer := make(chan error, 1)
	p.topics = append(p.topics, item.Topic)
	p.answers = append(p.answers, answer)
	return answer
}

// Request holds a role forward as PublishAsync holds a publish, by its envelope's topic, the role's.
func (p *heldPublisher) Request(_ string, item contracts.Envelope, _ bus.RequestOptions) <-chan error {
	return p.PublishAsync(item)
}

func (p *heldPublisher) sent() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.topics)
}

// accept answers the publishes numbered from through to-1, in send order, as accepted.
func (p *heldPublisher) accept(from, to int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, answer := range p.answers[from:to] {
		answer <- nil
	}
}

// An issue's events go to each destination one at a time, in id order: the second event is sent
// to the issue's topic only once NATS answered the first event's send there, and to the role route
// only once the first event's role send was answered, while the first event's three destinations
// go out side by side. The issue's events are recorded once every answer of that owner is in, and
// an event is marked published only then, with its destinations in the order it has them.
func TestAnIssuesEventsGoToEachDestinationOneAtATime(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	first := seedThreeDestinationEvent(t, database, broker)
	second := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.MessageEventPayload{Message: model.Message{ID: uuid.NewString(), IssueKey: new("T-1"), Body: "Next"}},
	})
	publisher := &heldPublisher{}
	stop := run(t, database, publisher, broker)
	defer stop()

	firstTopics := []string{"notifications.dispatch.issue.T-1.comment.created", roleTopic, "notifications.agent.writer"}
	waitFor(t, 5*time.Second, "the first event's destinations sent", func() bool { return len(publisher.sent()) == 3 })
	time.Sleep(200 * time.Millisecond)
	if got := publisher.sent(); !slices.Equal(got, firstTopics) {
		t.Fatalf("sent %v before any answer, want only the first event's destinations %v", got, firstTopics)
	}

	publisher.accept(0, 1)
	waitFor(t, 5*time.Second, "the second event's topic sent once the first's was answered", func() bool { return len(publisher.sent()) == 4 })
	time.Sleep(200 * time.Millisecond)
	if got := publisher.sent(); len(got) != 4 || got[3] != "notifications.dispatch.issue.T-1.message.created" {
		t.Fatalf("sent %v, want the second event's issue topic next, and its role route still held", got)
	}
	publisher.accept(1, 3)
	waitFor(t, 5*time.Second, "the second event's role route sent", func() bool { return len(publisher.sent()) == 5 })
	if got := publisher.sent()[4]; got != roleTopic {
		t.Fatalf("the fifth send went to %s, want the role route", got)
	}
	for _, event := range []model.Event{first, second} {
		if publishedAt(t, database, event.ID) != nil {
			t.Fatalf("event %d was marked published with an answer of its batch still to come", event.ID)
		}
		if destinations, attempts := publishedDestinations(t, database, event.ID); len(destinations) != 0 || attempts != 0 {
			t.Fatalf("event %d recorded %v after %d attempts with an answer of its batch still to come", event.ID, destinations, attempts)
		}
	}

	publisher.accept(3, 5)
	waitFor(t, 5*time.Second, "both events marked published", func() bool {
		return publishedAt(t, database, first.ID) != nil && publishedAt(t, database, second.ID) != nil
	})
	if destinations, _ := publishedDestinations(t, database, first.ID); !slices.Equal(destinations, firstTopics) {
		t.Fatalf("published_destinations = %v, want %v", destinations, firstTopics)
	}
}

// An outbox that stops with a batch's answers still coming records what NATS already accepted and
// releases the rest: the event whose every destination was answered is marked published, and the
// one still waiting is neither published nor backed off, so it is sent again by the next holder.
func TestAnOutboxStoppedMidBatchMarksTheAnsweredAndReleasesTheRest(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	first := seedThreeDestinationEvent(t, database, broker)
	second := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.MessageEventPayload{Message: model.Message{ID: uuid.NewString(), IssueKey: new("T-1"), Body: "Next"}},
	})
	publisher := &heldPublisher{}
	stop := run(t, database, publisher, broker)
	waitFor(t, 5*time.Second, "the first event sent", func() bool { return len(publisher.sent()) == 3 })
	publisher.accept(0, 3)
	waitFor(t, 5*time.Second, "the second event sent", func() bool { return len(publisher.sent()) == 5 })
	time.Sleep(100 * time.Millisecond)
	stop()

	if publishedAt(t, database, first.ID) == nil {
		t.Fatal("the event NATS answered in full was not marked published when the outbox stopped")
	}
	if publishedAt(t, database, second.ID) != nil {
		t.Fatal("the event still waiting for its answers was marked published")
	}
	var attempts int
	var nextAttempt *time.Time
	if err := database.Pool.QueryRow(context.Background(),
		`select attempt_count, next_attempt_at from events where id = $1`, second.ID).Scan(&attempts, &nextAttempt); err != nil {
		t.Fatalf("read the unanswered event: %v", err)
	}
	if attempts != 0 || nextAttempt != nil {
		t.Fatalf("the unanswered event has attempt_count %d and next_attempt_at %v, want it released: 0 and none", attempts, nextAttempt)
	}
}

// destinationFamily names the destination key a stream subject belongs to, as the outbox orders
// sends on it: an issue's subjects are its topic family, and every other subject is its own.
func destinationFamily(subject string) string {
	if rest, ok := strings.CutPrefix(subject, "notifications.dispatch.issue."); ok {
		issue, _, _ := strings.Cut(rest, ".")
		return "issue " + issue
	}
	return subject
}

// assertStreamInOrderPerDestination fails t unless the stream holds want messages, and each
// destination's - each issue's topic family, each session's own subject - in event id order.
func assertStreamInOrderPerDestination(t *testing.T, url string, want int) {
	t.Helper()
	reader, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect the reader: %v", err)
	}
	defer reader.Close()
	bySubject, all := streamOrder(t, reader)
	if len(all) != want {
		t.Fatalf("the stream holds %d messages, want %d, one per destination", len(all), want)
	}
	byFamily := map[string][]int64{}
	for subject, ids := range bySubject {
		byFamily[destinationFamily(subject)] = append(byFamily[destinationFamily(subject)], ids...)
	}
	for family, ids := range byFamily {
		if at := firstDescent(ids); at >= 0 {
			t.Fatalf("%s holds event %d after event %d: %v", family, ids[at], ids[at-1], ids)
		}
	}
}

// A NATS outage holds the backlog back without reordering it. An event the outbox could not send
// because NATS was unreachable is not the event's failure, so it is released, not backed off
// behind the events after it: once NATS is back, every issue's events reach each destination in id
// order, the order the Legion daemon's sequence fence needs.
func TestABacklogPublishedAcrossANATSOutageKeepsItsOrder(t *testing.T) {
	const issues, count = 2, 20
	_, url := testnats.Start(t)
	proxy := testnats.StartProxy(t, url)
	client, err := bus.ConnectOwningStream([]string{proxy.URL()})
	if err != nil {
		t.Fatalf("connect the outbox's client: %v", err)
	}
	t.Cleanup(client.Close)
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedThroughputBacklog(t, database, broker, issues, count)

	proxy.Cut()
	stop := runOutbox(t, database, client, broker)
	defer stop()
	// Long enough for the outbox to have tried, and failed, more than one send.
	time.Sleep(12 * time.Second)
	if left := unpublishedCount(t, database); left != count {
		t.Fatalf("%d events were marked published while NATS was down", count-left)
	}
	proxy.Restore()
	restarted := time.Now()

	waitFor(t, 30*time.Second, "the backlog published once NATS is back", func() bool {
		return unpublishedCount(t, database) == 0
	})
	t.Logf("the backlog of %d events was published %s after NATS came back", count, time.Since(restarted).Round(time.Millisecond))
	assertStreamInOrderPerDestination(t, url, count+count/2)
}

// One issue's events reach its NATS subject in seq order, which is what the Legion daemon's sequence
// fence consumes (packages/daemon/internal/admit/admit.go): it applies an issue's fact only above
// the last seq it applied from that issue and drops the rest as stale. Event ids do not commit in
// id order across issues (events.Broker.Append takes only its owner's row): here issue Y's first
// event draws the lowest id and stays open while issue X's two events commit and are published.
// Then Y's first commits, and Y's second, which can draw its id only once Y's row is free,
// commits after it. The outbox claims every row still unpublished, lowest id first
// (claimPendingEvents), so Y's late, lower id is published once it commits, and before Y's
// second.
//
// An outbox that kept a cursor on events.id at the last id it had published, the way a reader
// could while the ids committed in id order, would pass Y's first event's id when it published X's
// second, and never send it: Y's subject would carry seq 2 with no seq 1, and the daemon would hold
// an issue whose earlier fact never arrives.
func TestOneIssuesEventsReachItsSubjectInSeqOrderWhileAnotherIssueCommitsAcrossThem(t *testing.T) {
	ctx := context.Background()
	_, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect the outbox's client: %v", err)
	}
	t.Cleanup(client.Close)
	database := storetest.Open(t)
	broker := events.NewBroker()
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name) values ('TT', 'Test')`); err != nil {
		t.Fatalf("create project: %v", err)
	}
	const x, y = "TT-1", "TT-2"
	for number, key := range []string{x, y} {
		if _, err := database.Pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ($1, 'TT', $2, 'Order', '{"kind":"user","id":"alice"}', 'U')
		`, key, number+1); err != nil {
			t.Fatalf("create issue %s: %v", key, err)
		}
	}
	stop := runOutbox(t, database, client, broker)
	defer stop()

	// issue.deployed by the system notifies no one, so each event has the issue's subject as its
	// one destination.
	deployed := func(key string, run int) model.Event {
		return model.Event{
			IssueKey: new(key), Type: "issue.deployed", Actor: model.Actor{Kind: "system", ID: "delivery-timeline"},
			Payload: map[string]any{"repo": "acme/widgets", "number": 501, "run_id": run, "completed_at": "2024-01-01T02:10:00Z"},
		}
	}
	committed := func(event model.Event) model.Event {
		t.Helper()
		event = appendEvent(t, database, broker, event)
		broker.Publish(event)
		return event
	}

	open, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin Y's first write: %v", err)
	}
	defer open.Rollback(context.Background())
	y1, err := broker.Append(ctx, open, deployed(y, 1))
	if err != nil {
		t.Fatalf("append Y's first event: %v", err)
	}
	x1 := committed(deployed(x, 2))
	x2 := committed(deployed(x, 3))
	if y1.ID >= x1.ID {
		t.Fatalf("Y's open event drew id %d, not below X's committed %d, so this test proves nothing", y1.ID, x1.ID)
	}
	waitFor(t, 30*time.Second, "X's events published while Y's first write is open", func() bool {
		return publishedAt(t, database, x1.ID) != nil && publishedAt(t, database, x2.ID) != nil
	})
	if err := open.Commit(ctx); err != nil {
		t.Fatalf("commit Y's first write: %v", err)
	}
	broker.Publish(y1)
	y2 := committed(deployed(y, 4))
	// An owner's events run in one lane, which one batch at a time holds and no other batch reads
	// into (lanes), and a lane's sends go in id order, so Y's first event is sent and recorded
	// before its second, however many batches run beside them.
	waitFor(t, 30*time.Second, "Y's second event published", func() bool {
		return publishedAt(t, database, y2.ID) != nil
	})

	seqs, ids := issueSubjectOrder(t, url)
	for _, key := range []string{x, y} {
		if !slices.Equal(seqs[key], []int{1, 2}) {
			t.Fatalf("issue %s's subject carried seqs %v, want [1 2]", key, seqs[key])
		}
	}
	// The stream holds Y's first event after X's second although its id is lower: the write
	// committed out of id order, and the outbox still sent it.
	if want := []int64{x1.ID, x2.ID, y1.ID, y2.ID}; !slices.Equal(ids, want) {
		t.Fatalf("the stream holds events %v, want %v", ids, want)
	}
}

// issueSubjectOrder reads every message the notification stream holds on an issue's subject, in
// stream order, and returns the seq of each event grouped by its issue, and the event ids of all of
// them in stream order.
func issueSubjectOrder(t *testing.T, url string) (map[string][]int, []int64) {
	t.Helper()
	reader, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect the reader: %v", err)
	}
	defer reader.Close()
	js, err := reader.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	seqs := map[string][]int{}
	var ids []int64
	for sequence := info.State.FirstSeq; sequence <= info.State.LastSeq && info.State.Msgs > 0; sequence++ {
		message, err := js.GetMsg(bus.Stream, sequence)
		if err != nil {
			t.Fatalf("read stream message %d: %v", sequence, err)
		}
		if !strings.HasPrefix(message.Subject, "notifications.dispatch.issue.") {
			continue
		}
		var envelope contracts.Envelope
		if err := json.Unmarshal(message.Data, &envelope); err != nil {
			t.Fatalf("decode an envelope: %v", err)
		}
		var event model.Event
		if err := json.Unmarshal([]byte(envelope.Payload), &event); err != nil {
			t.Fatalf("decode the event on %s: %v", message.Subject, err)
		}
		if event.IssueKey == nil || !strings.HasPrefix(message.Subject, "notifications.dispatch.issue."+*event.IssueKey+".") {
			t.Fatalf("event %d of issue %v arrived on %s", event.ID, event.IssueKey, message.Subject)
		}
		seqs[*event.IssueKey] = append(seqs[*event.IssueKey], event.Seq)
		ids = append(ids, event.ID)
	}
	return seqs, ids
}

// A connection cut while a batch is in flight - sent, its acknowledgements still on their way back
// across a remote hub's round trip - keeps the backlog's order. The window sends nothing on the
// connection that comes back, a send that met the cut stops its chain, and every event that met the
// cut is released, not backed off, so a later scan sends them again, each destination's in id
// order, and the stream's MsgId drops the copies of those that had landed. A copy the window had
// to send again without its id is the exception, which the stream stores as a message of its own.
// Once the backlog is published, the stream holds every destination once, each issue's events in
// id order on each, and no event counted an attempt.
func TestABatchCutOffInFlightKeepsItsOrder(t *testing.T) {
	const issues, count = 4, 2 * batchSize
	_, url := testnats.Start(t)
	proxy := testnats.StartProxy(t, url, testnats.ProxyLatency(150*time.Millisecond))
	client, err := bus.ConnectOwningStream([]string{proxy.URL()})
	if err != nil {
		t.Fatalf("connect the outbox's client: %v", err)
	}
	t.Cleanup(client.Close)
	reader, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect the reader: %v", err)
	}
	defer reader.Close()
	js, err := reader.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedThroughputBacklog(t, database, broker, issues, count)

	stop := runOutbox(t, database, client, broker)
	defer stop()
	// Cut once the first envelopes reached the stream and before any acknowledgement made it back.
	waitFor(t, 10*time.Second, "the first batch reaching the stream", func() bool {
		info, err := js.StreamInfo(bus.Stream)
		return err == nil && info.State.Msgs > 0
	})
	proxy.Cut()
	if left := unpublishedCount(t, database); left != count {
		t.Fatalf("%d events were marked published before the cut; the cut came too late to test anything", count-left)
	}
	time.Sleep(2 * time.Second)
	proxy.Restore()

	waitFor(t, 90*time.Second, "the backlog published after the cut", func() bool {
		return unpublishedCount(t, database) == 0
	})
	var attempted int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from events where attempt_count <> 0`).Scan(&attempted); err != nil {
		t.Fatalf("count attempted events: %v", err)
	}
	if attempted != 0 {
		t.Fatalf("%d events were backed off for the cut, want every one released", attempted)
	}
	assertStreamInOrderPerDestination(t, url, count+count/2)
}

// An outbox that dies after NATS acknowledged an event's destinations and before it marked the
// event published leaves the event unpublished and the publisher hold its own until the hold
// lapses, so the process that takes the hold over publishes it again: at least once, never at most
// once. The repeat adds nothing to the stream, which drops it inside its duplicate window by each
// destination's MsgId.
func TestAnOutboxThatDiesBetweenAckAndMarkPublishesAgainWithoutDuplicates(t *testing.T) {
	const issues, count = 2, 10
	_, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect the outbox's client: %v", err)
	}
	t.Cleanup(client.Close)
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedThroughputBacklog(t, database, broker, issues, count)
	// The mark is what fails, as a process that dies right after the acknowledgements never makes it.
	if _, err := database.Pool.Exec(context.Background(), `
		create function refuse_mark() returns trigger language plpgsql as $$
		begin
			if new.published_at is not null then raise exception 'the process died before the mark'; end if;
			return new;
		end $$;
		create trigger refuse_mark before update on events for each row execute function refuse_mark();
	`); err != nil {
		t.Fatalf("make the mark fail: %v", err)
	}

	stop := runOutbox(t, database, client, broker)
	reader, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect the reader: %v", err)
	}
	defer reader.Close()
	waitFor(t, 30*time.Second, "every destination acknowledged", func() bool {
		_, all := streamOrder(t, reader)
		return len(all) == count+count/2
	})
	stop()
	if left := unpublishedCount(t, database); left != count {
		t.Fatalf("%d events were marked published though every mark failed", count-left)
	}
	// What a process killed mid-batch leaves: the hold's row still naming it, lapsing a TTL after
	// its last renewal.
	dead := uuid.New()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into duty_holds (name, holder, expires_at) values ($1, $2, now() + make_interval(secs => $3))
	`, publisherHold, dead, publisherHoldOptions.TTL.Seconds()); err != nil {
		t.Fatalf("leave the dead holder's row: %v", err)
	}
	died := time.Now()

	if _, err := database.Pool.Exec(context.Background(), `drop trigger refuse_mark on events`); err != nil {
		t.Fatalf("let the mark succeed: %v", err)
	}
	stop = runOutbox(t, database, client, broker)
	defer stop()
	waitFor(t, publisherHoldOptions.TTL+10*time.Second, "the backlog marked published by the next holder", func() bool {
		return unpublishedCount(t, database) == 0
	})
	if took := time.Since(died); took < publisherHoldOptions.TTL-time.Second {
		t.Fatalf("the next holder published %s after the death, before the dead holder's hold lapsed", took.Round(time.Millisecond))
	}
	assertStreamInOrderPerDestination(t, url, count+count/2)
}
