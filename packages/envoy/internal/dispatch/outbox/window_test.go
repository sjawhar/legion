package outbox

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// heldPublisher answers no publish until the test answers it, as NATS answers a publish a round
// trip after it was sent.
type heldPublisher struct {
	mu      sync.Mutex
	topics  []string
	answers []chan error
}

func (p *heldPublisher) Window() bus.Sender { return p }

func (p *heldPublisher) PublishAsync(item contracts.Envelope) <-chan error {
	p.mu.Lock()
	defer p.mu.Unlock()
	answer := make(chan error, 1)
	p.topics = append(p.topics, item.Topic)
	p.answers = append(p.answers, answer)
	return answer
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

// The outbox sends a whole batch before NATS has answered any of it, and marks an event published
// only once NATS has answered every destination it was sent to: until then the event is neither
// published nor backed off, and once its answers are in, its destinations are recorded in the
// order they were sent.
func TestABatchIsSentBeforeAnyAnswerAndMarkedOnlyOnceEveryAnswerIsIn(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	first := seedThreeDestinationEvent(t, database, broker)
	second := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.MessageEventPayload{Message: model.Message{ID: "message-1", IssueKey: new("T-1"), Body: "Next"}},
	})
	publisher := &heldPublisher{}
	stop := run(t, database, publisher, broker)
	defer stop()

	firstTopics := []string{"notifications.dispatch.issue.T-1.comment.created", roleTopic, "notifications.agent.writer"}
	wantSent := append(slices.Clone(firstTopics), "notifications.dispatch.issue.T-1.message.created", roleTopic)
	waitFor(t, 5*time.Second, "both events sent with no answer back", func() bool { return len(publisher.sent()) == len(wantSent) })
	if got := publisher.sent(); !slices.Equal(got, wantSent) {
		t.Fatalf("sent %v, want %v: each event's destinations in turn, in id order", got, wantSent)
	}

	publisher.accept(0, 2)
	time.Sleep(200 * time.Millisecond)
	for _, event := range []model.Event{first, second} {
		if publishedAt(t, database, event.ID) != nil {
			t.Fatalf("event %d was marked published with an answer still to come", event.ID)
		}
		if destinations, attempts := publishedDestinations(t, database, event.ID); len(destinations) != 0 || attempts != 0 {
			t.Fatalf("event %d recorded %v after %d attempts with an answer still to come", event.ID, destinations, attempts)
		}
	}

	publisher.accept(2, len(wantSent))
	waitFor(t, 5*time.Second, "both events marked published", func() bool {
		return publishedAt(t, database, first.ID) != nil && publishedAt(t, database, second.ID) != nil
	})
	if destinations, _ := publishedDestinations(t, database, first.ID); !slices.Equal(destinations, firstTopics) {
		t.Fatalf("published_destinations = %v, want %v", destinations, firstTopics)
	}
}

// An outbox that stops with a batch's answers still coming records what NATS already accepted and
// releases the rest: the event whose every destination was answered is marked published, and the
// one still waiting is neither published nor backed off nor left claimed, so another process takes
// it at once rather than when claimWindow lapses.
func TestAnOutboxStoppedMidBatchMarksTheAnsweredAndReleasesTheRest(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	first := seedThreeDestinationEvent(t, database, broker)
	second := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.MessageEventPayload{Message: model.Message{ID: "message-1", IssueKey: new("T-1"), Body: "Next"}},
	})
	publisher := &heldPublisher{}
	stop := run(t, database, publisher, broker)
	waitFor(t, 5*time.Second, "both events sent", func() bool { return len(publisher.sent()) == 5 })
	publisher.accept(0, 3)
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

// assertStreamInIDOrder fails t unless the stream holds want messages, each subject's in event id
// order, and all of them in event id order.
func assertStreamInIDOrder(t *testing.T, url string, want int) {
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
	for subject, ids := range bySubject {
		if at := firstDescent(ids); at >= 0 {
			t.Fatalf("subject %s holds event %d after event %d: %v", subject, ids[at], ids[at-1], ids)
		}
	}
	if at := firstDescent(all); at >= 0 {
		t.Fatalf("the stream holds event %d after event %d: %v", all[at], all[at-1], all)
	}
}

// lapseClaims makes every unpublished event due now, as claimWindow passing after a process died
// mid-batch does.
func lapseClaims(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`update events set next_attempt_at = now() - interval '1 second' where published_at is null`); err != nil {
		t.Fatalf("lapse the claims: %v", err)
	}
}

// A NATS outage holds the backlog back without reordering it. An event the outbox could not send
// because NATS was unreachable is not the event's failure, so it is not backed off behind the
// events after it: once NATS is back, the backlog reaches the stream in id order, every issue's
// events on its subject in the order the Legion daemon's sequence fence needs.
func TestABacklogPublishedAcrossANATSOutageKeepsItsOrder(t *testing.T) {
	const issues, count = 2, 20
	_, url := testnats.Start(t)
	proxy, proxied := startOutageProxy(t, strings.TrimPrefix(url, "nats://"), 0)
	client, err := bus.ConnectOwningStream([]string{proxied})
	if err != nil {
		t.Fatalf("connect the outbox's client: %v", err)
	}
	t.Cleanup(client.Close)
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedThroughputBacklog(t, database, broker, issues, count)

	proxy.down()
	stop := runOutbox(t, database, client, broker)
	defer stop()
	// Long enough for the outbox to have tried, and failed, more than one send.
	time.Sleep(12 * time.Second)
	if left := unpublishedCount(t, database); left != count {
		t.Fatalf("%d events were marked published while NATS was down", count-left)
	}
	proxy.up(t)
	restarted := time.Now()

	waitFor(t, 30*time.Second, "the backlog published once NATS is back", func() bool {
		return unpublishedCount(t, database) == 0
	})
	t.Logf("the backlog of %d events was published %s after NATS came back", count, time.Since(restarted).Round(time.Millisecond))
	assertStreamInIDOrder(t, url, count+count/2)
}

// A connection cut while a batch is in flight - sent, its acknowledgements still on their way back
// across a remote hub's round trip - keeps the backlog's order. The window sends nothing on the
// connection that comes back, and every event that met the cut is released, not backed off, so
// the next scan sends them again in id order and the stream's MsgId drops the copies of those that
// had landed: once the backlog is published, the stream holds every destination once, each
// issue's events in id order, and no event counted an attempt.
func TestABatchCutOffInFlightKeepsItsOrder(t *testing.T) {
	const issues, count = 4, 2 * batchSize
	_, url := testnats.Start(t)
	proxy, proxied := startOutageProxy(t, strings.TrimPrefix(url, "nats://"), 150*time.Millisecond)
	client, err := bus.ConnectOwningStream([]string{proxied})
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
	proxy.down()
	if left := unpublishedCount(t, database); left != count {
		t.Fatalf("%d events were marked published before the cut; the cut came too late to test anything", count-left)
	}
	time.Sleep(2 * time.Second)
	proxy.up(t)

	waitFor(t, 60*time.Second, "the backlog published after the cut", func() bool {
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
	assertStreamInIDOrder(t, url, count+count/2)
}

// An outbox that dies after NATS acknowledged an event's destinations and before it marked the
// event published leaves the event unpublished and claimed, so another process publishes it again
// once the claim lapses: at least once, never at most once. The repeat adds nothing to the stream,
// which drops it inside its duplicate window by each destination's MsgId.
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
	waitFor(t, 10*time.Second, "every destination acknowledged", func() bool {
		_, all := streamOrder(t, reader)
		return len(all) == count+count/2
	})
	stop()
	if left := unpublishedCount(t, database); left != count {
		t.Fatalf("%d events were marked published though every mark failed", count-left)
	}

	if _, err := database.Pool.Exec(context.Background(), `drop trigger refuse_mark on events`); err != nil {
		t.Fatalf("let the mark succeed: %v", err)
	}
	lapseClaims(t, database)
	stop = runOutbox(t, database, client, broker)
	defer stop()
	waitFor(t, 10*time.Second, "the backlog marked published by the next process", func() bool {
		return unpublishedCount(t, database) == 0
	})
	assertStreamInIDOrder(t, url, count+count/2)
}
