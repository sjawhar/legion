package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// hangingSeenKV is the listener state bucket as a pod reads it over a bus that went silent
// without closing the connection: every read blocks until the test releases it.
type hangingSeenKV struct {
	natsgo.KeyValue
	released <-chan struct{}
}

func (k hangingSeenKV) Get(string) (natsgo.KeyValueEntry, error) {
	<-k.released
	return nil, natsgo.ErrKeyNotFound
}

// GET /v1/interests/ reads each lapsed row's last heartbeat from the listener state bucket
// (interestRowFor). A bucket that stopped answering must cost the answer the route's own budget
// and no more: the rows still list, without a last heartbeat for the ones it could not read.
// Before this change that read had no budget at all, so the whole list waited out the JetStream
// MaxWait once per lapsed row, well past this listener's HTTP write timeout.
func TestTheInterestsListAnswersWithinItsBudgetWhileTheLastSeenBucketHangs(t *testing.T) {
	const lapsed = "ses_interests_last_seen_hangs"
	released := make(chan struct{})
	t.Cleanup(func() { close(released) })
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener's bus: %v", err)
	}
	t.Cleanup(client.Close)
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the interest registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	state, err := openListenerState(client.Conn, 1, listenerStateRequestWait)
	if err != nil {
		t.Fatalf("open the listener state bucket: %v", err)
	}
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1),
		session.WithLastSeenBucket(bus.KeyValue{KeyValue: hangingSeenKV{KeyValue: state.KeyValue, released: released}}))
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)

	// The session never registered, so the session cache misses it and the row's last heartbeat
	// is read from the bucket, as a lapsed session's is.
	if _, err := registry.Upsert(store.Interest{SessionID: lapsed, MachineID: "test-machine", Dir: "/test"},
		[]string{contracts.AgentSubject(lapsed), "notifications.test.>"}); err != nil {
		t.Fatalf("write %s's interest record: %v", lapsed, err)
	}
	waitFor(t, 5*time.Second, "the interest cache to hold the lapsed row", func() bool {
		return slices.ContainsFunc(registry.List(), func(item store.Interest) bool { return item.SessionID == lapsed })
	})

	answered := make(chan *httptest.ResponseRecorder, 1)
	started := time.Now()
	go func() {
		recorder := httptest.NewRecorder()
		adminInterestsHandler(registry, sessions, nil).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/interests/", nil))
		answered <- recorder
	}()
	var recorder *httptest.ResponseRecorder
	select {
	case recorder = <-answered:
	case <-time.After(sendRequestBudget + 5*time.Second):
		t.Fatalf("GET /v1/interests/ did not answer within %s while the last-seen bucket hung, its %s budget long past",
			sendRequestBudget+5*time.Second, sendRequestBudget)
	}
	if took := time.Since(started); took > sendRequestBudget+2*time.Second {
		t.Fatalf("GET /v1/interests/ answered after %s, want it inside its %s budget", took, sendRequestBudget)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /v1/interests/ = %d %s, want 200", recorder.Code, recorder.Body.String())
	}
	var rows []interestRow
	if err := json.NewDecoder(recorder.Body).Decode(&rows); err != nil {
		t.Fatalf("decode the list: %v", err)
	}
	if len(rows) != 1 || rows[0].SessionID != lapsed || rows[0].LastSeen != 0 {
		t.Fatalf("the list answered %+v, want the lapsed row alone with no last heartbeat", rows)
	}
}
