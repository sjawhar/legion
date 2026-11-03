package main

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
)

// A relay that holds a message another path is pushing (holdWhilePushedElsewhere) delays that
// session alone: every other session's relay runs on a goroutine of its own, so its messages flow
// while the hold stands. The held message is pushed once, by the relay itself, as soon as the
// other push ends unlanded and its attempt record is gone.
//
// An attempt record with nothing pushing behind it is what the relay must survive: it is the
// record a worker leaves when its push is handed back or its process ends, and the message is the
// relay's to deliver from then on.
func TestARelayHoldDelaysOnlyThatSession(t *testing.T) {
	const (
		heldPort = 4096
		freePort = 4097
	)
	rig := newRelayRig(t)
	held := "ses_held_" + id.New()
	free := "ses_free_" + id.New()
	heldTopic := "notifications.github.acme.held-" + id.New() + ".mention"
	freeTopic := "notifications.github.acme.free-" + id.New() + ".mention"

	heldPushes := make(chan string, 8)
	freePushes := make(chan string, 8)
	rig.harness.config.deliverer.HTTPClient = &http.Client{Transport: listenerTransport(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(request.URL.Host, strconv.Itoa(heldPort)) {
			heldPushes <- string(body)
		} else {
			freePushes <- string(body)
		}
		return listenerDeliveryResponse(http.StatusNoContent), nil
	})}

	rig.subscribe(t, `{"session_id":"`+held+`","port":`+strconv.Itoa(heldPort)+`,"host":"127.0.0.1","topics":["`+heldTopic+`"]}`)
	rig.subscribe(t, `{"session_id":"`+free+`","port":`+strconv.Itoa(freePort)+`,"host":"127.0.0.1","topics":["`+freeTopic+`"]}`)
	for _, sessionID := range []string{held, free} {
		if entry := rig.entry(t, sessionID); entry.Delivery != session.DeliveryConsumer {
			t.Fatalf("the entry of %s records delivery %q, want the consumer's", sessionID, entry.Delivery)
		}
	}

	// A ledger attempt with no push behind it: another path says it is pushing this message, so
	// the relay of the held session holds it rather than pushing or acking it.
	heldItem := listenerTestEnvelope(heldTopic, "hold-"+id.New())
	heldAttempt := holdLedgerAttempt(t, rig.harness.config, heldItem.DedupeKey, held)
	if _, err := rig.harness.client.JS().Publish(heldTopic, marshalListenerEnvelope(t, heldItem)); err != nil {
		t.Fatalf("store the held message: %v", err)
	}
	awaitLogLine(t, rig, "session relay holds a message another path is pushing", 30*time.Second)

	// While that hold stands, the other session's message must flow.
	freeItem := listenerTestEnvelope(freeTopic, "free-"+id.New())
	if _, err := rig.harness.client.JS().Publish(freeTopic, marshalListenerEnvelope(t, freeItem)); err != nil {
		t.Fatalf("store the other session's message: %v", err)
	}
	select {
	case body := <-freePushes:
		if !strings.Contains(body, freeItem.EventID) {
			t.Fatalf("the other session's port was pushed:\n%s\nwant %s", body, freeItem.EventID)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the other session's message %s never reached its port while %s held one", freeItem.EventID, held)
	}
	select {
	case body := <-heldPushes:
		t.Fatalf("the held session's port was pushed while the hold stood:\n%s", body)
	default:
	}

	// The other push ends unlanded: its ledger attempt is given up, and the relay pushes the
	// message it held, exactly once.
	releaseLedgerAttempt(t, rig.harness.config, heldAttempt)
	select {
	case body := <-heldPushes:
		if !strings.Contains(body, heldItem.EventID) {
			t.Fatalf("the held session's port was pushed:\n%s\nwant %s", body, heldItem.EventID)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the held message %s never reached the port after the other push ended", heldItem.EventID)
	}
	select {
	case body := <-heldPushes:
		t.Fatalf("the held session's port was pushed the message a second time:\n%s", body)
	case <-time.After(2 * time.Second):
	}
}

// failingLedger fails its first failures Attempt calls, as a ledger whose bucket does not answer
// does, and proceeds after them.
type failingLedger struct {
	dedupe.Ledger
	failures int
	calls    int
}

func (l *failingLedger) Attempt(string, string) (dedupe.Attempt, error) {
	l.calls++
	if l.calls <= l.failures {
		return dedupe.Attempt{}, errLedgerDown
	}
	return dedupe.Attempt{Verdict: dedupe.Proceed}, nil
}

// A relay holding a message the ledger cannot answer for asks it again every relayHeldRecheck,
// for as long as the outage lasts. That outage is one episode: it is warned about once, however
// many asks fail, rather than once a second for every held session, and one INFO line says when
// the ledger answered again.
func TestARelayHoldWarnsOncePerLedgerEpisodeAndOnceTheLedgerIsBack(t *testing.T) {
	const sessionID = "ses_hold_episode"
	logs := &cmdtest.LockedBuffer{}
	ledger := &failingLedger{failures: 3}
	relays := &sessionRelays{cfg: listenerDeliveryHandlerConfig{logger: logging.NewWithWriter("test", logs), ledger: ledger}}
	acker := &countingAcker{}
	// A message on the session's own agent subject is judged by nothing but the ledger
	// (followedTopicVerdict), so the hold here is the ledger's alone.
	item := listenerTestEnvelope(contracts.AgentSubject(sessionID), "hold-episode-"+id.New())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	verdict, _ := relays.holdWhilePushedElsewhere(ctx, sessionID, item, bus.SessionMessage{SessionAcker: acker})

	if verdict != relayPushesIt {
		t.Fatalf("the hold ended on verdict %d, want the relay pushing the message once the ledger answered", verdict)
	}
	if ledger.calls != ledger.failures+1 {
		t.Fatalf("the hold asked the ledger %d times, want %d: one a recheck until it answered", ledger.calls, ledger.failures+1)
	}
	if acker.progressed != ledger.failures {
		t.Fatalf("the hold kept the message in progress %d times, want one a recheck (%d)", acker.progressed, ledger.failures)
	}
	if got := strings.Count(logs.String(), `"msg":"`+ledgerUnavailableLine+`"`); got != 1 {
		t.Fatalf("the hold logged %d %q WARNs over %d failed asks, want one for the episode:\n%s", got, ledgerUnavailableLine, ledger.failures, logs.String())
	}
	if got := strings.Count(logs.String(), `"msg":"delivery ledger answered again"`); got != 1 {
		t.Fatalf("the ledger coming back logged %d lines, want one:\n%s", got, logs.String())
	}
}
