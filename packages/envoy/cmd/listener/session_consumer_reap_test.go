package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// sessionConsumerReapRig is the listener's /v1 routes over the in-process server, with the
// session-consumer connection main opens (connectSessionDelivery), the session windows these
// tests cross - a minute live, a two-minute gone horizon, and a bucket whose maximum age outlives
// both, which is the deployed shape - and a clock the test moves rather than waits out. consumers
// is the test's own view of the session consumers, so what it reads of them is what the server
// holds and not what the listener believes.
type sessionConsumerReapRig struct {
	sessions  *session.SessionRegistry
	delivery  *sessionDelivery
	deps      *listenerDeps
	consumers *bus.SessionConsumers
	mux       *http.ServeMux
	now       *stateClock
}

func newSessionConsumerReapRig(t *testing.T) *sessionConsumerReapRig {
	t.Helper()
	url := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{url}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener: %v", err)
	}
	t.Cleanup(client.Close)
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the interest registry: %v", err)
	}
	waitForRegistry(t, registry)
	now := &stateClock{}
	sessions, err := session.OpenSessionRegistry(
		client.Conn,
		session.WithSessionReplicas(1),
		session.WithSessionTTL(time.Hour),
		session.WithSessionClock(now.Now),
		session.WithSessionLiveWindow(time.Minute),
		session.WithSessionGoneHorizon(2*time.Minute),
	)
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	waitForSessions(t, sessions)
	logger := logging.NewWithWriter("test", io.Discard)
	delivery, err := connectSessionDelivery([]string{url}, logger)
	if err != nil {
		t.Fatalf("the session-consumer connection did not open: %v", err)
	}
	t.Cleanup(delivery.close)
	delivery.relay = &recordingRelay{}
	consumers, err := bus.ConnectSessionConsumers([]string{url})
	if err != nil {
		t.Fatalf("connect the test's view of the session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)
	mux := http.NewServeMux()
	deps := &listenerDeps{client: client, registry: registry, sessions: sessions, consumers: delivery}
	registerV1Routes(mux, deps, "test-machine", logger)
	return &sessionConsumerReapRig{sessions: sessions, delivery: delivery, deps: deps, consumers: consumers, mux: mux, now: now}
}

// register posts sessionID's registration as its plugin's heartbeat does, so the listener creates
// the session's consumer and writes its record, and returns the answer the session reads, whose
// delivery field names what it is delivered to through.
func (rig *sessionConsumerReapRig) register(t *testing.T, sessionID string) subscribeResponse {
	t.Helper()
	recorder := httptest.NewRecorder()
	body := strings.NewReader(consumerRegistration(sessionID, "e"))
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", body))
	if recorder.Code != http.StatusOK {
		t.Fatalf("register %s: status %d: %s", sessionID, recorder.Code, recorder.Body.String())
	}
	var answer subscribeResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode the answer to %s's registration: %v: %s", sessionID, err, recorder.Body.String())
	}
	return answer
}

// waitForRecordDeleted waits, reading the bucket rather than the cache, until sessionID has no
// record there (session.SessionRegistry.Refresh). It is how a test holding a registration open
// waits for the reaper pass it started to have deleted the record that registration is replacing,
// so the pass's consumer delete is one the registration really raced. A deadline reached is
// reported and not fatal: the caller holds the session's lock, which a fatal would never release,
// and the pass that deleted nothing fails the test on its own count.
func (rig *sessionConsumerReapRig) waitForRecordDeleted(t *testing.T, sessionID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		_, err := rig.sessions.Refresh(ctx, sessionID)
		if errors.Is(err, nats.ErrKeyNotFound) {
			return
		}
		if ctx.Err() != nil {
			t.Errorf("the record of %s is still in the bucket (%v); want the pass to have deleted it", sessionID, err)
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// requireConsumer fails the test unless the stream holds sessionID's consumer, when saying when it
// was read.
func (rig *sessionConsumerReapRig) requireConsumer(t *testing.T, sessionID, when string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := rig.consumers.Info(ctx, sessionID); err != nil {
		t.Fatalf("the consumer of %s %s: %v; want the stream holding it", sessionID, when, err)
	}
}

// reap runs one pass of the session reaper wired as main wires it (main.go hands the pass a
// delete over the listener's own session consumers and stores) and returns the records it
// deleted.
func (rig *sessionConsumerReapRig) reap(t *testing.T) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return rig.sessions.ReapSessions(ctx, session.SessionReaper{
		DeleteConsumer: func(ctx context.Context, sessionID string) error {
			return rig.delivery.deleteConsumer(ctx, rig.deps, sessionID)
		},
	})
}

// A session's consumer dies with its session: the pass that deletes the record of a session past
// the gone horizon deletes that session's consumer too, on the listener's own connection and by
// the name the listener created it under, so the stream holds a consumer for each session the
// deployment has rather than for each it has ever relayed. A session inside the horizon keeps its
// consumer and the position that consumer holds, whether it is live or has stopped answering: its
// record stands, so it is receiving through that consumer the moment it answers again.
func TestAReaperPassDeletesTheReapedSessionsConsumerAndKeepsTheOthers(t *testing.T) {
	rig := newSessionConsumerReapRig(t)
	rig.register(t, "ses_reaped")
	rig.now.advance(90 * time.Second)
	rig.register(t, "ses_stalled")
	rig.now.advance(60 * time.Second)
	rig.register(t, "ses_live")
	// ses_reaped is 2m30s old (past the 2m horizon), ses_stalled 1m (past the 1m live window,
	// inside the horizon) and ses_live has just registered.
	if _, state := rig.sessions.Lookup("ses_stalled"); state != session.Unresponsive {
		t.Fatalf("ses_stalled = %s, want %s", state, session.Unresponsive)
	}
	for _, sessionID := range []string{"ses_reaped", "ses_stalled", "ses_live"} {
		rig.requireConsumer(t, sessionID, "after its registration")
	}

	if reaped := rig.reap(t); reaped != 1 {
		t.Fatalf("the pass reaped %d records, want the one past the gone horizon", reaped)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if info, err := rig.consumers.Info(ctx, "ses_reaped"); !errors.Is(err, bus.ErrSessionConsumerNotFound) {
		t.Fatalf("the consumer of ses_reaped after the pass: %+v, %v; want it deleted with the session's record", info, err)
	}
	rig.requireConsumer(t, "ses_stalled", "after the pass that reaped ses_reaped")
	rig.requireConsumer(t, "ses_live", "after the pass that reaped ses_reaped")
}

// A registration that lands while the reaper deletes its session's record keeps the consumer it
// is answered with. The pass judged the record the session left behind, not the one its
// registration writes, so the consumer it deletes afterwards is the one that registration just
// created, and a session told it is delivered to through a consumer that no longer exists
// receives nothing until its next heartbeat recreates one at its resume point, a pause of up to
// the heartbeat's own period with the position the consumer held lost.
//
// The interleaving is built rather than waited for: afterRegisterFilter runs under the session's
// lock, after the registration's consumer call and before the row and the entry it writes, and
// from there the test runs the pass and waits for its record delete. So the pass deletes the
// record of a session that is registering again, and goes on to delete that session's consumer
// with the registration already holding one.
func TestARegistrationThatRacesTheReapOfItsRecordKeepsItsConsumer(t *testing.T) {
	rig := newSessionConsumerReapRig(t)
	rig.register(t, "ses_back")
	// Past the two-minute gone horizon, so the pass below judges the record this session's next
	// registration replaces.
	rig.now.advance(3 * time.Minute)
	rig.requireConsumer(t, "ses_back", "after its first registration")

	reaped := make(chan int, 1)
	var raced sync.Once
	rig.delivery.afterRegisterFilter = func() {
		raced.Do(func() {
			go func() { reaped <- rig.reap(t) }()
			rig.waitForRecordDeleted(t, "ses_back")
		})
	}
	answer := rig.register(t, "ses_back")

	if answer.Delivery == nil || answer.Delivery.Mode != deliveryModeConsumer {
		t.Fatalf("the registration that raced the pass was answered delivery %+v, want %s",
			answer.Delivery, deliveryModeConsumer)
	}
	if records := <-reaped; records != 1 {
		t.Fatalf("the pass deleted %d records, want the one it judged gone", records)
	}
	rig.requireConsumer(t, "ses_back", "after the pass that deleted the record it was registering over")
	if _, state := rig.sessions.Lookup("ses_back"); state != session.Live {
		t.Fatalf("ses_back is %s after the registration that raced the pass, want %s", state, session.Live)
	}
}
