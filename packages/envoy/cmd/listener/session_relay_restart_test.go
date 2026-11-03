package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// errCreateFault is the error the tests' create fault fails a consumer create with.
var errCreateFault = errors.New("create fault injected by the test")

// failCreates makes the next n creates of sessionID's consumer through the rig's session-consumer
// connection (the relay's and the registrations') fail unsent, and returns a channel that hears
// each failure as it happens: by then the restart that made the create has deleted the consumer.
func failCreates(t *testing.T, rig *relayRig, sessionID string, n int) <-chan struct{} {
	t.Helper()
	name, _ := bus.SessionConsumerName(sessionID)
	failed := make(chan struct{}, n)
	var mu sync.Mutex
	left := n
	consumers := rig.deps.consumers.consumers
	consumers.SetCreateFault(func(consumer string) error {
		mu.Lock()
		defer mu.Unlock()
		if consumer != name || left == 0 {
			return nil
		}
		left--
		failed <- struct{}{}
		return errCreateFault
	})
	t.Cleanup(func() { consumers.SetCreateFault(nil) })
	return failed
}

// awaitCreateFailure waits up to 10 s for the next failure failCreates reports.
func awaitCreateFailure(t *testing.T, failed <-chan struct{}) {
	t.Helper()
	select {
	case <-failed:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay's restart never reached its create")
	}
}

// requireNoConsumer fails the test unless sessionID has no consumer.
func requireNoConsumer(t *testing.T, consumers *bus.SessionConsumers, sessionID string) {
	t.Helper()
	if info, err := consumers.Info(context.Background(), sessionID); !errors.Is(err, bus.ErrSessionConsumerNotFound) {
		t.Fatalf("the consumer of %s after its restart's create failed: %+v, %v; want it deleted", sessionID, info, err)
	}
}

// sessionWithAHeldFrame registers a session in consumer mode on a new relay rig, has it write one
// frame (done) and then hold the next unwritten (received, no done), as a session mid-turn does,
// so its consumer has a message awaiting an ack and the relay's next restart deletes and creates
// it. It returns the rig, the session, its stand-in, the held message and the ack floor.
func sessionWithAHeldFrame(t *testing.T, prefix string) (*relayRig, string, *sessionStandIn, contracts.Envelope, uint64) {
	t.Helper()
	rig := newRelayRig(t)
	sessionID := prefix + id.New()
	agent := contracts.AgentSubject(sessionID)
	var holdNext atomic.Bool
	standIn := startSessionStandIn(t, rig.url, sessionID, func(frame relayedFrame) []string {
		if holdNext.CompareAndSwap(true, false) {
			return []string{relayReplyReceived}
		}
		return answerDone(frame)
	})
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	written, floor := rig.storeEvent(t, agent)
	requireFrames(t, standIn.collect(t, 1, 5*time.Second), written)
	awaitAcked(t, rig.consumers, sessionID, floor)
	holdNext.Store(true)
	held, _ := rig.storeEvent(t, agent)
	requireFrames(t, standIn.collect(t, 1, 5*time.Second), held)
	awaitAckState(t, rig.consumers, sessionID, 1, floor)
	return rig, sessionID, standIn, held, floor
}

// A restart whose create fails after its delete succeeded loses nothing: the relay, retrying while
// it owns the session, creates the consumer again at the floor the restart read, with no new
// registration, and the session receives the held message again and every message stored while the
// consumer was missing, once and in stream order. Without the pending start the retry found no
// consumer and paused the relay until the next heartbeat, whose create started at the stream's end.
func TestSessionConsumerRestartWhoseCreateFailsRecreatesTheConsumerAtTheFloor(t *testing.T) {
	rig, sessionID, standIn, held, floor := sessionWithAHeldFrame(t, "ses_create_failed_")
	agent := contracts.AgentSubject(sessionID)

	// The relay's first create and its first retry's (1 s on) fail; its second retry, 3 s on,
	// creates.
	failed := failCreates(t, rig, sessionID, 2)
	// A new epoch restarts the relay at the ack floor: it reads the floor and deletes the consumer.
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-2"))
	awaitCreateFailure(t, failed)
	requireNoConsumer(t, rig.consumers, sessionID)
	missed := []contracts.Envelope{held}
	for range 3 {
		item, _ := rig.storeEvent(t, agent)
		missed = append(missed, item)
	}
	requireFrames(t, standIn.collect(t, 4, 10*time.Second), missed...)
	standIn.none(t, time.Second)
	info, err := rig.consumers.Info(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read the consumer the relay created again: %v", err)
	}
	if info.Config.OptStartSeq != floor+1 {
		t.Fatalf("the relay created the consumer again at %d, want the floor's next sequence %d", info.Config.OptStartSeq, floor+1)
	}
}

// A registration that arrives while a restart's failed create left the consumer missing creates it
// at the start that restart read, not at the stream's end (the registration carries no resume
// point), so the session receives the held message and the messages stored meanwhile.
func TestSessionConsumerRegistrationWhileARestartLeftNoConsumerCreatesItAtTheFloor(t *testing.T) {
	rig, sessionID, standIn, held, floor := sessionWithAHeldFrame(t, "ses_registered_in_gap_")
	agent := contracts.AgentSubject(sessionID)

	failed := failCreates(t, rig, sessionID, 1)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-2"))
	awaitCreateFailure(t, failed)
	requireNoConsumer(t, rig.consumers, sessionID)
	backlog := []contracts.Envelope{held}
	for range 3 {
		item, _ := rig.storeEvent(t, agent)
		backlog = append(backlog, item)
	}
	// The session's next heartbeat, well inside the relay's first retry wait (1 s).
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-2"))
	info, err := rig.consumers.Info(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("read the consumer the registration created: %v", err)
	}
	if info.Config.OptStartSeq != floor+1 {
		t.Fatalf("the registration created the consumer at %d, want the floor's next sequence %d, not the stream's end", info.Config.OptStartSeq, floor+1)
	}

	requireFrames(t, standIn.collect(t, 4, 10*time.Second), backlog...)
	standIn.none(t, time.Second)
}

// burstSessions is how many sessions TestSessionConsumerBurstOfRestartsDeliversEveryBacklog
// relays: enough that their restarts, all at once at the listener's start, overrun the server
// without relayRestartLimit, and few enough that the test runs in well under 30 s on 2 CPUs.
const burstSessions = 200

// burstFrame is one relay frame a burstStandIn received.
type burstFrame struct {
	sessionID string
	eventID   string
	seq       uint64
}

// burstStandIn answers every relay frame of every session received and done at once on one NATS
// connection, as many plugins in consumer mode do, and records each.
type burstStandIn struct {
	frames chan burstFrame
}

func startBurstStandIn(t *testing.T, url string, capacity int) *burstStandIn {
	t.Helper()
	conn := testnats.Connect(t, url)
	t.Cleanup(conn.Close)
	standIn := &burstStandIn{frames: make(chan burstFrame, capacity)}
	prefix := bus.DeliverSubjectPrefix
	if _, err := conn.Subscribe(prefix+"*", func(msg *natsgo.Msg) {
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err != nil {
			t.Errorf("decode a frame on %s: %v", msg.Subject, err)
			return
		}
		seq, err := strconv.ParseUint(msg.Header.Get(relayHeaderStreamSeq), 10, 64)
		if err != nil {
			t.Errorf("a frame on %s carries stream sequence %q: %v", msg.Subject, msg.Header.Get(relayHeaderStreamSeq), err)
		}
		for _, reply := range []string{relayReplyReceived, relayReplyDone} {
			if err := conn.Publish(msg.Reply, []byte(reply)); err != nil {
				t.Errorf("reply %s to a frame on %s: %v", reply, msg.Subject, err)
			}
		}
		standIn.frames <- burstFrame{sessionID: strings.TrimPrefix(msg.Subject, prefix), eventID: item.EventID, seq: seq}
	}); err != nil {
		t.Fatalf("subscribe the stand-in to every relay subject: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the stand-in's subscription: %v", err)
	}
	return standIn
}

// registerBurst registers every session with the listener on port in consumer mode, a few at a
// time, with no resume point, and fails the test unless each answer says consumer.
func registerBurst(t *testing.T, port int, sessionIDs []string) {
	t.Helper()
	work := make(chan string)
	errs := make(chan error, len(sessionIDs))
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for sessionID := range work {
				errs <- registerConsumerSession(port, sessionID)
			}
		})
	}
	for _, sessionID := range sessionIDs {
		work <- sessionID
	}
	close(work)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// registerConsumerSession registers sessionID with the listener on port in consumer mode and
// reports an answer that is not 200 with consumer delivery.
func registerConsumerSession(port int, sessionID string) error {
	request, err := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/interests/subscribe", strings.NewReader(consumerRegistration(sessionID, "epoch-1")))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+listenerTestToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("register %s: %w", sessionID, err)
	}
	defer response.Body.Close()
	var answer subscribeAnswer
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		return fmt.Errorf("register %s: status %d, answer unreadable: %w", sessionID, response.StatusCode, err)
	}
	if response.StatusCode != http.StatusOK || answer.Delivery == nil || answer.Delivery.Mode != deliveryModeConsumer {
		return fmt.Errorf("register %s: status %d, delivery %+v; want 200 in consumer mode", sessionID, response.StatusCode, answer.Delivery)
	}
	return nil
}

// awaitPulled waits up to 30 s until every session's consumer has a pull request waiting: the
// listener's relay has restarted it and pulls it.
func awaitPulled(t *testing.T, consumers *bus.SessionConsumers, sessionIDs []string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for _, sessionID := range sessionIDs {
		for {
			info, err := consumers.Info(context.Background(), sessionID)
			if err == nil && info.NumWaiting > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the consumer of %s has no pull waiting after 30 s: %+v, %v", sessionID, info, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// A listener killed (SIGKILL) while it relays many sessions restarts every one of their consumers
// at once when it starts again, from its watched session cache, with no new registration: each
// session, none of which ever sent a resume point, receives the messages stored while the listener
// was down within 10 s of its restart, each once and in stream order. The restarts wait their turn
// (relayRestartLimit) and their calls' timeout starts only once they hold it, so none times out
// waiting, and none leaves its consumer missing.
func TestSessionConsumerBurstOfRestartsDeliversEveryBacklog(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	uri := testnats.URL(t)
	const machineID = "relay-burst"
	const perSession = 3
	listener := startListenerProcess(t, binary, uri, machineID)
	listener.waitHealthy(t)
	consumers := observeConsumers(t, uri)
	standIn := startBurstStandIn(t, uri, burstSessions*perSession*2)
	run := id.New()
	sessionIDs := make([]string, burstSessions)
	for i := range sessionIDs {
		sessionIDs[i] = fmt.Sprintf("ses_burst_%s_%03d", run, i)
	}
	registerBurst(t, listener.port, sessionIDs)
	awaitPulled(t, consumers, sessionIDs)

	if err := listener.Cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the listener: %v", err)
	}
	<-listener.Exited
	publisher := connectPublisher(t, uri)
	want := map[string][]string{}
	for i := range perSession {
		for _, sessionID := range sessionIDs {
			item := newEnvelope("agent", contracts.AgentSubject(sessionID), fmt.Sprintf("while the listener was down %d", i))
			if err := publisher.Publish(item); err != nil {
				t.Fatalf("publish to %s: %v", sessionID, err)
			}
			want[sessionID] = append(want[sessionID], item.EventID)
		}
	}

	restarted := startListenerProcess(t, binary, uri, machineID)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the restarted listener's output:\n%s", restarted.Output.String())
		}
	})
	startedAt := time.Now()
	got := map[string][]burstFrame{}
	deadline := time.After(10 * time.Second)
	var last time.Time
	for received := range burstSessions * perSession {
		select {
		case frame := <-standIn.frames:
			got[frame.sessionID] = append(got[frame.sessionID], frame)
			last = time.Now()
		case <-deadline:
			complete := 0
			for _, sessionID := range sessionIDs {
				if len(got[sessionID]) >= perSession {
					complete++
				}
			}
			t.Fatalf("10 s after the restart %d of %d frames arrived; %d of %d sessions have their whole backlog", received, burstSessions*perSession, complete, burstSessions)
		}
	}
	t.Logf("every session had its backlog %s after the listener's restart", last.Sub(startedAt).Round(time.Millisecond))
	// A frame relayed twice would arrive within this second.
	extra := time.After(time.Second)
	for waiting := true; waiting; {
		select {
		case frame := <-standIn.frames:
			got[frame.sessionID] = append(got[frame.sessionID], frame)
		case <-extra:
			waiting = false
		}
	}
	for _, sessionID := range sessionIDs {
		frames := got[sessionID]
		events := make([]string, len(frames))
		for i, frame := range frames {
			events[i] = frame.eventID
			if i > 0 && frame.seq <= frames[i-1].seq {
				t.Fatalf("the frames of %s are out of stream order: %+v", sessionID, frames)
			}
		}
		if strings.Join(events, ",") != strings.Join(want[sessionID], ",") {
			t.Fatalf("%s received %v, want each of %v once, in order", sessionID, events, want[sessionID])
		}
	}
}
