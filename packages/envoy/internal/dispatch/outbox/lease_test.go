package outbox

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// process is one envoy-dispatch process's outbox under test: its own pool, its own events broker
// and its own NATS client, which counts every envelope it sends.
type process struct {
	name    string
	outbox  *Outbox
	store   *store.Store
	broker  *events.Broker
	sending *sendCounter
	stop    func()
}

// sendCounter counts the envelopes every process of a test sends, by event and subject.
type sendCounter struct {
	mu    sync.Mutex
	sends map[string]int
}

func (c *sendCounter) add(item contracts.Envelope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sends == nil {
		c.sends = map[string]int{}
	}
	c.sends[item.SourceEventID+" "+item.Topic]++
}

// of is how many envelopes of event were sent, and the most sent to any one subject.
func (c *sendCounter) of(event int64) (total, most int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, count := range c.sends {
		if strings.HasPrefix(key, fmt.Sprint(event)+" ") {
			total += count
			most = max(most, count)
		}
	}
	return total, most
}

// countingClient is a bus client whose windows count every envelope they send.
type countingClient struct {
	*bus.Client
	counter *sendCounter
}

func (c countingClient) WindowUntil(deadline func() time.Time) bus.Sender {
	return countingSender{Sender: c.Client.WindowUntil(deadline), counter: c.counter}
}

type countingSender struct {
	bus.Sender
	counter *sendCounter
}

func (s countingSender) PublishAsync(item contracts.Envelope) <-chan error {
	s.counter.add(item)
	return s.Sender.PublishAsync(item)
}

// startProcess runs an outbox on database's own outbox pool, publishing through a client of its own
// connected to url, until the test ends or stop is called.
func startProcess(t *testing.T, name string, database *store.Store, url string, counter *sendCounter) *process {
	t.Helper()
	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect %s's NATS client: %v", name, err)
	}
	t.Cleanup(client.Close)
	pool, err := database.Pool.Outbox()
	if err != nil {
		t.Fatalf("open %s's outbox pool: %v", name, err)
	}
	p := &process{name: name, store: database, broker: events.NewBroker(), sending: counter}
	p.outbox = New(Deps{
		Store:     &store.Store{Pool: pool},
		Publisher: countingClient{Client: client, counter: counter},
		Broker:    p.broker,
		Listen:    database.Pool.Listener,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.outbox.Run(ctx); close(done) }()
	p.stop = sync.OnceFunc(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("%s's outbox did not stop", name)
		}
	})
	t.Cleanup(p.stop)
	return p
}

// publishing is the processes that publish now.
func publishing(processes ...*process) []string {
	var names []string
	for _, p := range processes {
		if p.outbox.Publishing() {
			names = append(names, p.name)
		}
	}
	return names
}

// waitForOnePublisher waits until exactly one of processes publishes, and returns it.
func waitForOnePublisher(t *testing.T, within time.Duration, processes ...*process) *process {
	t.Helper()
	var holder *process
	waitFor(t, within, "exactly one outbox publishing", func() bool {
		holder = nil
		count := 0
		for _, p := range processes {
			if p.outbox.Publishing() {
				holder = p
				count++
			}
		}
		return count == 1
	})
	return holder
}

// writeMessage commits one message event on issue T-1 through p's store and broker, as a request to
// p would, and returns it.
func writeMessage(t *testing.T, p *process, body string) model.Event {
	t.Helper()
	event := appendEvent(t, p.store, p.broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: new("T-1"), Body: body},
	})
	p.broker.Publish(event)
	return event
}

// waitForPublished waits until event is marked published and was sent to every subject once.
func waitForPublished(t *testing.T, within time.Duration, database *store.Store, counter *sendCounter, event model.Event) {
	t.Helper()
	waitFor(t, within, fmt.Sprintf("event %d published", event.ID), func() bool {
		return publishedAt(t, database, event.ID) != nil
	})
	if total, most := counter.of(event.ID); total == 0 || most != 1 {
		t.Fatalf("event %d: %d envelopes sent, at most %d to one subject, want each of its subjects sent once", event.ID, total, most)
	}
}

// outboxHangGuard bounds every wait the row below makes. It is a hang guard, not a budget: the row
// asserts what the outbox did and in what order, never how fast it did it, so load only makes the
// same reading take longer to reach, and a wait that reaches this bound means the thing it waited
// for never happened at all.
const outboxHangGuard = 60 * time.Second

// backOffDue is how long the back-off of the row below is due in, and the one bound the row puts
// on the outbox's own work: the insert's notification must reach the holder, and the scan it
// wakes must read next_attempt_at, inside it. A scan later than that finds the back-off already
// due, arms a wait of zero and is caught by the lower end of the wait assertion, so the row still
// fails on what happened rather than on a clock - but it would be failing for the machine's
// slowness and not the outbox's, which is why this is ten seconds against the single-digit
// milliseconds that work takes on an idle pod.
const backOffDue = 10 * time.Second

// An idle outbox scans nothing: no claim runs while no event is written and no retry falls due.
// An event another process commits - which reaches this one only through the database, not its
// broker - is claimed at once by the LISTEN on dispatch_events, and a back-off is claimed when it
// falls due, by the one timer armed to it, with nothing else waking the outbox in between. Every
// step is read from what the outbox and the database recorded - the waits armed, the claims made,
// the moment the database marked the event published - and never from how long a step took.
func TestTheOutboxDoesNotScanWhileIdle(t *testing.T) {
	database := storetest.Open(t)
	other := storetest.SecondProcess(t, database)
	seedIssue(t, database, "T-1", nil)
	publisher := &recordingPublisher{}
	outbox := New(Deps{Store: database, Publisher: publisher, Broker: events.NewBroker(), Listen: database.Pool.Listener})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// Every wait the outbox arms its one retry timer for, with the moment it was armed and the
	// claims the outbox had made by then: the sequence this row reads, in place of a wall clock.
	type armedWait struct {
		wait   time.Duration
		at     time.Time
		claims int64
	}
	var armedMu sync.Mutex
	var armed []armedWait
	outbox.afterRetryArmed = func(wait time.Duration) {
		armedMu.Lock()
		defer armedMu.Unlock()
		armed = append(armed, armedWait{wait: wait, at: time.Now(), claims: outbox.claims.Load()})
	}
	// armedWaits copies what has been armed so far, so nothing reads the slice unlocked.
	armedWaits := func() []armedWait {
		armedMu.Lock()
		defer armedMu.Unlock()
		return slices.Clone(armed)
	}
	var listening atomic.Bool
	outbox.afterListening = func() {
		listening.Store(true)
		// One wake more, so the holder is certain to enter a wait after this flag is set.
		// OnConnected signals before it reports, so the holder may have taken that connection's
		// own wake, scanned and parked again while the flag was still false, and such a wait
		// records nothing; without this the row could read a rest the catch-up scan has yet to
		// follow.
		outbox.signal()
	}
	// restingAt is the claims the outbox had made when the holder last entered its wait with
	// nothing pending and the LISTEN connection already up, and -1 until it enters one. A wait
	// entered with a wake pending records nothing, because the scan that wake causes is still to
	// come; a wait entered before that connection is up records nothing either, because the
	// connection's own catch-up scan is still to come and would land inside the idle window
	// below. The holder stores this before it waits and scans only after, so while restingAt
	// equals the claims made the holder is waiting and nothing has scanned since - a state that
	// holds for as long as the outbox has nothing to do, rather than an edge a reader can arrive
	// too late to see.
	var restingAt atomic.Int64
	restingAt.Store(-1)
	outbox.afterServeWait = func(wakePending bool) {
		if !wakePending && listening.Load() {
			restingAt.Store(outbox.claims.Load())
		}
	}
	go func() { outbox.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	// atRest waits for the holder to be waiting with nothing pending and nothing scanned since,
	// a wait it entered with the LISTEN connection already up. That is this row's "the outbox has
	// nothing left to do": every catch-up scan - the hold's take, and that connection's - has
	// already run, and only a new event can wake the outbox again. This row's publisher never
	// disconnects, so such a wait is always one the holder reached with nothing to scan, never
	// one it reached with NATS down.
	atRest := func(what string) {
		t.Helper()
		// restingAt is read first, so the claims it is compared with are never older than it: an
		// equal pair is a wait no scan has followed, never a scan this read straddled.
		waitFor(t, outboxHangGuard, what, func() bool { return restingAt.Load() == outbox.claims.Load() })
	}

	waitFor(t, outboxHangGuard, "the outbox taking the publisher hold", outbox.Publishing)
	// The first scan and the LISTEN connection's catch-up run as the hold is taken; the row reads
	// that they are over rather than allowing them a span of time.
	atRest("the outbox resting after the hold's first scan and the LISTEN connection's catch-up")
	idleFrom := outbox.claims.Load()
	// A lower bound on quiet, not an upper bound on work: the row asserts nothing scanned in this
	// window, and load can only make an idle outbox quieter, never busier.
	time.Sleep(12 * time.Second)
	if claims := outbox.claims.Load() - idleFrom; claims != 0 {
		t.Fatalf("an idle outbox ran %d claims in 12 s, want 0", claims)
	}

	// Committed on another process's pool, and published to no broker this outbox hears.
	fresh := appendEvent(t, other, events.NewBroker(), model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "fresh", IssueKey: new("T-1"), Body: "fresh"},
	})
	// A local clock for the hang guards and the readings below alone: no assertion reads it.
	committed := time.Now()
	// The 12 s window above showed this outbox scans on nothing periodic, so a claim at all after
	// this commit is the notification's; the bound only stops the row hanging when no claim ever
	// comes.
	waitFor(t, outboxHangGuard, "the event another process committed claimed", func() bool {
		return outbox.claims.Load() != idleFrom
	})
	t.Logf("the event another process committed was claimed %s after its commit", time.Since(committed).Round(time.Millisecond))
	// The same hang guard: the claim above is this event's own scan, so the only way to this bound
	// is a publication that never happens at all.
	waitFor(t, outboxHangGuard, "the other process's event published", func() bool { return publishedAt(t, database, fresh.ID) != nil })
	// The sequence below starts from an outbox at rest: the wait above returns as soon as the
	// other process's event is marked published, while the batch that published it has still to
	// start the batch that follows it and the scan has still to arm whatever comes of that.
	atRest("the outbox resting after the other process's event")

	// A back-off due in backOffDue, committed by the other process: its insert wakes the outbox,
	// whose scan finds nothing due and arms its one retry timer to the back-off's own moment, and
	// the timer's scan is the one that publishes it. The waits armed earlier in this row are not
	// this one's.
	armedBefore := len(armedWaits())
	// The waits this back-off's own scan armed, read afresh each time: the slice only grows.
	waitsSince := func() []armedWait { return armedWaits()[armedBefore:] }
	// due is the moment the database computed for this back-off; every assertion about it below is
	// made against that value, on the database's own clock.
	backedOff, due := appendBackedOffEvent(t, other, backOffDue)
	// A local clock for this loop's hang guard and the readings below alone: no assertion reads it.
	inserted := time.Now()
	var published time.Time
	for {
		// Read oldest first, so nothing is judged against a reading younger than itself: the waits,
		// then the claims (never fewer than the waits' own count), then the database's answer,
		// whose now() is therefore no earlier than the moment those claims were read.
		since := waitsSince()
		claims := outbox.claims.Load()
		at, beforeDue := backOffState(t, database, backedOff.ID)
		if at != nil {
			published = *at
			break
		}
		// The one clock left in this loop is a hang guard, 60 s against the backOffDue the work
		// waits out, so no load reaches it: it fires only when nothing was published at all. A
		// hang guard needs no agreement between clocks, so the local one serves.
		if time.Since(inserted) > outboxHangGuard {
			t.Fatalf("the back-off due in %s was never published: %s after its insert the outbox had armed %v",
				backOffDue, time.Since(inserted).Round(time.Millisecond), waitsSince())
		}
		// Nothing else wakes the outbox until the timer fires: no claim since the wait was armed,
		// while the database says the back-off is still ahead on its own clock.
		if len(since) > 0 && beforeDue {
			if made := claims - since[0].claims; made != 0 {
				t.Fatalf("the outbox made %d claims in the %s since it armed a %s wait for the back-off, %s after the insert and before the back-off fell due, want none",
					made, time.Since(since[0].at).Round(time.Millisecond), since[0].wait.Round(time.Millisecond),
					since[0].at.Sub(inserted).Round(time.Millisecond))
			}
		}
		// How often this loop reads, bounding nothing: a coarser poll would only read the same
		// sequence later.
		time.Sleep(5 * time.Millisecond)
	}
	publishedAfter := time.Since(inserted)
	since := waitsSince()
	if len(since) != 1 {
		t.Fatalf("the outbox armed %d waits between the back-off's insert and its publish, want 1: %v", len(since), since)
	}
	// The wait is a value the database computed from its own next_attempt_at, not an elapsed time
	// this test measured, so a loaded runner can only make it smaller: the only way past backOffDue
	// is a wait armed for something that is not this back-off. The lower end rules out the one
	// case left: a wait of zero is the first scan after the insert reading a next_attempt_at
	// already past, so that scan came more than backOffDue after the inserting transaction began -
	// whatever delayed it - and the run never exercised the armed timer at all. backOffDue's own
	// comment says why ten seconds is enough room for that scan.
	if since[0].wait <= 0 || since[0].wait > backOffDue {
		t.Fatalf("the outbox armed a %s wait for a back-off due in %s, want one above zero and no more than %s",
			since[0].wait, backOffDue, backOffDue)
	}
	// Exact where a margin was a guess: the publication time and the moment the back-off fell due
	// are both the database's own values, so no clock is compared with another and no load can
	// move either. A publication before that moment would be the outbox publishing a back-off that
	// was not yet due.
	if published.Before(due) {
		t.Fatalf("the back-off was published at %s, %s before the %s the database made it due, want no sooner than due",
			published, due.Sub(published).Round(time.Millisecond), due)
	}
	// A reading for the record, not an assertion: the publishing batch starts another batch, so
	// the scan the timer woke claims more than once.
	t.Logf("the outbox armed a %s wait %s after the insert, the back-off was published %s after it and %s after the database made it due, and the outbox made %d claims from the arming to the publish",
		since[0].wait.Round(time.Millisecond), since[0].at.Sub(inserted).Round(time.Millisecond),
		publishedAfter.Round(time.Millisecond), published.Sub(due).Round(time.Millisecond),
		outbox.claims.Load()-since[0].claims)
}

// appendBackedOffEvent commits an event on database whose next attempt is due after wait on the
// database's clock, as a back-off leaves it. It returns the event and the next_attempt_at the
// database computed: the moment it falls due, which only the database's own clock can be read
// against.
func appendBackedOffEvent(t *testing.T, database *store.Store, wait time.Duration) (model.Event, time.Time) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin event transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	event, err := events.NewBroker().Append(ctx, tx, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "backed-off", IssueKey: new("T-1"), Body: "backed off"},
	})
	if err != nil {
		t.Fatalf("append event: %v", err)
	}
	var due time.Time
	if err := tx.QueryRow(ctx, `update events set next_attempt_at = now() + make_interval(secs => $2), attempt_count = 1 where id = $1 returning next_attempt_at`,
		event.ID, wait.Seconds()).Scan(&due); err != nil {
		t.Fatalf("back the event off: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit event: %v", err)
	}
	return event, due
}

// backOffState reads whether the event has been published and whether its back-off is still
// ahead, both on the database's own clock and in one statement: next_attempt_at is a value the
// database computed, so only the database can say whether it has passed. A published event has no
// next_attempt_at left (the publisher clears it), and nothing ahead of the clock either.
func backOffState(t *testing.T, database *store.Store, eventID int64) (published *time.Time, beforeDue bool) {
	t.Helper()
	var ahead *bool
	if err := database.Pool.QueryRow(context.Background(),
		`select published_at, now() < next_attempt_at from events where id = $1`, eventID).Scan(&published, &ahead); err != nil {
		t.Fatalf("read the event's back-off state: %v", err)
	}
	return published, ahead != nil && *ahead
}

// Two processes' outboxes on one database: exactly one publishes at every moment, and every event
// either process writes is sent once, by whichever holds the publisher hold.
func TestOnlyOneOutboxPublishesAtATime(t *testing.T) {
	_, url := testnats.Start(t)
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	counter := &sendCounter{}
	first := startProcess(t, "first", database, url, counter)
	second := startProcess(t, "second", storetest.SecondProcess(t, database), url, counter)
	waitForOnePublisher(t, 10*time.Second, first, second)

	var written []model.Event
	deadline := time.Now().Add(10 * time.Second)
	for sample := 0; time.Now().Before(deadline); sample++ {
		if names := publishing(first, second); len(names) != 1 {
			t.Fatalf("sample %d: publishing %v, want exactly one outbox", sample, names)
		}
		if sample%10 == 0 {
			writer := []*process{first, second}[(sample/10)%2]
			written = append(written, writeMessage(t, writer, fmt.Sprintf("message %d", sample)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, event := range written {
		waitForPublished(t, 5*time.Second, database, counter, event)
	}
}

// A process that stops gracefully releases the publisher hold, and the other takes it over at once
// rather than when the hold would have lapsed.
func TestAStoppedOutboxHandsThePublisherToTheOtherAtOnce(t *testing.T) {
	_, url := testnats.Start(t)
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	counter := &sendCounter{}
	first := startProcess(t, "first", database, url, counter)
	second := startProcess(t, "second", storetest.SecondProcess(t, database), url, counter)
	holder := waitForOnePublisher(t, 10*time.Second, first, second)
	standby := first
	if holder == first {
		standby = second
	}

	holder.stop()
	stopped := time.Now()
	waitFor(t, time.Second, "the standby publishing once the holder stopped", standby.outbox.Publishing)
	t.Logf("the standby took the publisher hold %s after the holder stopped", time.Since(stopped).Round(time.Millisecond))
	event := writeMessage(t, standby, "after the hand-over")
	waitForPublished(t, time.Second, database, counter, event)
}

// A holder whose NATS connection is cut releases the publisher hold once it has been without NATS
// for natsLossGrace, and the other process, whose NATS works, takes the hold and publishes what is
// written after the cut.
func TestAnOutboxThatLosesNATSReleasesThePublisher(t *testing.T) {
	_, url := testnats.Start(t)
	proxy := testnats.StartProxy(t, url)
	proxied := proxy.URL()
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	counter := &sendCounter{}
	cut := startProcess(t, "cut", database, proxied, counter)
	waitFor(t, 10*time.Second, "the first process taking the publisher hold", cut.outbox.Publishing)
	working := startProcess(t, "working", storetest.SecondProcess(t, database), url, counter)

	proxy.Cut()
	downAt := time.Now()
	waitFor(t, natsLossGrace+500*time.Millisecond, "the cut process releasing the publisher hold", func() bool {
		return cut.outbox.hold.Load() == nil
	})
	t.Logf("the cut process released the publisher hold %s after the cut", time.Since(downAt).Round(time.Millisecond))
	event := writeMessage(t, working, "after the cut")
	waitForPublished(t, 10*time.Second-time.Since(downAt), database, counter, event)
	if !working.outbox.Publishing() || cut.outbox.Publishing() {
		t.Fatalf("publishing %v after the cut, want only the process whose NATS works", publishing(cut, working))
	}
}

// A holder whose NATS connection goes silent - still connected, nothing answering - finds out when
// a batch's sends go unanswered and a probe of their connection fails, and releases the publisher
// hold. It does not take the hold back while its connection stays silent, although the connection
// still reports itself connected: the other process, whose NATS works, takes it and publishes the
// event, within 10 s of the silence.
func TestAnOutboxWhoseNATSGoesSilentGivesThePublisherToTheOther(t *testing.T) {
	_, url := testnats.Start(t)
	proxy := testnats.StartProxy(t, url)
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	counter := &sendCounter{}
	silent := startProcess(t, "silent", database, proxy.URL(), counter)
	waitFor(t, 10*time.Second, "the first process taking the publisher hold", silent.outbox.Publishing)
	working := startProcess(t, "working", storetest.SecondProcess(t, database), url, counter)

	proxy.Freeze()
	t.Cleanup(proxy.Restore)
	frozenAt := time.Now()
	event := writeMessage(t, working, "after the silence")
	waitFor(t, 10*time.Second, "the working process publishing the event", func() bool {
		return working.outbox.Publishing() && publishedAt(t, database, event.ID) != nil
	})
	t.Logf("the working process published the event %s after the silence", time.Since(frozenAt).Round(time.Millisecond))
	for range 20 {
		if silent.outbox.Publishing() {
			t.Fatalf("publishing %v while the silent process's connection stays silent, want only the process whose NATS works",
				publishing(silent, working))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A standby waits for the publisher hold to be released or to lapse, or for its NATS connection to
// change, and for nothing else: the events the holder writes and publishes do not make it try the
// hold. 100 events written one after another while the holder holds send at most the few takes and
// lapse reads the standby's timer to the holder's lapse makes over that span (each renewal moves the
// lapse 6 s on), not one take and one read per event, which would put a write on duty_holds for
// every event on every standby.
func TestAStandbyDoesNotTryTheHoldForEveryEventTheHolderWrites(t *testing.T) {
	_, url := testnats.Start(t)
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	counter := &sendCounter{}
	holder := startProcess(t, "holder", database, url, counter)
	waitFor(t, 10*time.Second, "the holder taking the publisher hold", holder.outbox.Publishing)

	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect the standby's NATS client: %v", err)
	}
	t.Cleanup(client.Close)
	tracer := &statementTracer{}
	standby := New(Deps{
		Store:     tracedStore(t, storetest.SecondProcess(t, database), tracer),
		Publisher: client,
		Broker:    events.NewBroker(),
		Listen:    database.Pool.Listener,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { standby.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, 5*time.Second, "the standby trying the publisher hold once", func() bool {
		return len(tracer.matching("insert into duty_holds")) > 0
	})
	// The standby's LISTEN connection's catch-up wake comes as it starts.
	time.Sleep(time.Second)

	takes, reads := len(tracer.matching("insert into duty_holds")), len(tracer.matching("from duty_holds where name"))
	var last model.Event
	for index := range 100 {
		last = writeMessage(t, holder, fmt.Sprintf("message %d", index))
	}
	waitForPublished(t, 10*time.Second, database, counter, last)
	if standby.Publishing() {
		t.Fatal("the standby publishes while the holder holds the publisher hold")
	}
	takes = len(tracer.matching("insert into duty_holds")) - takes
	reads = len(tracer.matching("from duty_holds where name")) - reads
	t.Logf("while 100 events were written and published, the standby sent %d takes of the hold and %d reads of its lapse", takes, reads)
	if takes > 3 || reads > 3 {
		t.Fatalf("the standby sent %d takes of the publisher hold and %d reads of its lapse while the holder wrote 100 events, want at most 3 of each: its timer to the lapse alone",
			takes, reads)
	}
}

// fenceAfterFirstSend is a publisher hold whose deadline passes as the batch's first envelope is
// sent, as a process that froze mid-batch past its deadline finds when it resumes.
type fenceAfterFirstSend struct {
	recordingPublisher
	passed atomic.Bool
}

func (p *fenceAfterFirstSend) WindowUntil(deadline func() time.Time) bus.Sender {
	return deadlineWindow{Sender: p, deadline: deadline}
}

func (p *fenceAfterFirstSend) PublishAsync(item contracts.Envelope) <-chan error {
	p.passed.Store(true)
	return p.recordingPublisher.PublishAsync(item)
}

func (p *fenceAfterFirstSend) Deadline() time.Time {
	if p.passed.Load() {
		return time.Now().Add(-time.Second)
	}
	return time.Now().Add(time.Hour)
}

// A batch checks the publisher hold's deadline before every send and before its record: once the
// deadline has passed it sends nothing more, counts no attempt, writes nothing, and reports the
// hold lost.
func TestABatchPastThePublisherHoldsDeadlineSendsAndRecordsNothingMore(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	for index := range 3 {
		appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
			Payload: model.Message{ID: fmt.Sprintf("message-%d", index), IssueKey: new("T-1"), Body: "fenced"},
		})
	}
	publisher := &fenceAfterFirstSend{}
	batch, err := scanBatch(context.Background(), Deps{Store: database, Publisher: publisher}, publisher, uuid.New())
	if err != nil {
		t.Fatalf("scan the batch: %v", err)
	}
	if !batch.fenced || batch.sent != 1 {
		t.Fatalf("batch = %+v, want it fenced after its first event", batch)
	}
	if sent := publisher.all(); len(sent) != 1 {
		t.Fatalf("the batch sent %d envelopes, want only the one before the deadline passed", len(sent))
	}
	var published, attempted int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) filter (where published_at is not null), count(*) filter (where attempt_count <> 0) from events
	`).Scan(&published, &attempted); err != nil {
		t.Fatalf("read the events: %v", err)
	}
	if published != 0 || attempted != 0 {
		t.Fatalf("%d events marked published and %d backed off past the deadline, want none written", published, attempted)
	}
	end := New(Deps{Store: database, Publisher: &recordingPublisher{}}).scan(context.Background(), publisher)
	if !end.fenced {
		t.Fatalf("a scan under a hold past its deadline ended %+v, want fenced before any claim", end)
	}
}

// reconnectedQuietly is a publisher whose client reads connected again without its connection
// change ever being reported, as a reconnect whose event waits in nats.go's callback queue behind
// a slow callback finds it: ConnectionChanged never closes, and nothing wakes the holder.
type reconnectedQuietly struct {
	recordingPublisher
	connected atomic.Bool
}

func (p *reconnectedQuietly) Connected() bool { return p.connected.Load() }

func (p *reconnectedQuietly) WindowUntil(deadline func() time.Time) bus.Sender {
	return deadlineWindow{Sender: p, deadline: deadline}
}

// A holder whose client reads connected again when natsLossGrace runs out serves on, though no
// connection event came: it reads the client's status as the grace ends, keeps the hold and scans,
// rather than giving the hold up for a loss that is over.
func TestAHolderConnectedAgainWhenTheGraceEndsKeepsTheHold(t *testing.T) {
	database := storetest.Open(t)
	publisher := &reconnectedQuietly{}
	outbox := New(Deps{Store: database, Publisher: publisher})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hold, _ := outbox.tryHold(ctx)
	if hold == nil {
		t.Fatal("the outbox did not take the publisher hold")
	}
	t.Cleanup(func() { _ = hold.Release(context.Background()) })

	ended := make(chan holdEnd, 1)
	go func() { ended <- outbox.serve(ctx, hold) }()
	time.Sleep(200 * time.Millisecond)
	if outbox.claims.Load() != 0 {
		t.Fatal("the holder scanned while its client read disconnected")
	}
	publisher.connected.Store(true)
	select {
	case end := <-ended:
		t.Fatalf("the holder stopped serving (%s) though its client read connected when the grace ended", end)
	case <-time.After(natsLossGrace + time.Second):
	}
	if outbox.claims.Load() == 0 {
		t.Fatal("the holder did not scan once it found its client connected at the grace's end")
	}
	cancel()
	if end := <-ended; end != stopping {
		t.Fatalf("serve ended %s once ctx ended, want %s", end, stopping)
	}
}

// A record that finds the publisher hold another's ends the holder as one that lost the hold, with
// the hold's loss deadline still ahead. The holder stops that hold's renewal there and then: its
// Lost is closed and it renews no more, the other's row is left alone, and a take of the hold
// once it is free again has one renewal running, not two renewing the same row.
func TestAHolderWhoseRecordFindsTheHoldAnothersStopsItsRenewal(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	tracer := &statementTracer{}
	outbox := New(Deps{Store: tracedStore(t, database, tracer), Publisher: &recordingPublisher{}})
	taken := time.Now()
	hold, _ := outbox.tryHold(ctx)
	if hold == nil {
		t.Fatal("the outbox did not take the publisher hold")
	}
	other := uuid.New()
	holdRow(t, database, other)
	appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: uuid.NewString(), IssueKey: new("T-1"), Body: "recorded under another's hold"},
	})

	if end := outbox.publish(ctx, hold); end != holdLost {
		t.Fatalf("publish ended %s, want %s", end, holdLost)
	}
	if elapsed := time.Since(taken); elapsed >= publisherHoldOptions.Renew {
		t.Fatalf("publish took %s, past the first renewal: the test is about a record finding the hold another's before any renewal does", elapsed)
	}
	select {
	case <-hold.Lost():
	default:
		t.Fatal("the lost hold's Lost is still open: its renewal runs on")
	}
	var holder uuid.UUID
	if err := database.Pool.QueryRow(ctx, `select holder from duty_holds where name = $1`, publisherHold).Scan(&holder); err != nil || holder != other {
		t.Fatalf("the hold's row names %s (%v), want the other process's %s left alone", holder, err, other)
	}

	if _, err := database.Pool.Exec(ctx, `delete from duty_holds where name = $1`, publisherHold); err != nil {
		t.Fatalf("free the hold: %v", err)
	}
	renewals := len(tracer.matching("update duty_holds set expires_at"))
	again, _ := outbox.tryHold(ctx)
	if again == nil {
		t.Fatal("the outbox did not take the free publisher hold again")
	}
	t.Cleanup(func() { _ = again.Release(context.Background()) })
	time.Sleep(publisherHoldOptions.Renew + publisherHoldOptions.Renew/4)
	if got := len(tracer.matching("update duty_holds set expires_at")) - renewals; got != 1 {
		t.Fatalf("%s after the hold was taken again, %d renewals were sent, want the new hold's one", publisherHoldOptions.Renew+publisherHoldOptions.Renew/4, got)
	}
}
