package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
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
	stored, err := client.LastStoredMessage(ctx, bus.KeptRoleSubject(role))
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
// copy lies on a subject no consumer filter carries (bus.StreamSubjects), so the durable never
// delivers it back and it is never counted received a second time.
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

// The drain's budget counts its forwards, not its reads. A role unheld for long enough gathers
// copies past the keep window, and a walk that spent one of roleKeepDrainMax on each of those
// handed the role's new holder nothing while every sender had been answered 202 kept. The walk
// is bounded by roleKeepScanMax instead, so a claim forwards every copy still inside the window.
func TestADrainSpendsItsBudgetOnForwardsRatherThanOnExpiredCopies(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const (
		role   = "kept-drain-budget"
		holder = "ses_kept_budget_claimant"
		copies = 30
	)
	// expired reports whether the copy the drain walks n-th, counting from 1, is read as past the
	// keep window: the first 21 are, and every second one after them, so every copy that must be
	// forwarded lies past roleKeepDrainMax reads of the lane.
	expired := func(n int) bool { return n <= 21 || n%2 == 0 }

	cfg := harness.config
	var forwarded []string
	cfg.forwardRole = inlineForward(func(_ string, item contracts.Envelope, _ time.Duration) error {
		forwarded = append(forwarded, item.DedupeKey)
		return nil
	})
	var fresh []string
	for i := range copies {
		item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, fmt.Sprintf("kept-budget-%02d", i))
		if !keepRoleMessage(harness.client, cfg.logger, role, item) {
			t.Fatalf("keep copy %d; the listener log says why:\n%s", i, harness.logs.String())
		}
		if !expired(i + 1) {
			fresh = append(fresh, roleForwardDedupePrefix+item.DedupeKey)
		}
	}
	walked := 0
	now := func() time.Time {
		walked++
		if expired(walked) {
			return time.Now().Add(roleKeepWindow + time.Minute)
		}
		return time.Now()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	entry := session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}
	if err := drainKeptRole(ctx, cfg, role, holder, entry, now); err != nil {
		t.Fatalf("drain role %s's kept lane: %v", role, err)
	}
	if !slices.Equal(forwarded, fresh) {
		t.Fatalf("the drain forwarded %v, want every copy still inside the keep window: %v", forwarded, fresh)
	}
}

// Two listeners draining one role must not hand one copy to two holders. Each drain claims a copy
// - advancing the role's cursor to that copy alone - before it forwards it, so a drain that lost
// the claim stops there; a cursor written once at the end of the walk let both of them forward
// everything they had read, to two different agent subjects.
func TestTwoDrainsOfOneRoleHandEachKeptCopyToExactlyOneHolder(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const (
		role    = "kept-two-drains"
		holderA = "ses_kept_drain_first"
		holderB = "ses_kept_drain_second"
		copies  = 6
	)
	var keys []string
	for i := range copies {
		item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, fmt.Sprintf("kept-two-drains-%02d", i))
		if !keepRoleMessage(harness.client, harness.config.logger, role, item) {
			t.Fatalf("keep copy %d; the listener log says why:\n%s", i, harness.logs.String())
		}
		keys = append(keys, roleForwardDedupePrefix+item.DedupeKey)
	}

	var mu sync.Mutex
	handed := map[string][]string{}
	hand := func(holder string) func(string, contracts.Envelope, time.Duration) error {
		return func(_ string, item contracts.Envelope, _ time.Duration) error {
			mu.Lock()
			defer mu.Unlock()
			handed[item.DedupeKey] = append(handed[item.DedupeKey], holder)
			return nil
		}
	}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	toFirst := hand(holderA)
	first := harness.config
	first.forwardRole = inlineForward(func(subject string, item contracts.Envelope, window time.Duration) error {
		// The first drain holds its first forward until the second has walked the whole lane,
		// which is the moment two listeners drain one role at once.
		once.Do(func() {
			close(started)
			<-release
		})
		return toFirst(subject, item, window)
	})
	// The second drain stands in for a second listener: it has guards of its own, where one
	// listener's guard for the role would hold it until the first drain released it. One guard per
	// role bounds the drains inside one listener (roleKeepGuards); across listeners the cursor's
	// compare-and-swap is the guard, which is what this test exercises.
	second := harness.config
	second.roleKeepDrains = newRoleKeepGuards()
	second.forwardRole = inlineForward(hand(holderB))

	entry := session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		done <- drainKeptRole(ctx, first, role, holderA, entry, time.Now)
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := drainKeptRole(ctx, second, role, holderB, entry, time.Now); err != nil {
		t.Fatalf("the second listener's drain of role %s's kept lane: %v", role, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the first listener's drain of role %s's kept lane: %v", role, err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, key := range keys {
		switch holders := handed[key]; len(holders) {
		case 1:
		case 0:
			t.Errorf("the kept copy under %s reached no holder: %v", key, handed)
		default:
			t.Errorf("the kept copy under %s reached %v; a kept copy reaches exactly one holder", key, holders)
		}
	}
}

// The drain is a forward path beside the role lane and the publish route, and it judges a copy by
// the rule they both apply (beforeForward): a copy whose own sender has since claimed the role is
// not forwarded back to it, and one whose payload the new holder's delivery modes do not take is
// not forwarded either. Each is passed over with the cursor already past it, so the role's next
// claim is not handed it again.
func TestTheDrainJudgesAKeptCopyByTheRulesEveryForwardPathApplies(t *testing.T) {
	for _, tc := range []struct {
		name string
		role string
		// prepare stamps the rule under test onto the copy that is kept.
		prepare func(item *contracts.Envelope, holder string)
		// entry is the new holder's session entry, which the rule judges the copy against.
		entry session.SessionEntry
		// line is the drain's line for the copy it passed over.
		line string
	}{
		{
			name:    "a copy whose own sender now holds the role",
			role:    "kept-echo",
			prepare: func(item *contracts.Envelope, holder string) { item.SourceSession = holder },
			entry:   session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true},
			line:    "listener passed over a kept role message its own sender has claimed the role for",
		},
		{
			name:    "a copy the new holder's delivery modes do not take",
			role:    "kept-unadvertised",
			prepare: func(item *contracts.Envelope, _ string) { item.Payload = `{"delivery":{"mode":"steer"}}` },
			entry:   session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true, Capabilities: []string{"btw"}},
			line:    "listener kept role message's new holder does not advertise delivery mode",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			harness := newListenerDeliveryHarness(t, nil)
			holder := "ses_" + strings.ReplaceAll(tc.role, "-", "_") + "_claimant"
			cfg := harness.config
			var forwards atomic.Int32
			cfg.forwardRole = inlineForward(func(string, contracts.Envelope, time.Duration) error {
				forwards.Add(1)
				return nil
			})
			item := listenerTestEnvelope(contracts.RoleTopicPrefix+tc.role, tc.role+"-1")
			tc.prepare(&item, holder)
			if !keepRoleMessage(harness.client, cfg.logger, tc.role, item) {
				t.Fatalf("keep the role message; the listener log says why:\n%s", harness.logs.String())
			}
			drain := func(what string) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := drainKeptRole(ctx, cfg, tc.role, holder, tc.entry, time.Now); err != nil {
					t.Fatalf("%s drain of role %s's kept lane: %v", what, tc.role, err)
				}
			}

			drain("the claim's")
			if forwards.Load() != 0 {
				t.Fatalf("the drain forwarded the copy %d times, want none:\n%s", forwards.Load(), harness.logs.String())
			}
			if passed := strings.Count(harness.logs.String(), tc.line); passed != 1 {
				t.Fatalf("the drain logged %q %d times, want once:\n%s", tc.line, passed, harness.logs.String())
			}

			// The cursor is past it, so a later claim's drain is not handed it again.
			drain("a later claim's")
			if forwards.Load() != 0 {
				t.Fatalf("a later drain forwarded the copy %d times: the cursor did not move past it", forwards.Load())
			}
			if passed := strings.Count(harness.logs.String(), tc.line); passed != 1 {
				t.Fatalf("the drain logged %q %d times after a second drain, want once:\n%s", tc.line, passed, harness.logs.String())
			}
		})
	}
}

// A kept copy carries its sender: the publish route stamps the sender onto the envelope before it
// is kept, so the holder handed the copy later reads the same stamp a live delivery carries.
func TestAKeptRoleMessageCarriesTheSenderThePublishStamped(t *testing.T) {
	d, client := keptRoleDeps(t)
	uri := testnats.URL(t)
	const (
		role   = "kept-sender-stamp"
		holder = "ses_kept_sender_claimant"
		sender = "ses_kept_sender"
		dir    = "/work/kept-sender"
	)
	if _, err := d.registry.Upsert(store.Interest{SessionID: sender, MachineID: "test-machine", Dir: dir}, []string{"notifications.test.kept"}); err != nil {
		t.Fatalf("register the sender's interest record: %v", err)
	}

	recorder, answer := publishToRole(t, d, role,
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","source_session":"`+sender+`","message":"nobody holds this yet"}`)
	if recorder.Code != http.StatusAccepted || !answer.Kept {
		t.Fatalf("publish to an unheld role: status %d, body %s; want 202 kept", recorder.Code, recorder.Body.String())
	}
	kept := waitForKeptRoleMessage(t, client, role, answer.EventID)
	if kept.Sender == nil || kept.Sender.SessionID != sender || kept.Sender.Machine != "test-machine" || kept.Sender.Cwd != dir {
		t.Fatalf("the kept copy's sender is %+v, want the stamp the publish made for %s", kept.Sender, sender)
	}

	frames := startStandIn(t, uri, holder, always)
	if err := d.sessions.Put(holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register the claimant: %v", err)
	}
	claimRecorder := httptest.NewRecorder()
	roleSetHandler(d, "test-machine").ServeHTTP(claimRecorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set",
		strings.NewReader(`{"session_id":"`+holder+`","role":"`+role+`"}`)))
	if claimRecorder.Code != http.StatusOK {
		t.Fatalf("claim of %s: status %d, body %s", role, claimRecorder.Code, claimRecorder.Body.String())
	}
	forwarded := expectFrames(t, frames, 1, 500*time.Millisecond)[0]
	if forwarded.Sender == nil || forwarded.Sender.SessionID != sender || forwarded.Sender.Cwd != dir {
		t.Fatalf("the forwarded copy's sender is %+v, want the stamp the publish made for %s", forwarded.Sender, sender)
	}
}

// A role token POST /v1/roles/set refuses (rolePattern) is a role no session can ever claim, so a
// copy kept under it would expire unread. Keeping is off for it, and its sender gets the 404 it
// can act on, with nothing stored.
func TestAPublishToARoleNoClaimCanNameIsRefusedAndKeepsNothing(t *testing.T) {
	d, client := keptRoleDeps(t)
	const role = "legion-infra-INFRA-839-architect"

	recorder, _ := publishThrough(d,
		`{"topic":"`+contracts.RoleTopicPrefix+role+`","source":"agent","message":"a token no claim can name"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("publish to a role no claim can name: status %d, body %s; want 404", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != "{\"error\":\"no holder for role "+role+"\",\"reason\":\"unclaimed\"}\n" {
		t.Fatalf("body = %q", body)
	}
	assertNothingKept(t, client, role)

	// The claim route refuses the same token, which is why nothing may be kept under it.
	claimRecorder := httptest.NewRecorder()
	roleSetHandler(d, "test-machine").ServeHTTP(claimRecorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set",
		strings.NewReader(`{"session_id":"ses_kept_invalid_role","role":"`+role+`"}`)))
	if claimRecorder.Code != http.StatusBadRequest {
		t.Fatalf("claim of %s: status %d, body %s; want the 400 that refuses the token", role, claimRecorder.Code, claimRecorder.Body.String())
	}
}

// A keep that lands after a concurrent claim's drain has read the lane still reaches that holder:
// the keep resolves the role again and drains to whoever holds it now (drainAfterKeep). The claim
// and its drain are made here before the keep, which is the order that race leaves behind, and no
// second claim follows.
func TestAKeptCopyReachesTheHolderThatClaimedTheRoleUnderTheKeep(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const (
		role   = "kept-claimed-under-the-keep"
		holder = "ses_kept_under_claim"
	)
	cfg := harness.config
	forwards := make(chan contracts.Envelope, 4)
	cfg.forwardRole = inlineForward(func(_ string, item contracts.Envelope, _ time.Duration) error {
		forwards <- item
		return nil
	})
	claimRoleFor(t, harness.registry, harness.sessions, holder, role, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	entry := session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}
	if err := drainKeptRole(ctx, cfg, role, holder, entry, time.Now); err != nil {
		t.Fatalf("the claim's own drain of role %s's still empty kept lane: %v", role, err)
	}

	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "kept-under-claim-1")
	if !keepRoleLaneMessage(cfg, role, "", item, roleHolderUnclaimed) {
		t.Fatalf("keep the role message; the listener log says why:\n%s", harness.logs.String())
	}
	select {
	case forwarded := <-forwards:
		if forwarded.EventID != item.EventID {
			t.Fatalf("the holder was handed event %s, want the kept %s", forwarded.EventID, item.EventID)
		}
		if forwarded.KeptAt == nil || *forwarded.KeptAt <= 0 {
			t.Fatalf("the forwarded copy carries kept_at %v, want the time the stream stored it", forwarded.KeptAt)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the copy kept for %s, which held the role already, reached it in no 20 seconds and no second claim comes:\n%s",
			holder, harness.logs.String())
	}
}

// A claim the restart grace retained is not a role without a holder: that holder re-registers
// inside the session TTL and keeps its claim, so it writes no POST /v1/roles/set and no drain
// follows, and a copy kept for it would wait for a claim that never comes. The lane keeps nothing
// and raises the delivery_failed it raised before the kept lane.
func TestTheRoleLaneKeepsNothingForAClaimTheRestartGraceRetained(t *testing.T) {
	const (
		role   = "kept-retained-claim"
		holder = "ses_kept_retained_holder"
	)
	client := setupPublishTestClient(t)
	// The claim is written before the listener's own registry opens, which is what a restart
	// leaves behind: that registry restores the claim and gives it one session TTL of grace
	// (store.ExpiredRoleClaimRetained) even while its holder's session registration is gone.
	writer, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open the registry that writes the claim: %v", err)
	}
	if _, err := writer.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("claim %s for a session the store does not hold: %v", role, err)
	}
	harness := newListenerDeliveryHarnessOn(t, client, nil)

	topic := contracts.RoleTopicPrefix + role
	probe, err := harness.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + topic)
	if err != nil {
		t.Fatalf("subscribe to the role topic's exception lane: %v", err)
	}
	t.Cleanup(func() { _ = probe.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the probe: %v", err)
	}

	item := listenerTestEnvelope(topic, "lane-retained-claim")
	harness.coreHandler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})

	assertDeliveryException(t, probe, item, "delivery_failed")
	assertNothingKept(t, client, role)
	if held, err := harness.registry.RoleHolder(role); err != nil || held != holder {
		t.Fatalf("the retained claim after the delivery = %q, %v; want %s still holding it", held, err, holder)
	}
}

// The kept lane is the listener's own: its copies and the cursors of its drains. A publish to it
// through the route would let any bearer holder rewind a role's drain or put a copy of its own on
// the lane, so the route refuses that prefix as it refuses agent topics, and stores nothing.
func TestAPublishToTheListenersKeptLaneIsRefused(t *testing.T) {
	d, client := keptRoleDeps(t)
	const role = "kept-lane-guard"
	for _, subject := range []string{bus.KeptRoleSubject(role), bus.KeptRoleCursorSubject(role)} {
		t.Run(subject, func(t *testing.T) {
			recorder, _ := publishThrough(d,
				`{"topic":"`+subject+`","source":"agent","message":"a copy of my own"}`)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("publish to %s: status %d, body %s; want 400", subject, recorder.Code, recorder.Body.String())
			}
			if body := recorder.Body.String(); !strings.Contains(body, bus.KeptTopicPrefix) {
				t.Fatalf("the refusal of %s is %q, want it to name %s", subject, body, bus.KeptTopicPrefix)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := client.LastStoredMessage(ctx, subject); !errors.Is(err, natsgo.ErrMsgNotFound) {
				t.Fatalf("the stream holds a message on %s (%v); the refused publish stored nothing", subject, err)
			}
		})
	}
}
