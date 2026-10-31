package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
)

// A kept copy whose drain never forwarded it must still reach the role's holder. The drain claims
// a copy by advancing the role's cursor past it and only then forwards it, so a drain that ends
// between the two - a listener killed mid-drain, or a forward that failed - leaves the cursor past
// a copy nothing ever delivered, and no later drain offers it again. Its sender was answered 202
// kept. The rule this pins is the one the architect of dispatch://INFRA-514 set for this tree on
// 2026-10-31: a message answered 202 kept is never lost, and the accepted worst case is a
// duplicate the holder drops by its key.
//
// The forward is failed rather than the process killed, which is the same point in the drain and
// is deterministic: the claim has landed and the forward has not happened. The fix the architect
// settled is claim, forward, confirm - the cursor carries a claimed-but-unconfirmed sequence, and
// the next drain forwards that copy again under the same stable id.
func TestAKeptCopyItsDrainNeverForwardedStillReachesTheRolesHolder(t *testing.T) {
	harness := newListenerDeliveryHarness(t, nil)
	const (
		role   = "kept-claimed-then-lost"
		holder = "ses_kept_claimed_then_lost"
	)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "kept-claimed-then-lost-00")
	if !keepRoleMessage(harness.client, harness.config.logger, role, item) {
		t.Fatalf("keep the copy; the listener log says why:\n%s", harness.logs.String())
	}
	key := roleForwardDedupePrefix + item.DedupeKey

	entry := session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The drain that ends between its claim and its forward. Its claim has already advanced the
	// role's cursor past this copy when the forward it never completes is attempted.
	lost := harness.config
	lost.forwardRole = inlineForward(func(string, contracts.Envelope, time.Duration) error {
		return errors.New("this listener ended between its claim of the copy and its forward")
	})
	if err := drainKeptRole(ctx, lost, role, holder, entry, time.Now); err == nil {
		t.Fatalf("the drain whose forward never completed reported no error, so this test no longer reaches the moment it is about")
	}

	// The listener that replaces it, draining the same role for the same holder. It has guards of
	// its own, which is what a second process has.
	var handed []string
	replacement := harness.config
	replacement.roleKeepDrains = newRoleKeepGuards()
	replacement.forwardRole = inlineForward(func(_ string, forwarded contracts.Envelope, _ time.Duration) error {
		handed = append(handed, forwarded.DedupeKey)
		return nil
	})
	if err := drainKeptRole(ctx, replacement, role, holder, entry, time.Now); err != nil {
		t.Fatalf("the replacement listener's drain of role %s's kept lane: %v", role, err)
	}
	if !slices.Contains(handed, key) {
		t.Fatalf("the kept copy under %s reached no holder after the drain that claimed it ended before forwarding it; the replacement drain handed over %v. A message answered 202 kept is never lost: the worst case is a duplicate the holder drops by its key", key, handed)
	}
}
