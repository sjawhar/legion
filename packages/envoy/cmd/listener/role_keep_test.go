package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// keptPublishAnswer is the publish route's 202 for a role message it kept: the envelope as it was
// stored, with the kept fields the answer carries only then.
type keptPublishAnswer struct {
	contracts.Envelope
	Holder     string `json:"holder"`
	Kept       bool   `json:"kept"`
	KeptReason string `json:"kept_reason"`
}

// readKeptRoleMessage reads the copy the stream holds on role's kept subject, and reports false
// when it holds none.
func readKeptRoleMessage(t *testing.T, client *bus.Client, role string) (contracts.Envelope, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stored, err := client.LastStoredMessage(ctx, contracts.KeptRoleSubject(role))
	if errors.Is(err, natsgo.ErrMsgNotFound) {
		return contracts.Envelope{}, false
	}
	if err != nil {
		t.Fatalf("read role %s's kept lane: %v", role, err)
	}
	var item contracts.Envelope
	if err := json.Unmarshal(stored.Data, &item); err != nil {
		t.Fatalf("decode the kept copy of role %s: %v", role, err)
	}
	return item, true
}

// waitForKeptRoleMessage waits until the stream holds the copy of eventID on role's kept subject,
// which the role lane writes after its own handler has answered, and returns it.
func waitForKeptRoleMessage(t *testing.T, client *bus.Client, role, eventID string) contracts.Envelope {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		item, ok := readKeptRoleMessage(t, client, role)
		if ok && item.EventID == eventID {
			return item
		}
		if time.Now().After(deadline) {
			t.Fatalf("role %s's kept lane does not hold event %s after 5s (last read: %+v, held: %t)", role, eventID, item, ok)
		}
	}
}

// assertNothingKept fails when the stream holds any copy on role's kept subject.
func assertNothingKept(t *testing.T, client *bus.Client, role string) {
	t.Helper()
	if item, ok := readKeptRoleMessage(t, client, role); ok {
		t.Fatalf("role %s's kept lane holds event %s, want nothing kept", role, item.EventID)
	}
}

// keptRoleDeps is the publish route and the role-claim route over one listener's stores, as main
// wires them.
func keptRoleDeps(t *testing.T) (*listenerDeps, *bus.Client) {
	t.Helper()
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	return withPublishRoute(t, &listenerDeps{client: client, registry: registry, sessions: sessions}), client
}

// publishToRole sends message to role's topic through the publish route and returns its answer.
func publishToRole(t *testing.T, d *listenerDeps, role, body string) (*httptest.ResponseRecorder, keptPublishAnswer) {
	t.Helper()
	recorder, _ := publishThrough(d, body)
	var answer keptPublishAnswer
	if recorder.Code == http.StatusOK || recorder.Code == http.StatusAccepted {
		if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode the publish answer for role %s: %v", role, err)
		}
	}
	return recorder, answer
}

// A role nobody holds is no longer a refusal: the listener keeps the message on that role's kept
// lane for whoever claims the role next, and answers its sender 202 saying so. The stored copy
// keeps its own role topic, so the next holder is handed the message as its sender sent it.
func TestAPublishToAnUnheldRoleIsKeptAndAnsweredWithItsReason(t *testing.T) {
	d, client := keptRoleDeps(t)
	const role = "kept-unclaimed"

	recorder, answer := publishToRole(t, d, role,
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"nobody holds this yet"}`)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("publish to an unheld role: status %d, body %s; want 202", recorder.Code, recorder.Body.String())
	}
	if !answer.Kept || answer.KeptReason != "unclaimed" || answer.Holder != "" {
		t.Fatalf("the answer is %+v, want kept with reason unclaimed and no holder", answer)
	}

	kept := waitForKeptRoleMessage(t, client, role, answer.EventID)
	if kept.Topic != contracts.RoleTopicPrefix+role {
		t.Fatalf("the kept copy's topic is %q, want the role topic it was published on", kept.Topic)
	}
	if kept.DedupeKey != answer.DedupeKey || kept.PayloadSummary != "nobody holds this yet" {
		t.Fatalf("the kept copy is %+v, want the envelope the answer named", kept)
	}
	if kept.KeptAt != nil {
		t.Fatalf("the stored copy carries kept_at %d: the time is stamped on the forward, from the stream's own store time", *kept.KeptAt)
	}
}

// A claim whose holder is no longer registered has lapsed. The message is kept for the role's next
// holder under that reason, and the lapsed claim is released exactly as it was before.
func TestAPublishToALapsedClaimIsKeptAndTheClaimIsStillReleased(t *testing.T) {
	d, client := keptRoleDeps(t)
	const (
		role   = "kept-lapsed"
		holder = "ses_kept_lapsed_holder"
	)
	if _, err := d.registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("claim %s for a session the store does not hold: %v", role, err)
	}

	recorder, answer := publishToRole(t, d, role,
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"is the holder still there?"}`)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("publish to a lapsed claim: status %d, body %s; want 202", recorder.Code, recorder.Body.String())
	}
	if !answer.Kept || answer.KeptReason != "holder_lapsed" || answer.Holder != holder {
		t.Fatalf("the answer is %+v, want kept with reason holder_lapsed naming %s", answer, holder)
	}
	waitForKeptRoleMessage(t, client, role, answer.EventID)

	if held, err := d.registry.RoleHolder(role); err != nil || held != "" {
		t.Fatalf("the lapsed claim after the publish = %q, %v; want released", held, err)
	}
}

// A sender that holds a queue of its own asks for keep: false and gets today's answer: the 404
// naming the state, with nothing stored for a later holder.
func TestAPublishWithKeepFalseToAnUnheldRoleIsRefusedAndKeepsNothing(t *testing.T) {
	d, client := keptRoleDeps(t)
	const role = "kept-opted-out"

	recorder, _ := publishThrough(d,
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"the sender queues this itself","keep":false}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("publish with keep: false to an unheld role: status %d, body %s; want 404", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != "{\"error\":\"no holder for role "+role+"\",\"reason\":\"unclaimed\"}\n" {
		t.Fatalf("body = %q", body)
	}
	assertNothingKept(t, client, role)
}

// The claim is where a kept message is handed over: the session that takes the role is forwarded
// the copy once, under the role forward's dedupe key and carrying the time it was kept, and the
// cursor the drain leaves means a second claim of the same role forwards nothing again.
func TestAClaimForwardsEachKeptRoleMessageToItsNewHolderExactlyOnce(t *testing.T) {
	d, client := keptRoleDeps(t)
	uri := testnats.URL(t)
	const (
		role   = "kept-drained"
		holder = "ses_kept_claimant"
	)

	recorder, answer := publishToRole(t, d, role,
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"wait for the next holder"}`)
	if recorder.Code != http.StatusAccepted || !answer.Kept {
		t.Fatalf("publish to an unheld role: status %d, body %s; want 202 kept", recorder.Code, recorder.Body.String())
	}
	waitForKeptRoleMessage(t, client, role, answer.EventID)

	frames := startStandIn(t, uri, holder, always)
	if err := d.sessions.Put(holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register the claimant: %v", err)
	}
	claim := func(what string) {
		t.Helper()
		claimRecorder := httptest.NewRecorder()
		roleSetHandler(d, "test-machine").ServeHTTP(claimRecorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set",
			strings.NewReader(`{"session_id":"`+holder+`","role":"`+role+`"}`)))
		if claimRecorder.Code != http.StatusOK {
			t.Fatalf("%s claim of %s: status %d, body %s", what, role, claimRecorder.Code, claimRecorder.Body.String())
		}
	}

	claim("first")
	forwarded := expectFrames(t, frames, 1, 500*time.Millisecond)[0]
	if forwarded.EventID != answer.EventID {
		t.Fatalf("the claimant received event %s, want the kept %s", forwarded.EventID, answer.EventID)
	}
	if forwarded.DedupeKey != roleForwardDedupePrefix+answer.DedupeKey {
		t.Fatalf("the forwarded copy's dedupe key is %q, want %s", forwarded.DedupeKey, roleForwardDedupePrefix+answer.DedupeKey)
	}
	if forwarded.Topic != contracts.RoleTopicPrefix+role {
		t.Fatalf("the forwarded copy's topic is %q, want the role topic", forwarded.Topic)
	}
	if forwarded.KeptAt == nil || *forwarded.KeptAt <= 0 {
		t.Fatalf("the forwarded copy carries kept_at %v, want the time the stream stored it", forwarded.KeptAt)
	}

	claim("second")
	select {
	case again := <-frames:
		t.Fatalf("a second claim of %s forwarded event %s again", role, again.EventID)
	case <-time.After(time.Second):
	}
}

// A kept copy the stream has held longer than roleKeepWindow is not handed to a later holder: a
// role token names a tree role rather than one run, so a holder generations later is not the
// addressee its sender had. It is dropped, counted and logged once, and the cursor moves past it,
// so the role's next claim does not see it again.
//
// The age is measured against the stream's own store time, which no caller may choose, so the test
// moves the drain's clock rather than holding a copy for an hour.
func TestAKeptRoleMessageOlderThanTheKeepWindowIsDroppedAndTheCursorMovesPastIt(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const (
		role   = "kept-expired"
		holder = "ses_kept_expired_claimant"
	)
	var forwards atomic.Int32
	cfg := harness.config
	cfg.forwardRole = inlineForward(func(string, contracts.Envelope, time.Duration) error {
		forwards.Add(1)
		return nil
	})
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "kept-expired-1")
	if !keepRoleMessage(harness.client, cfg.logger, role, item) {
		t.Fatalf("keep the role message; the listener log says why:\n%s", harness.logs.String())
	}
	entry := session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}
	drain := func(now func() time.Time) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := drainKeptRole(ctx, cfg, role, holder, entry, now); err != nil {
			t.Fatalf("drain role %s's kept lane: %v", role, err)
		}
	}

	drain(func() time.Time { return time.Now().Add(roleKeepWindow + time.Minute) })
	if forwards.Load() != 0 {
		t.Fatalf("a kept copy older than %s was forwarded %d times, want none", roleKeepWindow, forwards.Load())
	}
	const dropLine = "listener dropped a kept role message older than the keep window"
	if dropped := strings.Count(harness.logs.String(), dropLine); dropped != 1 {
		t.Fatalf("the drop is logged %d times, want once:\n%s", dropped, harness.logs.String())
	}
	scrape := httptest.NewRecorder()
	harness.metrics.Handler().ServeHTTP(scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if want := "test_role_kept_expired 1"; !strings.Contains(scrape.Body.String(), want) {
		t.Fatalf("metrics do not hold %s:\n%s", want, scrape.Body.String())
	}

	// The copy is still on the lane and is now well inside the window; a cursor that had not
	// moved past it would forward it here.
	drain(time.Now)
	if forwards.Load() != 0 {
		t.Fatalf("a later claim forwarded the dropped copy %d times: the cursor did not move past it", forwards.Load())
	}
	if dropped := strings.Count(harness.logs.String(), dropLine); dropped != 1 {
		t.Fatalf("the drop is logged %d times after a second drain, want once:\n%s", dropped, harness.logs.String())
	}
}

// Both of the role lane's give-up branches - a role nobody claims and a claim whose holder is
// gone - keep the message instead of reporting it lost: no delivery exception goes out. The kept
// copy lies on a subject no consumer filter carries (bus.DurableNotificationSubjects), so the
// durable never delivers it back and it is never counted received a second time.
func TestTheRoleLanesGiveUpBranchesKeepTheMessageWithNoException(t *testing.T) {
	cases := []struct {
		name string
		role string
		// holder is the session the role's claim names, registered nowhere; empty leaves the
		// role unclaimed.
		holder string
	}{
		{name: "unclaimed role", role: "lane-kept-unclaimed"},
		{name: "lapsed holder", role: "lane-kept-lapsed", holder: "ses_lane_kept_lapsed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newListenerDeliveryHarness(t, nil)
			topic := contracts.RoleTopicPrefix + tc.role
			if tc.holder != "" {
				if _, err := harness.registry.SetRole(tc.holder, "test-machine", tc.role, false); err != nil {
					t.Fatalf("claim %s for a session the store does not hold: %v", tc.role, err)
				}
			}
			probe, err := harness.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + ">")
			if err != nil {
				t.Fatalf("subscribe to the exception lanes: %v", err)
			}
			t.Cleanup(func() { _ = probe.Unsubscribe() })
			delivered := make(chan contracts.Envelope, 8)
			durable, err := startListenerSubscription(harness.client, "listener-kept-"+id.New(), func(message *natsgo.Msg) {
				var item contracts.Envelope
				if err := json.Unmarshal(message.Data, &item); err == nil {
					delivered <- item
				}
				_ = message.Ack()
			})
			if err != nil {
				t.Fatalf("start the durable subscription: %v", err)
			}
			t.Cleanup(func() { _ = durable.Unsubscribe() })
			if err := harness.client.Conn.Flush(); err != nil {
				t.Fatalf("flush the probes: %v", err)
			}

			item := listenerTestEnvelope(topic, "lane-kept-"+tc.role)
			harness.coreHandler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})

			kept := waitForKeptRoleMessage(t, harness.client, tc.role, item.EventID)
			if kept.Topic != topic {
				t.Fatalf("the kept copy's topic is %q, want the role topic %s", kept.Topic, topic)
			}
			noException(t, probe)
			select {
			case back := <-delivered:
				t.Fatalf("the durable delivered event %s on %s: a kept copy is on no consumer's filter", back.EventID, back.Topic)
			case <-time.After(time.Second):
			}
			scrape := httptest.NewRecorder()
			harness.metrics.Handler().ServeHTTP(scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if want := `test_messages_received{source="agent",topic_prefix="notifications.role"} 1`; !strings.Contains(scrape.Body.String(), want) {
				t.Fatalf("metrics do not hold %s, so the kept copy was counted received:\n%s", want, scrape.Body.String())
			}
		})
	}
}

// Keeping is only for a role the listener cannot deliver to. A live holder's send is delivered and
// keeps nothing, and a holder that takes the forward and answers no receipt is still the 504
// receipt_timeout and keeps nothing either: its forward is already stored on its agent subject.
func TestADeliveredSendAndAReceiptTimeoutBothKeepNothing(t *testing.T) {
	d, client := keptRoleDeps(t)
	uri := testnats.URL(t)

	t.Run("a live holder is delivered", func(t *testing.T) {
		const (
			role   = "kept-live-holder"
			holder = "ses_kept_live_holder"
		)
		claimRoleFor(t, d.registry, d.sessions, holder, role, nil)
		startStandIn(t, uri, holder, always)

		recorder, answer := publishToRole(t, d, role,
			`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"the holder is here"}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("publish to a live holder: status %d, body %s; want 200", recorder.Code, recorder.Body.String())
		}
		if answer.Kept || answer.KeptReason != "" || answer.Holder != holder {
			t.Fatalf("the answer is %+v, want a delivered send to %s with no kept fields", answer, holder)
		}
		assertNothingKept(t, client, role)
	})

	t.Run("a deaf holder is a receipt timeout", func(t *testing.T) {
		const (
			role   = "kept-deaf-holder"
			holder = "ses_kept_deaf_holder"
		)
		claimRoleFor(t, d.registry, d.sessions, holder, role, nil)
		startStandIn(t, uri, holder, never)

		recorder, _ := publishThrough(d,
			`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"the holder never answers"}`)
		if recorder.Code != http.StatusGatewayTimeout {
			t.Fatalf("publish to a deaf holder: status %d, body %s; want 504", recorder.Code, recorder.Body.String())
		}
		var failure roleSendFailure
		if err := json.Unmarshal(recorder.Body.Bytes(), &failure); err != nil {
			t.Fatalf("decode the 504: %v", err)
		}
		if failure.Reason != sendReceiptTimeout || failure.Holder != holder {
			t.Fatalf("the 504 is %+v, want receipt_timeout naming %s", failure, holder)
		}
		assertNothingKept(t, client, role)
	})
}
