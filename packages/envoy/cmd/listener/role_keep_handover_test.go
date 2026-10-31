package main

import (
	"context"
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
	"github.com/sjawhar/envoy/internal/session"
)

// keptHandover is one forward a drain of a kept lane made: the session it reached and the dedupe
// key it carried.
type keptHandover struct {
	holder string
	key    string
}

// keptLaneUnderClaim is one listener's kept lane under test: the harness's stores, a delivery
// config whose role forwards the test stands in for, and the /v1/roles/set route over them. A
// drain is started the way production starts one - by a claim through that route, by a keep
// (keepForNextHolder) or by the sweep (sweepKeptRoleLanes) - rather than by calling the drain
// itself, since what these tests are about is what happens after one walk of the lane ends.
type keptLaneUnderClaim struct {
	harness listenerDeliveryHarness
	deps    *listenerDeps

	mu     sync.Mutex
	handed []keptHandover
	// forwards carries each forward as it is made, so a test waits for the copies it expects
	// rather than for a drain whose rounds it cannot see.
	forwards chan keptHandover
}

// newKeptLaneUnderClaim builds the lane over harness. forward stands in for the holder: it is
// called with the number of the forward being attempted, counting from 1, the session it is
// addressed to and the envelope, and a forward it fails is one the drain could not make.
func newKeptLaneUnderClaim(t *testing.T, harness listenerDeliveryHarness, forward func(attempt int, holder string, item contracts.Envelope) error) *keptLaneUnderClaim {
	t.Helper()
	lane := &keptLaneUnderClaim{harness: harness, forwards: make(chan keptHandover, 64)}
	var attempts atomic.Int32
	cfg := harness.config
	cfg.forwardRole = inlineForward(func(subject string, item contracts.Envelope, _ time.Duration) error {
		holder := strings.TrimPrefix(subject, contracts.AgentTopicPrefix)
		if err := forward(int(attempts.Add(1)), holder, item); err != nil {
			return err
		}
		handed := keptHandover{holder: holder, key: item.DedupeKey}
		lane.mu.Lock()
		lane.handed = append(lane.handed, handed)
		lane.mu.Unlock()
		lane.forwards <- handed
		return nil
	})
	lane.deps = &listenerDeps{
		client:    harness.client,
		registry:  harness.registry,
		sessions:  harness.sessions,
		delivery:  &cfg,
		confirmed: newConfirmedRecord(t),
	}
	return lane
}

// keep stores count copies on role's kept lane, as the listener's own give-up paths store one
// (keepRoleMessage), and returns the dedupe keys a drain forwards them under, in the order they
// were kept.
func (l *keptLaneUnderClaim) keep(t *testing.T, role, prefix string, count int) []string {
	t.Helper()
	keys := make([]string, 0, count)
	for index := range count {
		item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, fmt.Sprintf("%s-%02d", prefix, index))
		if !keepRoleMessage(l.harness.client, l.harness.config.logger, role, item) {
			t.Fatalf("keep copy %d; the listener log says why:\n%s", index, l.harness.logs.String())
		}
		keys = append(keys, roleForwardDedupePrefix+item.DedupeKey)
	}
	return keys
}

// register registers sessionID as the live self-subscribed session a drain forwards to.
func (l *keptLaneUnderClaim) register(t *testing.T, sessionID string) {
	t.Helper()
	if err := l.harness.sessions.Put(sessionID, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
}

// claim takes role for sessionID through POST /v1/roles/set, the route a session claims a role
// through and the one that hands the claimant the role's kept lane.
func (l *keptLaneUnderClaim) claim(t *testing.T, sessionID, role string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	roleSetHandler(l.deps, "test-machine").ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/roles/set",
		strings.NewReader(`{"session_id":"`+sessionID+`","role":"`+role+`"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("claim of %s for %s: status %d, body %s", role, sessionID, recorder.Code, recorder.Body.String())
	}
}

// await collects the next count forwards, within window, and fails with the listener's log when
// fewer arrive.
func (l *keptLaneUnderClaim) await(t *testing.T, count int, window time.Duration) []keptHandover {
	t.Helper()
	collected := make([]keptHandover, 0, count)
	deadline := time.After(window)
	for len(collected) < count {
		select {
		case handed := <-l.forwards:
			collected = append(collected, handed)
		case <-deadline:
			t.Fatalf("the drain forwarded %d of %d copies within %s: %+v\n%s", len(collected), count, window, collected, l.harness.logs.String())
		}
	}
	return collected
}

// keys is the dedupe key of each forward made so far, in order.
func (l *keptLaneUnderClaim) keys() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	keys := make([]string, 0, len(l.handed))
	for _, handed := range l.handed {
		keys = append(keys, handed.key)
	}
	return keys
}

// A role's kept lane is handed over whole, from the one claim that started it. One walk of the
// lane forwards at most roleKeepDrainMax copies, and nothing starts another drain while the
// holder stays live: a keep happens only while a role has no live holder, and a live holder
// claims no second time. A lane of more copies than one walk forwards therefore used to strand
// every copy past the twentieth until it aged out unread, with every sender answered 202 kept.
func TestOneClaimHandsOverAKeptLaneLongerThanOneWalkOfIt(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const (
		role   = "kept-handover-budget"
		holder = "ses_kept_handover_budget"
		copies = roleKeepDrainMax + 5
	)
	lane := newKeptLaneUnderClaim(t, harness, func(int, string, contracts.Envelope) error { return nil })
	keys := lane.keep(t, role, "kept-handover-budget", copies)
	lane.register(t, holder)

	lane.claim(t, holder, role)
	handed := lane.await(t, copies, 30*time.Second)

	for _, forwarded := range handed {
		if forwarded.holder != holder {
			t.Fatalf("a copy under %s reached %s, want the role's one holder %s", forwarded.key, forwarded.holder, holder)
		}
	}
	if forwarded := lane.keys(); !slices.Equal(forwarded, keys) {
		t.Fatalf("the claim handed over %v, want every kept copy once and in the order it was kept: %v", forwarded, keys)
	}
}

// A copy whose forward failed is left on the lane for the role's next drain, and while the role's
// holder stays live nothing starts one: no claim follows a holder that already holds the role, and
// no keep follows a role that has one. The handover walks the lane again itself, after a back-off,
// so the copy reaches the holder its sender was told it was kept for.
func TestAKeptCopyWhoseForwardFailedReachesTheHolderWithNoSecondClaim(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const (
		role   = "kept-handover-retry"
		holder = "ses_kept_handover_retry"
	)
	lane := newKeptLaneUnderClaim(t, harness, func(attempt int, _ string, _ contracts.Envelope) error {
		if attempt == 1 {
			return fmt.Errorf("the forward this listener's first walk of the lane ended on")
		}
		return nil
	})
	keys := lane.keep(t, role, "kept-handover-retry", 1)
	lane.register(t, holder)

	lane.claim(t, holder, role)
	handed := lane.await(t, 1, 30*time.Second)

	if handed[0].holder != holder || handed[0].key != keys[0] {
		t.Fatalf("the copy that reached a holder is %+v, want %s to %s", handed[0], keys[0], holder)
	}
}

// A drain forwards to the session it was started for and reads no claim again, so a role that
// changes holder under it would have the rest of its lane forwarded to a session that no longer
// holds it - the two-holder case the kept lane exists to avoid. The drain reads the role's current
// holder from the registry's watched copy before each copy instead, leaves the lane when it is no
// longer its own session's, and the handover walks it again for the holder it finds then.
func TestADrainStopsWhenTheRoleChangesHolderUnderIt(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const (
		role    = "kept-handover-moved"
		first   = "ses_kept_handover_first"
		second  = "ses_kept_handover_second"
		copies  = 4
		changed = 2
	)
	lane := newKeptLaneUnderClaim(t, harness, func(attempt int, _ string, _ contracts.Envelope) error {
		if attempt == changed {
			// The role moves to its second holder exactly here, with half the lane still on it:
			// a claim another listener took, which this listener's drain reads but did not make.
			if _, err := harness.registry.SetRole(second, "test-machine", role, false); err != nil {
				t.Errorf("claim %s for %s: %v", role, second, err)
			}
		}
		return nil
	})
	keys := lane.keep(t, role, "kept-handover-moved", copies)
	lane.register(t, first)
	lane.register(t, second)

	lane.claim(t, first, role)
	handed := lane.await(t, copies, 30*time.Second)

	for index, forwarded := range handed {
		want := first
		if index >= changed {
			want = second
		}
		if forwarded.holder != want || forwarded.key != keys[index] {
			t.Fatalf("copy %d is %+v, want %s to %s: the copies kept after the role changed holder are the new holder's",
				index, forwarded, keys[index], want)
		}
	}
}

// A lane of copies past roleKeepWindow is walked through rather than given up on. A walk that only
// drops or passes copies over forwards nothing, and a handover that read such walks as idle gave
// the lane up after roleKeepDrainIdleRounds of them: a role left unheld for long enough gathers
// more expired copies than those walks read, and every copy behind them - each with a sender
// answered 202 kept and a live holder waiting - was stranded until it expired too. A walk that
// moved the role's cursor made progress, so the handover goes on to the copies behind them.
func TestAHandoverWalksPastMoreExpiredCopiesThanItsIdleWalksAllow(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const (
		role    = "kept-handover-expired"
		holder  = "ses_kept_handover_expired"
		expired = (roleKeepDrainIdleRounds + 1) * roleKeepScanMax
		fresh   = 3
	)
	cfg := harness.config
	var forwarded []string
	cfg.forwardRole = inlineForward(func(_ string, item contracts.Envelope, _ time.Duration) error {
		forwarded = append(forwarded, item.DedupeKey)
		return nil
	})
	var want []string
	for i := range expired + fresh {
		item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, fmt.Sprintf("kept-handover-expired-%04d", i))
		if !keepRoleMessage(harness.client, cfg.logger, role, item) {
			t.Fatalf("keep copy %d; the listener log says why:\n%s", i, harness.logs.String())
		}
		if i >= expired {
			want = append(want, roleForwardDedupePrefix+item.DedupeKey)
		}
	}
	// The drain's clock, read once for each copy it judges and in the order the copies lie on the
	// lane: the first expired of them are read as past the keep window, the rest as fresh. The
	// age is the stream's own store time, which no caller may choose, so the clock moves rather
	// than the copies aging.
	judged := 0
	now := func() time.Time {
		judged++
		if judged <= expired {
			return time.Now().Add(roleKeepWindow + time.Minute)
		}
		return time.Now()
	}
	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := drainKeptRole(ctx, cfg, role, now); err != nil {
		t.Fatalf("drain role %s's kept lane: %v", role, err)
	}

	if !slices.Equal(forwarded, want) {
		t.Fatalf("the handover forwarded %v, want the %d copies lying behind %d expired ones: %v", forwarded, fresh, expired, want)
	}
}

// A handover does not spend its budget of walks that made no progress on walks that failed. A bus
// that is not answering is a reason to walk the lane again rather than to leave it: the copies on
// it have senders answered 202 kept and a live holder waiting, and no claim and no keep will start
// another handover while that holder holds the role. The handover retries on a cadence that widens
// instead (roleKeepDrainBackoff), for longer than roleKeepDrainIdleRounds walks.
func TestAHandoverRetriesMoreFailedWalksThanItsIdleWalksAllow(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const (
		role     = "kept-handover-failures"
		holder   = "ses_kept_handover_failures"
		failures = roleKeepDrainIdleRounds + 1
	)
	lane := newKeptLaneUnderClaim(t, harness, func(attempt int, _ string, _ contracts.Envelope) error {
		if attempt <= failures {
			return fmt.Errorf("forward %d, which this listener's bus did not take", attempt)
		}
		return nil
	})
	keys := lane.keep(t, role, "kept-handover-failures", 1)
	lane.register(t, holder)

	lane.claim(t, holder, role)
	handed := lane.await(t, 1, 90*time.Second)

	if handed[0].holder != holder || handed[0].key != keys[0] {
		t.Fatalf("the copy that reached a holder is %+v, want %s to %s after %d failed walks", handed[0], keys[0], holder, failures)
	}
}

// Each walk of a handover forwards to the holder the pass that started it resolved, and that
// resolution is made after the pause before the walk rather than before it. A handover that
// resolved the holder and then waited forwarded to a session read up to roleKeepClaimWindow ago,
// which the walk's own check cannot catch: a role nobody claims reads as no evidence that this
// walk's session lost it (roleKeepDrainHolds), so the copies went to a session that had released
// the role and the cursor moved past them.
func TestAHandoverResolvesTheHolderOfAWalkAfterThePauseBeforeIt(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const (
		role   = "kept-handover-released"
		holder = "ses_kept_handover_released"
		copies = 2
	)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for i := range copies {
		item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, fmt.Sprintf("kept-handover-released-%02d", i))
		if !keepRoleMessage(harness.client, harness.config.logger, role, item) {
			t.Fatalf("keep copy %d; the listener log says why:\n%s", i, harness.logs.String())
		}
	}
	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})

	var attempts []string
	failed, released := make(chan struct{}), make(chan struct{})
	cfg := harness.config
	cfg.forwardRole = inlineForward(func(_ string, item contracts.Envelope, _ time.Duration) error {
		attempts = append(attempts, item.DedupeKey)
		if len(attempts) > 1 {
			return nil
		}
		// The walk ends on this forward, which earns the handover a pause before its next one.
		close(failed)
		return errors.New("the forward this listener's first walk of the lane ended on")
	})
	// The holder releases the role inside that pause, which is where a handover that had resolved
	// its holder before the pause could not see it.
	go func() {
		defer close(released)
		<-failed
		time.Sleep(roleKeepDrainRetryPause / 2)
		if err := harness.registry.Remove(ctx, holder, []string{contracts.RoleTopicPrefix + role}); err != nil {
			t.Errorf("release %s's claim of %s: %v", holder, role, err)
		}
	}()

	err := drainKeptRole(ctx, cfg, role, time.Now)
	<-released

	if len(attempts) != 1 {
		t.Fatalf("the handover forwarded %v; only the first was attempted while %s held the role, and the copies behind it belong to whoever claims it next", attempts, holder)
	}
	// A handover reports the error of its last walk, which is the forward this one failed: one
	// that reported none no longer ends its first walk where this test needs it to.
	if err == nil {
		t.Fatalf("the handover of role %s's kept lane reported no error, so its first walk no longer ends on a forward that failed", role)
	}
}

// The sweep is the backstop under which no kept lane waits for a claim that does not come. A
// handover runs in a goroutine of the process that started it, so a listener restarted or replaced
// mid-handover ends it with copies behind its cursor, and nothing starts another: a keep happens
// only while a role has no live holder, and a live holder claims no second time. One pass of the
// sweep hands such a lane to the role's live holder with no claim and no keep behind it.
func TestTheSweepHandsOverALaneNoHandoverIsDraining(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const (
		role   = "kept-sweep-stranded"
		holder = "ses_kept_sweep_stranded"
		copies = 3
	)
	cfg := harness.config
	forwards := make(chan string, 16)
	cfg.forwardRole = inlineForward(func(_ string, item contracts.Envelope, _ time.Duration) error {
		forwards <- item.DedupeKey
		return nil
	})
	var keys []string
	for i := range copies {
		item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, fmt.Sprintf("kept-sweep-%02d", i))
		if !keepRoleMessage(harness.client, cfg.logger, role, item) {
			t.Fatalf("keep copy %d; the listener log says why:\n%s", i, harness.logs.String())
		}
		keys = append(keys, roleForwardDedupePrefix+item.DedupeKey)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	first, err := harness.client.NextStoredMessage(ctx, bus.KeptRoleSubject(role), 1)
	if err != nil {
		t.Fatalf("read the first copy on role %s's kept lane: %v", role, err)
	}
	// The cursor a listener that ended mid-handover leaves: it is finished with the first copy
	// and the copies behind that one are nobody's. It is written as the bytes a drain writes
	// rather than through a drain, so no handover of this role has run in this process.
	cursor := []byte(fmt.Sprintf(`{"kept":%d,"claiming":0}`, first.Sequence))
	if _, err := harness.client.PublishCursorAt(ctx, bus.KeptRoleCursorSubject(role), cursor, 0); err != nil {
		t.Fatalf("write the cursor role %s's ended handover left: %v", role, err)
	}
	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})

	sweepKeptRoleLanes(ctx, cfg)

	left := map[string]bool{keys[1]: true, keys[2]: true}
	for deadline := time.After(30 * time.Second); len(left) > 0; {
		select {
		case key := <-forwards:
			if key == keys[0] {
				t.Fatalf("the sweep's handover forwarded %s, the copy the cursor was already finished with", key)
			}
			delete(left, key)
		case <-deadline:
			t.Fatalf("the sweep left %d of the copies behind role %s's cursor on its lane:\n%s", len(left), role, harness.logs.String())
		}
	}
}

// Dispatch's outbox publishes a role-routed event straight onto the role lane as a core message,
// and sends it once: it marks the event published as soon as NATS accepted it, which for a role
// topic is a flush with no receipt behind it, reads no no_holder exception and never sends that
// event again. So an event routed to a role no session held was dropped. The envelope publishRoute
// builds carries no keep flag, so the listener keeps it as it keeps any role message, and the
// role's next claim is handed it.
func TestADispatchRoleRoutedEventToAnUnheldRoleReachesTheNextHolder(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const (
		role   = "kept-dispatch-route"
		holder = "ses_kept_dispatch_route"
	)
	lane := newKeptLaneUnderClaim(t, harness, func(int, string, contracts.Envelope) error { return nil })

	// The shape publishRoute hands the bus for a role route: the event's own envelope, the role
	// topic, and no keep flag at all.
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "dispatch-route-1")
	item.Keep = nil
	harness.coreHandler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
	waitForKeptRoleMessage(t, harness.client, role, item.EventID)

	lane.register(t, holder)
	lane.claim(t, holder, role)

	handed := lane.await(t, 1, 30*time.Second)
	if handed[0].holder != holder || handed[0].key != roleForwardDedupePrefix+item.DedupeKey {
		t.Fatalf("the Dispatch event reached %+v, want %s to %s", handed[0], roleForwardDedupePrefix+item.DedupeKey, holder)
	}
}
