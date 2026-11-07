package intake

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// What the two rows here prove together is Acceptance A1 of dispatch://INFRA-1530: a fact held
// well past its acknowledgement window is delivered once and prepared once, because consumeMessage
// reports it in progress while its handler runs (keepInProgress).
//
// The first row is that claim, on the daemon's own consumers. The second is the control, and it is
// what makes the first row's pass mean something: it holds a message of its own for the same time,
// on a consumer with the same acknowledgement window and no progress signal at all, and JetStream
// delivers that message again. Without the control, the first row would pass just as well on timing
// that never produced a redelivery, and would prove nothing about the signal.
//
// Both rows hold the first of a backlog of three events rather than one message alone. A held
// message's redelivery is served to a pull request of the client's that is still open: the daemon
// asks for one message at a time (jetstream.PullMaxMessages(1) in consumeConsumer), so while its
// handler holds a message it has no request open, and the redelivery waits for the request the
// library sends after the handler returns — just after the acknowledgement the handler's end sent,
// which the server usually takes first. That race is why a one-message row is not a test: measured
// here, one run of six lost the redelivery entirely. A backlog is what the daemon pays the cost on
// in production as well (dispatch://INFRA-456), and it lets the first row assert what A1 asks of
// every event rather than of one.
//
// The control asks for one message more than the backlog holds, so its own request is still open
// when the window expires and the redelivery lands in it the moment the server makes it, in every
// run rather than in most of them. That is the one way the control's client differs from the
// daemon's, and it differs there so the control is a fact rather than a coin toss.

// heldBacklog is how many Dispatch issue events each row publishes, each under a key of its own.
const heldBacklog = 3

// heldOnce holds the first delivery that reaches it until the test releases it, and lets every
// later one straight through. Both rows hold their first delivery this way, so both rows put the
// same hold on the same window.
type heldOnce struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
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
	timer := time.AfterFunc(wait, func() { close(h.release) })
	t.Cleanup(func() { timer.Stop() })
}

// publishHeldBacklog publishes the rows' backlog on the Dispatch durable's subject: heldBacklog
// issue events, each naming an issue of its own, so a second preparation of any of them shows as
// that issue's own second preparation.
func publishHeldBacklog(t *testing.T, js jetstream.JetStream) {
	t.Helper()
	for i := range heldBacklog {
		key := fmt.Sprintf("CAPTURE-%d", 300+i)
		publish(t, js, "notifications.dispatch.issue."+key+".issue.updated", envelopeJSON(t, fmt.Sprintf("held-%d", i+1), "dispatch",
			fmt.Sprintf(`{"id":%d,"issue_key":%q,"seq":1,"notify":true,"type":"issue.updated","actor":{"kind":"user","id":"someone"},`+
				`"payload":{"key":%q,"status":"backlog","title":"a held event","parent":null,"rank":"A","labels":[],"resolved_repo":"sjawhar/legion"}}`, i+1, key, key)))
	}
}

// The fix: the first fact of a backlog is held for three acknowledgement windows, and JetStream
// still delivers every event of that backlog once and has every one of them prepared once.
func TestAFactHeldPastItsAcknowledgementWindowIsDeliveredOnceAndPreparedOnce(t *testing.T) {
	pool := migratedPool(t)
	js, stream := testJetStream(t)
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

	consumer, err := stream.CreateOrUpdateConsumer(context.Background(), jetstream.ConsumerConfig{
		Durable:       "held-control",
		FilterSubject: "notifications.dispatch.issue.>",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       ackWait,
	})
	if err != nil {
		t.Fatalf("create the control consumer: %v", err)
	}

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
