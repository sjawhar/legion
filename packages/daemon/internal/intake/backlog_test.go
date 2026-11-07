package intake

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// heldFacts is a handler whose fact costs work, as every fact does — it takes the one fact lock
// and writes records — but whose work the test ends rather than the clock: every Apply reports
// that it has a fact in hand and then waits for release, so the test can read what the consumer
// owes while a fact is still in hand instead of racing a sleep against the acknowledgement window.
type heldFacts struct {
	// started carries the name of the first fact the handler is handed, which the failure message
	// below names. It is buffered for that one fact, and every later Apply leaves it as it is.
	started chan string
	release chan struct{}
}

func (h *heldFacts) Apply(_ context.Context, _ pgx.Tx, fact Fact) (Result, error) {
	name := fmt.Sprintf("%T", fact)
	if issue, ok := fact.(DispatchIssue); ok {
		name = issue.Key
	}
	select {
	case h.started <- name:
	default:
	}
	<-h.release
	return Result{}, nil
}

// A consumer working a backlog takes each message's acknowledgement window for that message's own
// work. JetStream starts that window when it delivers a message, not when the consumer's loop
// reaches it, so a message the client library fetched into its buffer spends the window queued
// behind the ones before it: with the library's default batch of 500, every message past the first
// few is redelivered while the consumer is still working, and each redelivery is prepared again —
// the Dispatch read an unrecorded issue's event makes — and takes the fact lock again before it is
// found already processed. That is work a consumer that is already behind pays instead of draining.
//
// So this holds the fact handler on the first event of a backlog of twelve and, while it is held,
// reads what JetStream says the consumer owes an acknowledgement for: a consumer that fetches
// consumerFetchBatch messages owes no more than that however long a fact takes, and nothing it
// holds can outlive its window behind another message's work. That held read is what guards the
// fetch batch; the preparation count is not the same statement counted at the other end, since
// once the handler is released the facts finish in milliseconds and a larger batch would pass it.
// Every event prepared exactly once says only that nothing was redelivered or applied twice over
// the run.
func TestABacklogIsDeliveredOnceWhileEachFactTakesItsAcknowledgementWindow(t *testing.T) {
	pool := migratedPool(t)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	// A hang guard, not the subject: a hold longer than this window has the held fact delivered
	// again and prepared twice, which is what the preparation count below reads as a fault.
	spec.AckWait = time.Minute
	var mu sync.Mutex
	prepared := map[string]int{}
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
	const backlog = 12
	for i := range backlog {
		key := fmt.Sprintf("CAPTURE-%d", 100+i)
		publish(t, js, "notifications.dispatch.issue."+key+".issue.updated", envelopeJSON(t, fmt.Sprintf("dispatch-%d", i+1), "dispatch",
			fmt.Sprintf(`{"id":%d,"issue_key":%q,"seq":1,"notify":true,"type":"issue.updated","actor":{"kind":"user","id":"someone"},`+
				`"payload":{"key":%q,"status":"backlog","title":"a backlog event","parent":null,"rank":"A","labels":[],"resolved_repo":"sjawhar/legion"}}`, i+1, key, key)))
	}

	facts := &heldFacts{started: make(chan string, 1), release: make(chan struct{})}
	defer runConsumers(t, consumers, facts)()
	// The handler is released before the consumers are stopped, whatever ends the test: a stop
	// waits for Run to return, and Run cannot return while a handler of its own holds a fact.
	release := sync.OnceFunc(func() { close(facts.release) })
	defer release()

	var held string
	testwait.Eventually(t, "the handler holding the backlog's first fact", func() bool {
		select {
		case held = <-facts.started:
			return true
		default:
			return false
		}
	})
	item, err := stream.Consumer(context.Background(), dispatchConsumerName(spec.Project))
	if err != nil {
		t.Fatalf("read the Dispatch consumer: %v", err)
	}
	info, err := item.Info(context.Background())
	if err != nil {
		t.Fatalf("read the Dispatch consumer's info: %v", err)
	}
	// The literal is deliberate: this row is what guards consumerFetchBatch, so it must not be
	// written in terms of the constant it guards - raising the constant would raise this bar with
	// it and the row would pass whatever the library then holds.
	if info.NumAckPending > 1 {
		t.Fatalf("the Dispatch consumer owes an acknowledgement for %d messages while the handler holds %s, want at most the one it fetches: the rest spend their window queued",
			info.NumAckPending, held)
	}

	release()
	testwait.Eventually(t, "every event of the backlog prepared", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(prepared) == backlog
	})
	assertNoAckPending(t, stream, dispatchConsumerName(spec.Project))

	mu.Lock()
	defer mu.Unlock()
	for key, count := range prepared {
		if count != 1 {
			t.Errorf("%s prepared %d times, want once: a redelivery prepared it again, so the consumer held it past its acknowledgement window", key, count)
		}
	}
}
