package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/session"
)

// The case of two drains at once - each kept copy reaches exactly one holder - is
// TestTwoDrainsOfOneRoleHandEachKeptCopyToExactlyOneHolder in role_keep_test.go, which holds one
// drain inside its first forward while a second listener walks the whole lane. It is not
// duplicated here; what the claim adds to it, a lane left alone while another drain's claim can
// still be forwarding its copy, is TestAClaimNoDrainReleasedHoldsTheLaneUntilItsWindowHasPassed
// below.

// A drain that ended on a forward it could not make loses neither that copy nor the lane behind
// it. The drain claims a copy before it forwards it and moves the cursor past it only once the
// forward is done, so the role's next drain reads the copy the forward never reached again, and
// then the copies that drain never got to - where a cursor advanced at the claim left the failed
// copy behind and offered no later drain a copy nothing delivered.
//
// The failure is on the second of four copies, which is what makes this case its own beside
// TestAKeptCopyItsDrainNeverForwardedStillReachesTheRolesHolder: the copy before it was forwarded
// and confirmed, so it is not handed over twice, and the two copies after it were never read by
// the drain that failed and must still be delivered.
func TestADrainThatFailedAForwardLeavesThatCopyAndTheRestToTheNextDrain(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const (
		role   = "kept-forward-failed"
		holder = "ses_kept_forward_failed"
		copies = 4
	)
	var keys []string
	for i := range copies {
		item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, fmt.Sprintf("kept-forward-failed-%02d", i))
		if !keepRoleMessage(harness.client, harness.config.logger, role, item) {
			t.Fatalf("keep copy %d; the listener log says why:\n%s", i, harness.logs.String())
		}
		keys = append(keys, roleForwardDedupePrefix+item.DedupeKey)
	}

	entry := session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var attempted []string
	failing := harness.config
	failing.forwardRole = inlineForward(func(_ string, forwarded contracts.Envelope, _ time.Duration) error {
		attempted = append(attempted, forwarded.DedupeKey)
		if len(attempted) == 2 {
			return errors.New("the forward this listener ended on")
		}
		return nil
	})
	if err := drainKeptRole(ctx, failing, role, holder, entry, time.Now); err == nil {
		t.Fatalf("the drain whose second forward failed reported no error, so this test no longer reaches the moment it is about")
	}
	if !slices.Equal(attempted, keys[:2]) {
		t.Fatalf("the first drain attempted %v, want the first two copies %v", attempted, keys[:2])
	}

	// The listener that drains the role next, with guards of its own, which is what a second
	// process has.
	var handed []string
	next := harness.config
	next.roleKeepDrains = newRoleKeepGuards()
	next.forwardRole = inlineForward(func(_ string, forwarded contracts.Envelope, _ time.Duration) error {
		handed = append(handed, forwarded.DedupeKey)
		return nil
	})
	if err := drainKeptRole(ctx, next, role, holder, entry, time.Now); err != nil {
		t.Fatalf("the next drain of role %s's kept lane: %v", role, err)
	}
	if !slices.Equal(handed, keys[1:]) {
		t.Fatalf("the next drain handed over %v, want the copy the failed forward never reached and the ones behind it: %v. A message answered 202 kept is never lost", handed, keys[1:])
	}
}

// A drain that ended between its forward and its confirm costs the holder one more copy at most,
// and the stream holds one copy of it on the holder's agent subject. The repeat carries the same
// stable Nats-Msg-Id as the first forward (bus.ForwardMsgID of the forward's dedupe key and the
// agent subject), so the stream's duplicate window collapses the two into the one stored copy the
// holder's session is handed.
//
// The drain's own context is ended inside its forward, after the stream has stored the copy: the
// confirm it would write next reaches nothing, which is the same point in the drain as a listener
// killed there and is deterministic. Nothing released that claim, so the role's next drain takes
// it over once it is past roleKeepClaimWindow.
func TestADrainThatEndedBetweenItsForwardAndItsConfirmLeavesTheHolderOneStoredCopy(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const (
		role   = "kept-confirm-lost"
		holder = "ses_kept_confirm_lost"
	)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "kept-confirm-lost-00")
	if !keepRoleMessage(harness.client, harness.config.logger, role, item) {
		t.Fatalf("keep the copy; the listener log says why:\n%s", harness.logs.String())
	}
	key := roleForwardDedupePrefix + item.DedupeKey
	// A session on a serving port answers no receipt: its forward counts as delivered once the
	// stream has stored it on the session's agent subject (roleForwardStoredOnly), which is the
	// copy this test counts.
	entry := session.SessionEntry{MachineID: "test-machine", Port: 1}
	subject := contracts.AgentSubject(holder)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var handed []string
	record := func(cfg listenerDeliveryHandlerConfig, after func()) func(string, contracts.Envelope, time.Duration, bool) (func() error, error) {
		forward := cfg.forwardRole
		return func(subject string, forwarded contracts.Envelope, timeout time.Duration, storedOnly bool) (func() error, error) {
			wait, err := forward(subject, forwarded, timeout, storedOnly)
			if err != nil {
				return nil, err
			}
			return func() error {
				err := wait()
				handed = append(handed, forwarded.DedupeKey)
				after()
				return err
			}, nil
		}
	}

	dying, cancelDying := context.WithTimeout(ctx, 60*time.Second)
	defer cancelDying()
	lost := harness.config
	// The listener ends here: its forward is published and stored, and the confirm it would
	// write next reaches nothing.
	lost.forwardRole = record(harness.config, cancelDying)
	if err := drainKeptRole(dying, lost, role, holder, entry, time.Now); err == nil {
		t.Fatalf("the drain that ended after its forward reported no error, so this test no longer reaches the moment it is about")
	}

	// The listener that drains the role next, with guards and caches of its own, which is what a
	// second process has: the first listener recorded the copy it forwarded in its dedupe cache
	// (finishRoleForward), and inside one process that record is what stops a copy already handed
	// over from being handed over again. It drains on a clock past roleKeepClaimWindow - which is
	// roleKeepDrainWindow, the whole of a drain's life - so the claim the drain above left behind
	// is one no drain can still be forwarding.
	past := func() time.Time { return time.Now().Add(roleKeepDrainWindow + time.Minute) }
	next := harness.config
	next.roleKeepDrains = newRoleKeepGuards()
	next.dedupeCache = dedupe.New(10 * time.Minute)
	t.Cleanup(next.dedupeCache.Stop)
	next.attemptCache = dedupe.New(5 * time.Minute)
	t.Cleanup(next.attemptCache.Stop)
	next.forwardRole = record(harness.config, func() {})
	if err := drainKeptRole(ctx, next, role, holder, entry, past); err != nil {
		t.Fatalf("the next drain of role %s's kept lane: %v", role, err)
	}
	if !slices.Equal(handed, []string{key, key}) {
		t.Fatalf("the two drains forwarded %v, want the one copy under %s forwarded once by each: the copy is not lost, and at most one more is sent", handed, key)
	}

	first, err := harness.client.NextStoredMessage(ctx, subject, 1)
	if err != nil {
		t.Fatalf("read the first copy stored on %s: %v", subject, err)
	}
	last, err := harness.client.LastStoredMessage(ctx, subject)
	if err != nil {
		t.Fatalf("read the last copy stored on %s: %v", subject, err)
	}
	if first.Sequence != last.Sequence {
		t.Fatalf("the stream holds copies %d to %d on %s, want the one copy the stable forward id keeps there however often the copy is forwarded", first.Sequence, last.Sequence, subject)
	}
}

// A claim no drain released - what a listener killed between its claim and its forward leaves -
// holds the role's lane until it is past roleKeepClaimWindow, and after that the copy it claimed
// is forwarded. Those are the two halves of what the claim is for: while the drain that wrote it
// can still be forwarding that copy, no other drain forwards it a second time, and once it cannot
// be, the copy reaches the role's holder rather than nobody.
func TestAClaimNoDrainReleasedHoldsTheLaneUntilItsWindowHasPassed(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const (
		role   = "kept-claim-abandoned"
		holder = "ses_kept_claim_abandoned"
	)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "kept-claim-abandoned-00")
	if !keepRoleMessage(harness.client, harness.config.logger, role, item) {
		t.Fatalf("keep the copy; the listener log says why:\n%s", harness.logs.String())
	}
	key := roleForwardDedupePrefix + item.DedupeKey

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stored, err := harness.client.LastStoredMessage(ctx, bus.KeptRoleSubject(role))
	if err != nil {
		t.Fatalf("read role %s's kept lane: %v", role, err)
	}
	// The cursor a drain leaves when it is killed with a copy claimed: the lane is finished with
	// nothing, and that copy is claimed and unconfirmed. It is written as the bytes a drain
	// writes rather than through the drain, so the test pins the body both of them read.
	claim := []byte(fmt.Sprintf(`{"kept":0,"claiming":%d}`, stored.Sequence))
	if _, err := harness.client.PublishCursorAt(ctx, bus.KeptRoleCursorSubject(role), claim, 0); err != nil {
		t.Fatalf("write the claim role %s's killed drain left: %v", role, err)
	}

	entry := session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}
	var handed []string
	cfg := harness.config
	cfg.forwardRole = inlineForward(func(_ string, forwarded contracts.Envelope, _ time.Duration) error {
		handed = append(handed, forwarded.DedupeKey)
		return nil
	})
	if err := drainKeptRole(ctx, cfg, role, holder, entry, time.Now); err != nil {
		t.Fatalf("the drain that found the claim outstanding: %v", err)
	}
	if len(handed) != 0 {
		t.Fatalf("a drain forwarded %v while another drain's claim of that copy could still be forwarding it; a kept copy reaches exactly one holder", handed)
	}

	// Past roleKeepClaimWindow - which is roleKeepDrainWindow, the whole of a drain's life - the
	// claim belongs to a drain that has ended, and the copy is the next drain's to forward.
	past := func() time.Time { return time.Now().Add(roleKeepDrainWindow + time.Minute) }
	if err := drainKeptRole(ctx, cfg, role, holder, entry, past); err != nil {
		t.Fatalf("the drain that took the abandoned claim over: %v", err)
	}
	if !slices.Equal(handed, []string{key}) {
		t.Fatalf("the drain past the claim's window handed over %v, want the copy under %s the killed drain claimed and never forwarded", handed, key)
	}
}
