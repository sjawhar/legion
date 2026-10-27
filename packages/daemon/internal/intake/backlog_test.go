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

// slowFacts is a handler whose fact costs work, as every fact does: it takes the one fact lock and
// writes records.
type slowFacts struct{ work time.Duration }

func (s slowFacts) Apply(context.Context, pgx.Tx, Fact) (Result, error) {
	time.Sleep(s.work)
	return Result{}, nil
}

// A consumer working a backlog takes each message's acknowledgement window for that message's own
// work. JetStream starts that window when it delivers a message, not when this loop reaches it, so
// a message the client library fetched into its buffer spends the window queued behind the ones
// before it: with the library's default batch of 500, every message past the first few is
// redelivered while the consumer is still working, and each redelivery is prepared again — the
// Dispatch read an unrecorded issue's event makes — and takes the fact lock again before it is
// found already processed. That is work a consumer that is already behind pays instead of draining.
//
// Here twelve events are published at once, each fact costs 300 ms and the window is 2 s, so a
// buffering consumer would hold the last of them for 3.6 s and have JetStream redeliver it. Every
// event is prepared exactly once.
func TestABacklogIsDeliveredOnceWhileEachFactTakesItsAcknowledgementWindow(t *testing.T) {
	pool := migratedPool(t)
	js, stream := testJetStream(t)
	spec := consumerSpec(&lockedBuffer{})
	spec.AckWait = 2 * time.Second
	var mu sync.Mutex
	prepared := map[string]int{}
	spec.PrepareRepository = func(_ context.Context, issue DispatchIssue) (DispatchIssue, error) {
		mu.Lock()
		defer mu.Unlock()
		prepared[issue.Key]++
		return issue, nil
	}
	consumers, err := OpenConsumers(context.Background(), js, spec)
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

	defer runConsumers(t, consumers, pool, slowFacts{work: 300 * time.Millisecond})()
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
			t.Errorf("%s prepared %d times, want once: the consumer held it past its acknowledgement window", key, count)
		}
	}
	// Every delivery counts here, a redelivered copy the consumer has not reached yet included, so
	// this is the whole backlog's cost rather than what the preparations show at this moment.
	item, err := stream.Consumer(context.Background(), dispatchConsumerName(spec.Project))
	if err != nil {
		t.Fatalf("read the Dispatch consumer: %v", err)
	}
	info, err := item.Info(context.Background())
	if err != nil {
		t.Fatalf("read the Dispatch consumer's info: %v", err)
	}
	if info.Delivered.Consumer != backlog {
		t.Errorf("JetStream delivered %d messages for a backlog of %d, want one delivery each", info.Delivered.Consumer, backlog)
	}
}
