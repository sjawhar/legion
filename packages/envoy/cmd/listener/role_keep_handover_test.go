package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
// drain is started the way production starts one - by a claim through that route
// (startKeptRoleDrain), or by a keep (drainAfterKeep) - rather than by calling the drain itself,
// since what these tests are about is what happens after one walk of the lane ends.
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
