package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// noClaimStore answers every boot token as unknown, with no store behind it: a fake for tests that
// only care whether helloResolver reaches the resolve at all, not what it answers.
type noClaimStore struct{}

func (noClaimStore) ClaimByBootTokenHash(context.Context, []byte) (supervise.Claim, bool, error) {
	return supervise.Claim{}, false, nil
}

// A hello that arrives before restoration — a live pane reconnecting while the boot's readiness
// gate still waits out an unreachable NATS or Dispatch (daemon.go's run, LEGION-580) — is held,
// not rejected after worker_rpc_timeout: the resolver blocks on restoration alone, with no
// deadline of its own, and is judged only once restoration closes s.restored (as it does in
// daemon.go's run(), right after "legion daemon started" is logged).
func TestHelloResolverHoldsAHelloUntilRestoredRatherThanRejectingItAfterTheTimeout(t *testing.T) {
	sup := newSupervisor(context.Background(), nil, "PROJECT", "", quietLogger())
	const resolveTimeout = 20 * time.Millisecond
	resolve := sup.helloResolver(api.NewBootTokens(noClaimStore{}), resolveTimeout)

	done := make(chan struct{})
	go func() {
		resolve("some-boot-token")
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("the resolver returned before restoration; it must hold the hello, not reject it on the resolve timeout")
	case <-time.After(10 * resolveTimeout):
		// Comfortably longer than the resolve timeout the old code would have rejected within:
		// still blocked proves the wait is on restoration, not that bound.
	}

	close(sup.restored)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the resolver never returned after restoration closed")
	}
}
