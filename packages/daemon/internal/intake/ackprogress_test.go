package intake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// What the two rows here prove together is Acceptance A1 of dispatch://INFRA-1530: a fact held
// well past its acknowledgement window is delivered once and prepared once, because consumeMessage
// reports it in progress while its handler runs (keepInProgress).
//
// The first row is that claim, on the daemon's own consumers, and it makes three assertions of one
// held backlog. The signal itself is the one that fails every run when the keeper is gone: the row
// counts the progress signals the daemon's client put on the wire (watchProgressSignals) between
// one acknowledgement window after the hold began and the moment the hold was freed, and asks for
// at least one. Nothing but the keeper publishes a signal there, so an empty window is a keeper
// that did not report the held fact, whatever JetStream went on to do about it.
//
// The other two assertions are what that signal buys, and they are what A1 is worth having: every
// event of the backlog is prepared exactly once, and the consumer's delivered count equals the
// backlog, so a redelivered copy shows even when the daemon dropped it as a duplicate and no
// second preparation did. Neither of them fails reliably when the keeper is gone, and the reason
// is the daemon's own client. It asks for one message at a time (jetstream.PullMaxMessages with
// consumerFetchBatch in consumeConsumer) and nats.go calls the handler inline, sending the next
// pull request only after the handler returns (nats.go v1.53.1's (*pullConsumer).Consume, whose
// internal handler calls the handler and only then checkPending, which sends the next pull
// request), so no request of the daemon's is open while the fact is held. A redelivery can only
// reach the daemon through the request that follows the handler, which races the acknowledgement
// the handler's end sent — the server works the two on separate queues, o.ackMsgs and
// o.nextMsgReqs — and the acknowledgement usually wins. A backlog does not settle that race for
// the held message: measured with the keeper removed, the two of them caught it in 12 runs of 20,
// and the signal assertion caught it in 20.
//
// The second row is the control, and it is what says the hold is long enough for the server to
// redeliver at all: the same backlog, the same window and the same hold, on a consumer that
// reports nothing in progress, and JetStream delivers the held message again. It is deterministic
// where the first row's delivered count is not, and the one way its client differs from the
// daemon's is why: it asks for one message more than the backlog holds
// (jetstream.PullMaxMessages(heldBacklog+1)), so a request of its own is still open when the
// window expires and the redelivery lands in it the moment the server makes it.
//
// Both rows hold the first of a backlog of three events rather than one message alone, because a
// backlog is what the daemon pays the cost on in production as well (dispatch://INFRA-456), and it
// lets the first row assert what A1 asks of every event rather than of one.

// heldBacklog is how many Dispatch issue events each row publishes, each under a key of its own.
const heldBacklog = 3

// heldOnce holds the first delivery that reaches it until the test releases it, and lets every
// later one straight through. Both rows hold their first delivery this way, so both rows put the
// same hold on the same window. It also keeps the two instants that bound the hold, which is the
// span a row that watches the wire asks its question over.
type heldOnce struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}

	mu    sync.Mutex
	from  time.Time
	until time.Time
}

func newHeldOnce() *heldOnce {
	return &heldOnce{entered: make(chan struct{}), release: make(chan struct{})}
}

// hold blocks the first delivery to reach it until releaseAfter frees it, and returns at once for
// every delivery after that. It gives up on a context that ends, so a handling the daemon cuts —
// its limit, or the test's own stop — does not wedge the row in a channel nothing will close.
func (h *heldOnce) hold(ctx context.Context) {
	first := false
	h.once.Do(func() {
		first = true
		h.mu.Lock()
		h.from = time.Now()
		h.mu.Unlock()
		close(h.entered)
	})
	if !first {
		return
	}
	select {
	case <-h.release:
	case <-ctx.Done():
	}
}

// releaseAfter frees the held delivery once it has been held for wait, counted from the moment it
// was held rather than from the publish, so the hold outlasts the acknowledgement window however
// long the delivery took to arrive. The test owns the timer and the handler only ever waits on it:
// a sleep inside the handler would make the row's timing the machine's business, and a poll of the
// clock would make it the scheduler's.
func (h *heldOnce) releaseAfter(t *testing.T, wait time.Duration) {
	t.Helper()
	<-h.entered
	timer := time.AfterFunc(wait, func() {
		h.mu.Lock()
		h.until = time.Now()
		h.mu.Unlock()
		close(h.release)
	})
	t.Cleanup(func() { timer.Stop() })
}

// window is when the first delivery was held and when releaseAfter freed it: from is zero until a
// delivery has been held, and until is zero until the timer has fired.
func (h *heldOnce) window() (from, until time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.from, h.until
}

// publishHeldBacklog publishes the rows' backlog on the Dispatch durable's subject: heldBacklog
// issue events, each naming an issue of its own, so a second preparation of any of them shows as
// that issue's own second preparation.
func publishHeldBacklog(t *testing.T, js jetstream.JetStream) {
	t.Helper()
	for i := range heldBacklog {
		publishIssueEvent(t, js, fmt.Sprintf("CAPTURE-%d", 300+i), i+1)
	}
}

// publishIssueEvent publishes one Dispatch issue event on the Dispatch durable's subject, naming
// issue key under the event id `held-<id>`: the backlog's events one at a time, and the one event
// a row that drives a single message through the daemon needs.
func publishIssueEvent(t *testing.T, js jetstream.JetStream, key string, id int) {
	t.Helper()
	publish(t, js, "notifications.dispatch.issue."+key+".issue.updated", envelopeJSON(t, fmt.Sprintf("held-%d", id), "dispatch",
		fmt.Sprintf(`{"id":%d,"issue_key":%q,"seq":1,"notify":true,"type":"issue.updated","actor":{"kind":"user","id":"someone"},`+
			`"payload":{"key":%q,"status":"backlog","title":"a held event","parent":null,"rank":"A","labels":[],"resolved_repo":"sjawhar/legion"}}`, id, key, key)))
}

// issueConsumer is a durable of the row's own on the subject the Dispatch events above are
// published to: the two rows that work a message outside the daemon's own consumers need one, and
// they differ only in their name and the window they give a delivery.
func issueConsumer(t *testing.T, stream jetstream.Stream, durable string, ackWait time.Duration) jetstream.Consumer {
	t.Helper()
	consumer, err := stream.CreateOrUpdateConsumer(context.Background(), jetstream.ConsumerConfig{
		Durable:       durable,
		FilterSubject: "notifications.dispatch.issue.>",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       ackWait,
	})
	if err != nil {
		t.Fatalf("create the %s consumer: %v", durable, err)
	}
	return consumer
}

// The fix: the first fact of a backlog is held for three acknowledgement windows, the daemon
// reports it in progress for as long as it holds, and JetStream still delivers every event of
// that backlog once and has every one of them prepared once.
func TestAFactHeldPastItsAcknowledgementWindowIsDeliveredOnceAndPreparedOnce(t *testing.T) {
	pool := migratedPool(t)
	js, stream := testJetStream(t)
	// Watched before the daemon opens, so no signal of the held fact's can be made before the
	// subscription that counts it exists.
	progress := watchProgressSignals(t, js)
	spec := consumerSpec(&lockedBuffer{})
	// The hold must cost the handling nothing but time: this row is the progress signal's, and the
	// limit that cuts a handler which never ends is R2's.
	spec.HandlerLimit = time.Minute
	var mu sync.Mutex
	prepared := map[string]int{}
	// The preparations are counted here and the fact is held in the handler instead, because
	// prepareRepository bounds this call at AckWait: a hold here would be cut and its message
	// naked, which is a nak's story, not this row's.
	spec.PrepareRepository = func(_ context.Context, issue DispatchIssue) (DispatchIssue, error) {
		mu.Lock()
		defer mu.Unlock()
		prepared[issue.Key]++
		return issue, nil
	}
	consumers, err := OpenConsumers(context.Background(), js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	publishHeldBacklog(t, js)

	held := newHeldOnce()
	defer runConsumers(t, consumers, handlerFunc(func(ctx context.Context, _ pgx.Tx, _ Fact) (Result, error) {
		held.hold(ctx)
		return Result{}, nil
	}))()
	held.releaseAfter(t, 3*spec.AckWait)

	testwait.Eventually(t, "every event of the held backlog prepared", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(prepared) == heldBacklog
	})
	assertNoAckPending(t, stream, dispatchConsumerName(spec.Project))

	// The signal itself, which is the assertion that fails every run when the keeper is gone. The
	// window opens one acknowledgement window after the hold began, so nothing made before the
	// held message could have expired is counted, and closes when the hold was freed. The keeper
	// ticks every third of AckWait and the window is the hold's last two of them, so about six
	// ticks fall inside it; the row asks for one, because one signal in that window is the whole
	// claim — the daemon reported a fact whose window had passed — and a loaded runner that delays
	// the other five says nothing about the fix.
	from, until := held.window()
	if until.IsZero() {
		t.Fatalf("the row's hold was never freed, so it has no window to count the daemon's progress signals in")
	}
	if signals := progress(from.Add(spec.AckWait), until); signals < 1 {
		t.Errorf("the daemon made no progress signal for the held fact between %s after it was held and its release %s later, want at least one: nothing reported it in progress past its acknowledgement window",
			spec.AckWait, until.Sub(from))
	}

	mu.Lock()
	defer mu.Unlock()
	for key, count := range prepared {
		if count != 1 {
			t.Errorf("%s prepared %d times, want once: its event was delivered again while the daemon was still working on the backlog", key, count)
		}
	}
	// Every delivery counts here, including a redelivered copy the consumer has not reached yet,
	// so a copy JetStream made and the daemon dropped as a duplicate would show even though no
	// second preparation did.
	item, err := stream.Consumer(context.Background(), dispatchConsumerName(spec.Project))
	if err != nil {
		t.Fatalf("read the Dispatch consumer: %v", err)
	}
	info, err := item.Info(context.Background())
	if err != nil {
		t.Fatalf("read the Dispatch consumer's info: %v", err)
	}
	if info.Delivered.Consumer != heldBacklog {
		t.Errorf("JetStream delivered %d messages for a backlog of %d, want one delivery each: the held fact was not reported in progress",
			info.Delivered.Consumer, heldBacklog)
	}
}

// The control: the same backlog, the same window and the same hold, on a consumer of the test's
// own that reports nothing in progress, and JetStream delivers the held message again. These are
// the two facts the row above denies.
func TestAFactHeldPastItsAcknowledgementWindowIsDeliveredTwiceWithNoProgressSignal(t *testing.T) {
	js, stream := testJetStream(t)
	const ackWait = 200 * time.Millisecond
	publishHeldBacklog(t, js)

	consumer := issueConsumer(t, stream, "held-control", ackWait)

	held := newHeldOnce()
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	entered := map[uint64]int{}
	var first uint64
	consuming, err := consumer.Consume(func(message jetstream.Msg) {
		metadata, err := message.Metadata()
		if err != nil {
			t.Errorf("read a control message's metadata: %v", err)
			return
		}
		mu.Lock()
		if first == 0 {
			first = metadata.Sequence.Stream
		}
		entered[metadata.Sequence.Stream]++
		mu.Unlock()
		held.hold(ctx)
		if err := message.Ack(); err != nil {
			t.Errorf("acknowledge a control message: %v", err)
		}
	}, jetstream.PullMaxMessages(heldBacklog+1))
	if err != nil {
		t.Fatalf("consume the control consumer: %v", err)
	}
	// The consumer is stopped and waited out before the test ends, so no delivery reaches a
	// handler that would report into a test already over; the hold is freed first, since Stop
	// waits for the handler holding a message to return.
	defer func() {
		consuming.Stop()
		<-consuming.Closed()
	}()
	defer cancel()
	held.releaseAfter(t, 3*ackWait)

	testwait.Eventually(t, "the control's first message delivered again and the rest of its backlog delivered", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return entered[first] >= 2 && len(entered) == heldBacklog
	})

	info, err := consumer.Info(context.Background())
	if err != nil {
		t.Fatalf("read the control consumer's info: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if entered[first] < 2 {
		t.Errorf("the control's handler was entered %d times with stream sequence %d, want at least twice: a message held past its window with no progress signal is delivered again",
			entered[first], first)
	}
	if info.Delivered.Consumer <= heldBacklog {
		t.Errorf("JetStream delivered %d messages for a backlog of %d with no progress signal, want more than the backlog: nothing held the redelivery off",
			info.Delivered.Consumer, heldBacklog)
	}
}

// What the three rows below prove together is Acceptance A2 of dispatch://INFRA-1530: the progress
// signal stops the moment the handler it was reporting for ends, it stops before the message's one
// terminal call so a nak keeps the retry it asked for, and a handler that never ends is cut at
// HandlerLimit with its message naked and delivered again.

// progressEvery is the keeper's interval in the row that drives it directly. It is short because
// that row waits out three of them and nothing else: the interval the daemon uses is a third of
// AckWait, which is the one caller's business, not this function's.
const progressEvery = 20 * time.Millisecond

// countedProgress is a delivery that counts the progress signals made for it and passes each one
// to the message it wraps, so a row can tell a tick the keeper made before stop() returned from
// one it made after. It holds every signal inside InProgress until blocks is closed, which is how
// its one row puts a tick in flight and keeps it there for as long as it has questions to ask
// about one. Every countedProgress is made with a blocks channel, so a row that wants none closes
// it before the keeper starts.
type countedProgress struct {
	jetstream.Msg
	mu      sync.Mutex
	signals int
	blocks  chan struct{}
}

func (c *countedProgress) InProgress() error {
	c.mu.Lock()
	c.signals++
	c.mu.Unlock()
	// The signal is counted before the wait, so a row watching reported() sees the tick it is
	// holding while it still holds it.
	<-c.blocks
	return c.Msg.InProgress()
}

func (c *countedProgress) reported() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.signals
}

// The first claim, on keepInProgress itself: stop() returns only once the keeper's goroutine has
// ended, so once it has returned no tick is in flight. The row drives the keeper directly rather
// than through a consumer loop, since that is all the function needs — a delivery, an interval
// and the stop it hands back. The delivery is real, and fetched rather than consumed, so each
// signal countedProgress passes on has a reply subject to go to.
func TestTheProgressSignalStopsWhenItsHandlerEnds(t *testing.T) {
	js, stream := testJetStream(t)
	publishIssueEvent(t, js, "CAPTURE-310", 1)
	// A window far longer than the row runs: a redelivery would hand the row a second message and
	// say nothing about the keeper.
	consumer := issueConsumer(t, stream, "progress-stop", time.Minute)
	message, err := consumer.Next()
	if err != nil {
		t.Fatalf("fetch the row's message: %v", err)
	}

	// The keeper's first tick is held inside InProgress until the row frees it, so the row asks
	// its question of a stop() that has a tick in flight for certain rather than of one that
	// happens to have caught a tick.
	counted := &countedProgress{Msg: message, blocks: make(chan struct{})}
	stop := keepInProgress(context.Background(), counted, progressEvery, slog.New(slog.NewTextHandler(&lockedBuffer{}, nil)))
	free := sync.OnceFunc(func() { close(counted.blocks) })
	// A wait that fails leaves the keeper running on a tick nothing freed, so the row frees the
	// tick first and ends the keeper after it: stop() waits for the goroutine that tick holds, and
	// the defers run in that order.
	defer stop()
	defer free()
	testwait.Eventually(t, "the keeper has reported the message in progress", func() bool { return counted.reported() > 0 })

	// stop() runs in a goroutine because what the row asks of it first is that it does not return.
	returned := make(chan struct{})
	go func() {
		stop()
		close(returned)
	}()
	// Three intervals: a stop() that waited for nothing would have returned many times over by
	// now, and the tick it would be leaving behind is still inside InProgress.
	select {
	case <-returned:
		t.Fatalf("stop() returned while a progress signal was still in flight, want it to wait for it: it returns only once the keeper's goroutine has ended, which is what lets its caller nak the message after it")
	case <-time.After(3 * progressEvery):
	}

	free()
	testwait.Eventually(t, "stop() returned once the signal it was waiting on was freed", func() bool {
		select {
		case <-returned:
			return true
		default:
			return false
		}
	})

	stopped := counted.reported()
	// Three intervals again: a keeper still running would have ticked three times over by now, and
	// one tick is enough to fail the row.
	time.Sleep(3 * progressEvery)
	if after := counted.reported(); after != stopped {
		t.Errorf("the keeper reported the message in progress %d more times after stop() returned, want none: stop returns only once its goroutine has ended, so no tick can still be in flight",
			after-stopped)
	}
}

// progressSignal is the acknowledgement-protocol payload a progress signal carries, the
// ackProgress of nats.go v1.53.1.
const progressSignal = "+WPI"

// watchProgressSignals answers how many messages the daemon's client reported in progress within a
// span, by watching the subject every acknowledgement of this server is published on. The wire is
// the only place a tick shows: nothing in the daemon's log says one was made, the consumer's info
// keeps no count of them, and the message's own state is the same either way. Every consumer of
// the server shares the subject, so what this counts is the daemon's client whole; that costs the
// two rows using it nothing, because each asks over a span in which the daemon has one message of
// the row's in hand and nothing else to work.
func watchProgressSignals(t *testing.T, js jetstream.JetStream) func(from, until time.Time) int {
	t.Helper()
	var mu sync.Mutex
	var made []time.Time
	subscription, err := js.Conn().Subscribe("$JS.ACK.>", func(signal *nats.Msg) {
		if string(signal.Data) != progressSignal {
			return
		}
		mu.Lock()
		made = append(made, time.Now())
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("watch the acknowledgement subject: %v", err)
	}
	// The connection this subscription is on is the test server's, which testJetStream closes when
	// the test ends, so a failure to unsubscribe here is that close and not the row's business.
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	return func(from, until time.Time) int {
		mu.Lock()
		defer mu.Unlock()
		count := 0
		for _, at := range made {
			if !at.Before(from) && !at.After(until) {
				count++
			}
		}
		return count
	}
}

// nakFailure is the line nakResolve writes at warn just before it naks a message whose handler
// returned an error (handleMessage's "retryable intake processing failure").
const nakFailure = "retryable intake processing failure"

// heldNak is the daemon's log with that one line held, which is how the row below widens the
// window between a handler's end and its message's nak. In the daemon that window is microseconds
// wide, so a keeper that outlived its handler would have to be caught inside it by luck; held for
// longer than the keeper's own interval, the window holds a tick of such a keeper with certainty,
// and holds none of a keeper stopped before the terminal call. It holds the nak named by holdsNak
// and no other, so the row's other retry measures the delay the nak asked for with nothing of the
// row's own added to it.
type heldNak struct {
	buffer lockedBuffer
	hold   time.Duration

	mu    sync.Mutex
	naks  int
	from  time.Time
	until time.Time
}

// holdsNak is which of the row's naks is held: the second, so the first retry is the one measured.
const holdsNak = 2

func (h *heldNak) Write(p []byte) (int, error) {
	written, err := h.buffer.Write(p)
	if !bytes.Contains(p, []byte(nakFailure)) {
		return written, err
	}
	h.mu.Lock()
	h.naks++
	holding := h.naks == holdsNak
	if holding {
		h.from = time.Now()
	}
	h.mu.Unlock()
	if !holding {
		return written, err
	}
	time.Sleep(h.hold)
	h.mu.Lock()
	h.until = time.Now()
	h.mu.Unlock()
	return written, err
}

// window is when the held nak's line was held from and until; both are zero until it has been.
func (h *heldNak) window() (time.Time, time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.from, h.until
}

// The second claim, end to end: the keeper stops before the message's one terminal call, so the
// nak keeps the delay it asked for. A tick that landed after a nak would reset the server's
// redelivery timer — nats-server's processNak keeps the entry in o.pending with a timestamp that
// expires after the nak's delay, and progressUpdate resets it to now — and the retry the nak asked
// for would become a whole AckWait.
//
// So the row measures that retry. It also watches the signals themselves, because the retry alone
// would not catch the fault this row is here for: a stop() left to a defer after the terminal
// call. nats.go marks a message acknowledged for a nak and refuses every later signal for it
// (ErrMsgAlreadyAckd, nats.go v1.53.1's (*jetStreamMsg).ackReply), so a keeper that outlives the
// nak publishes nothing and the retry stands anyway — on that client. What such a keeper does do
// is report the message in progress between its handler's end and the nak, which is where the row
// looks, and the one thing between the two the row can hold is the line nakResolve writes there
// (heldNak).
func TestTheProgressSignalStopsBeforeTheNakThatKeepsItsDelay(t *testing.T) {
	pool := migratedPool(t)
	js, stream := testJetStream(t)
	progress := watchProgressSignals(t, js)

	// The keeper ticks every third of AckWait, so a hold of a third and a bit covers a whole
	// interval whenever it starts.
	spec := consumerSpec(&lockedBuffer{})
	spec.AckWait, spec.NakDelay = 2*time.Second, 50*time.Millisecond
	// The row is the nak's, not the limit's: no handler here runs long enough to be cut.
	spec.HandlerLimit = time.Minute
	held := &heldNak{hold: spec.AckWait/3 + 100*time.Millisecond}
	spec.Logger = slog.New(slog.NewTextHandler(held, nil))
	consumers, err := OpenConsumers(context.Background(), js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	publishIssueEvent(t, js, "CAPTURE-320", 1)

	var mu sync.Mutex
	var entered []time.Time
	// The fact fails its first two deliveries and applies on the third, so the row ends on an
	// acknowledgement rather than retrying for ever.
	defer runConsumers(t, consumers, handlerFunc(func(_ context.Context, _ pgx.Tx, _ Fact) (Result, error) {
		mu.Lock()
		entered = append(entered, time.Now())
		delivery := len(entered)
		mu.Unlock()
		if delivery <= holdsNak {
			return Result{}, errors.New("a fact this row's handler refuses, so consumeMessage naks its message")
		}
		return Result{}, nil
	}))()

	testwait.Eventually(t, "the refused fact retried twice and then applied", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(entered) > holdsNak
	})
	assertNoAckPending(t, stream, dispatchConsumerName(spec.Project))

	mu.Lock()
	retry := entered[1].Sub(entered[0])
	mu.Unlock()
	// The margin: the delay the nak asked for is a fortieth of AckWait, and a late tick would make
	// this retry a whole AckWait. A quarter of AckWait is five times the delay, which a loaded
	// runner's scheduling does not cross, and four times under the window a late tick would cost.
	if retry > spec.AckWait/4 {
		t.Errorf("the naked fact was delivered again after %s, want nearer its %s nak delay than its %s acknowledgement window: a progress signal after the nak reset the server's redelivery timer",
			retry, spec.NakDelay, spec.AckWait)
	}
	from, until := held.window()
	if from.IsZero() || until.IsZero() {
		t.Fatalf("the row held no nak line, so nothing widened the window a keeper that outlived its handler would tick in")
	}
	if signals := progress(from, until); signals != 0 {
		t.Errorf("the daemon reported the message in progress %d times between its handler's end and its nak, want none: the keeper has to stop before the terminal call, not in a defer after it",
			signals)
	}
}

// The third claim: a handler that never ends is cut at HandlerLimit, its message naked and
// delivered again. The cut is what keeps a message reported in progress from being held for good,
// and it is all this row proves. While the handler holds the message the daemon has no pull
// request open (the comment above the first row of this file), so even a daemon that reported
// nothing in progress would not have this message delivered again before the cut: what the row
// measures is the cut itself, the redelivery the nak after it brings, and the attributes of the
// warn line that names the cut. It says nothing about the keeper.
func TestTheProgressSignalsHandlerIsCutAtItsLimit(t *testing.T) {
	pool := migratedPool(t)
	js, _ := testJetStream(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	// Three times the acknowledgement window, so the redelivery this row measures cannot be read
	// as the window's own, and a small fraction of the row's patience (testwait.Eventually waits a
	// minute), so a limit that never cuts fails the row instead of holding it.
	spec.HandlerLimit = 600 * time.Millisecond
	consumers, err := OpenConsumers(context.Background(), js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	publishIssueEvent(t, js, "CAPTURE-330", 1)

	held := newHeldOnce()
	var mu sync.Mutex
	var entered []time.Time
	stop := runConsumers(t, consumers, handlerFunc(func(ctx context.Context, _ pgx.Tx, _ Fact) (Result, error) {
		mu.Lock()
		entered = append(entered, time.Now())
		mu.Unlock()
		// The first delivery waits on the context consumeMessage passes and nothing releases it,
		// so it ends when and only when the limit cuts it; a handler sleeping on the clock would
		// ignore the cut and prove nothing. Every delivery after it returns at once.
		held.hold(ctx)
		return Result{}, nil
	}))
	defer stop()

	testwait.Eventually(t, "the cut fact delivered again", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(entered) >= 2
	})
	// The consumers stop on the second delivery, so the row ends there rather than working the
	// fact again.
	stop()

	mu.Lock()
	cut := entered[1].Sub(entered[0])
	mu.Unlock()
	if cut < spec.HandlerLimit/2 || cut > 2*spec.HandlerLimit {
		t.Errorf("the handler that never ends was cut and its fact delivered again %s after its first delivery, want about its %s handler limit plus the %s the nak after the cut asks for: the limit is the only thing that ends this handling",
			cut, spec.HandlerLimit, spec.NakDelay)
	}
	line := logLine(logs.String(), "an intake handler outran its limit and was cut")
	if line == "" {
		t.Fatalf("no line says an intake handler outran its limit and was cut, in:\n%s", logs.String())
	}
	for _, attribute := range []string{"consumer=", "event_id=", "fact=intake.DispatchIssue", "limit=" + spec.HandlerLimit.String()} {
		if !strings.Contains(line, attribute) {
			t.Errorf("the cut handler's line carries no %s, which is what names the cut: %s", attribute, line)
		}
	}
}

// heldFactLine is the line consumeMessage writes for a fact whose handling outlived the
// acknowledgement window, which the row below is here for.
const heldFactLine = "an intake fact outlived its acknowledgement window and was held in progress while it ran"

// This line is the only thing in the daemon that reports how long a fact took: nothing else times
// one, and the consumer-lag warning beside it (lagWatch) speaks five minutes late and names no
// duration. The issue's after-merge check reads the line in production
// (dispatch://INFRA-1530), so a change that dropped it, or that left its attributes behind, would
// go unnoticed until that deploy; hence a row of its own, under the name the check selects.
func TestAFactHeldPastItsAcknowledgementWindowSaysSoOnceWithHowLongItTook(t *testing.T) {
	pool := migratedPool(t)
	js, stream := testJetStream(t)
	logs := &lockedBuffer{}
	spec := consumerSpec(logs)
	// Far above the hold below, so the limit cuts nothing: a cut handler is the warn line beside
	// this one, and that is the row above's claim.
	spec.HandlerLimit = time.Minute
	consumers, err := OpenConsumers(context.Background(), js, pool, spec)
	if err != nil {
		t.Fatalf("OpenConsumers: %v", err)
	}
	// One fact, so the one line the row counts is the one fact's.
	publishIssueEvent(t, js, "CAPTURE-340", 1)

	held := newHeldOnce()
	defer runConsumers(t, consumers, handlerFunc(func(ctx context.Context, _ pgx.Tx, _ Fact) (Result, error) {
		held.hold(ctx)
		return Result{}, nil
	}))()
	// The hold the rows above put on the same window: three of them, so the handling outlasts the
	// window by a margin no scheduling delay closes. releaseAfter returns once the fact is held,
	// so the delivery has happened by the time the waits below start.
	held.releaseAfter(t, 3*spec.AckWait)

	// consumeMessage writes the line between the handler's end and the one acknowledgement, so a
	// consumer with nothing left to acknowledge has written it. Waiting on the line itself would
	// turn a dropped line into a timeout instead of the failure below.
	assertNoAckPending(t, stream, dispatchConsumerName(spec.Project))

	written := logs.String()
	line := logLine(written, heldFactLine)
	if line == "" {
		t.Fatalf("no line says a fact outlived its acknowledgement window, and one was held for %s of a %s window, in:\n%s",
			3*spec.AckWait, spec.AckWait, written)
	}
	if said := strings.Count(written, heldFactLine); said != 1 {
		t.Errorf("%d lines say a fact outlived its acknowledgement window, want one for the one fact: the line is the fact's, not the progress keeper's tick's, in:\n%s", said, written)
	}
	// How long it took is a measurement, so the row asks that it is reported and not what it
	// reads.
	for _, attribute := range []string{"consumer=", "event_id=", "delivered=", "took="} {
		if !strings.Contains(line, attribute) {
			t.Errorf("the held fact's line carries no %s, which is what an operator reads it for: %s", attribute, line)
		}
	}
}
