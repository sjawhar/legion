package daemon

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// fixedClaimStore answers every boot token hash with the one claim it is given, or as unknown
// when found is false: a fake for tests that control exactly what ClaimByBootTokenHash, the
// durable Postgres fact helloResolver checks before ever waiting on restoration, returns.
type fixedClaimStore struct {
	claim supervise.Claim
	found bool
}

func (s fixedClaimStore) ClaimByBootTokenHash(context.Context, []byte) (supervise.Claim, bool, error) {
	return s.claim, s.found, nil
}

// An unknown boot token — no claim this daemon's store holds minted it — is rejected at once,
// bounded by its own resolve timeout, never held waiting for restoration: the worker-stream port
// is plaintext and reachable from any pod, so holding an unresolved connection with no deadline
// would let a garbage token pin a goroutine and a file descriptor for the whole boot wait, with no
// cap on how many a client could open.
func TestHelloResolverRejectsAnUnknownTokenAtOnceDuringTheBootWait(t *testing.T) {
	sup := newSupervisor(context.Background(), nil, "PROJECT", "", quietLogger())
	const resolveTimeout = 20 * time.Millisecond
	resolve := sup.helloResolver(api.NewBootTokens(fixedClaimStore{found: false}), resolveTimeout)

	done := make(chan struct{})
	go func() {
		resolve("garbage-token")
		close(done)
	}()

	select {
	case <-done:
		// Rejected without ever waiting on restoration, which never closes in this test.
	case <-time.After(time.Second):
		t.Fatal("an unknown token was held rather than rejected at once: a garbage token must not pin a goroutine and a file descriptor for the whole boot wait")
	}
}

// A boot token a real claim minted — ClaimByBootTokenHash finds it — is held until restoration,
// not rejected: a live pane reconnecting while the boot's readiness gate still waits out an
// unreachable NATS or Dispatch (daemon.go's run, LEGION-580) waits on s.restored alone, with no
// deadline of its own, and is judged only once restoration closes it (as it does in daemon.go's
// run(), right after "legion daemon started" is logged).
func TestHelloResolverHoldsAKnownTokenUntilRestoredRatherThanRejectingItAfterTheTimeout(t *testing.T) {
	sup := newSupervisor(context.Background(), nil, "PROJECT", "", quietLogger())
	const resolveTimeout = 20 * time.Millisecond
	store := fixedClaimStore{found: true, claim: supervise.Claim{Token: claim.Token("a-claim"), Generation: 1}}
	resolve := sup.helloResolver(api.NewBootTokens(store), resolveTimeout)

	done := make(chan struct{})
	go func() {
		resolve("a-real-boot-token")
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("the resolver returned before restoration; a known token must be held, not rejected on the resolve timeout")
	case <-time.After(10 * resolveTimeout):
		// Comfortably longer than the resolve timeout: still blocked proves the wait is on
		// restoration, not that bound.
	}

	close(sup.restored)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the resolver never returned after restoration closed")
	}
}

// A hold on a known token is released, not left dangling, when the boot gives up rather than
// becoming ready: supervision.stop cancels s.ctx before the worker stream closes
// (daemon.go's run), so every held hello returns rather than leaking the goroutine and the
// connection it would otherwise pin forever.
func TestHelloResolverReleasesAKnownTokenWhenTheBootGivesUpRatherThanBecomingReady(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sup := newSupervisor(ctx, nil, "PROJECT", "", quietLogger())
	const resolveTimeout = 20 * time.Millisecond
	store := fixedClaimStore{found: true, claim: supervise.Claim{Token: claim.Token("a-claim"), Generation: 1}}
	resolve := sup.helloResolver(api.NewBootTokens(store), resolveTimeout)

	done := make(chan struct{})
	go func() {
		resolve("a-real-boot-token")
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("the resolver returned before the boot gave up; a known token must be held until then")
	case <-time.After(10 * resolveTimeout):
		// Comfortably longer than the resolve timeout: still blocked proves the wait is on s.ctx,
		// not that bound.
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the resolver never returned after the boot's context was cancelled; a held hello would pin a goroutine and a connection forever")
	}
}

// relaunchingStore is a supervise.Store/api.BootTokenStore fake whose current row remembers only
// its latest PutClaim, exactly as a real claim row does: ClaimByBootTokenHash finds the hash of
// whichever generation was written last, never an earlier one.
type relaunchingStore struct {
	mu          sync.Mutex
	currentHash []byte
	current     supervise.Claim
}

func (s *relaunchingStore) ClaimByBootTokenHash(_ context.Context, hash []byte) (supervise.Claim, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bytes.Equal(hash, s.currentHash) {
		return s.current, true, nil
	}
	return supervise.Claim{}, false, nil
}

func (s *relaunchingStore) PutClaim(_ context.Context, c supervise.Claim) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentHash = c.BootTokenHash
	s.current = c
	return nil
}

func (s *relaunchingStore) PutDelivery(context.Context, claim.Token, supervise.Delivery) error {
	return nil
}

func (s *relaunchingStore) PutClaimAndDelivery(ctx context.Context, c supervise.Claim, _ supervise.Delivery) error {
	return s.PutClaim(ctx, c)
}

func (s *relaunchingStore) RetireDelivery(ctx context.Context, c supervise.Claim, _ string) error {
	return s.PutClaim(ctx, c)
}

// A claim stored mid-launch across a restart (generation 1, no locator) relaunches to generation
// 2 inside s.start — ahead of close(s.restored) — while a shim's hello for generation 1's own
// token is already held in helloResolver, resolved once before the hold started. Judging that
// hold by the first resolve alone would accept the old generation after its claim has already
// moved on, taking the stream slot the real generation-2 shim's own hello then finds "already
// bound to a live stream". The fix resolves the token again once restoration ends and judges
// generation 1's hello by that second resolve, which answers one of two ways depending on the
// claim's own stored state when this restore ran: supervisor.restore rewrites a claim it finds
// StateLaunching with no locator to StateLaunchUncertain through the same Recording-wrapped store
// a relaunch's own mint goes through, so this process records generation 1's hash even without
// minting it itself, and the second resolve comes back Stale, refused as "stale worker
// generation". A claim already stored as StateLaunchUncertain before this restore ran — a
// still-earlier restore marked it so and then crashed again before ever relaunching it — skips
// that rewrite, so this process never records its hash at all, and the second resolve comes back
// known=false, refused as "unknown boot token" instead. Either way generation 1's hello is
// refused and generation 2's own, freshly minted hello is accepted.
func TestHelloResolverResolvesAgainAfterRestorationSoARelaunchDuringTheHoldIsNotMissed(t *testing.T) {
	type result struct {
		generation uint64
		stale      bool
		known      bool
	}

	t.Run("generation 1 stored as StateLaunching: restore's own rewrite records it, so the second resolve comes back Stale", func(t *testing.T) {
		store := &relaunchingStore{}
		tokens := api.NewBootTokens(store)
		recording := tokens.Recording(store)

		const boot1, boot2 = "generation-1-token", "generation-2-token"
		ctx := context.Background()
		if err := recording.PutClaim(ctx, supervise.Claim{
			Token: claim.Token("a-claim"), Generation: 1, BootTokenHash: supervise.HashBootToken(boot1),
		}); err != nil {
			t.Fatalf("write generation 1's claim: %v", err)
		}

		sup := newSupervisor(context.Background(), nil, "PROJECT", "", quietLogger())
		sup.machines[claim.Token("a-claim")] = &member{}
		const resolveTimeout = 20 * time.Millisecond
		resolve := sup.helloResolver(tokens, resolveTimeout)

		done := make(chan result, 1)
		go func() {
			_, generation, stale, known := resolve(boot1)
			done <- result{generation, stale, known}
		}()

		select {
		case <-done:
			t.Fatal("the resolver returned before restoration; generation 1's hello must be held")
		case <-time.After(10 * resolveTimeout):
		}

		// The relaunch restore (s.start) runs ahead of close(s.restored) in the real boot:
		// generation 1's claim, still held nowhere (no locator persisted), relaunches to
		// generation 2 with its own fresh token before the held hello is ever judged.
		if err := recording.PutClaim(ctx, supervise.Claim{
			Token: claim.Token("a-claim"), Generation: 2, BootTokenHash: supervise.HashBootToken(boot2),
		}); err != nil {
			t.Fatalf("write generation 2's claim: %v", err)
		}
		close(sup.restored)

		select {
		case got := <-done:
			if !got.known {
				t.Fatalf("resolve(%q) known = false, want true", boot1)
			}
			if !got.stale {
				t.Fatalf("resolve(%q) stale = false, want true: restore's rewrite of a StateLaunching claim to StateLaunchUncertain runs through the Recording store too, so this process records generation 1's hash even without minting it itself", boot1)
			}
		case <-time.After(time.Second):
			t.Fatal("the resolver never returned after restoration closed")
		}
	})

	t.Run("generation 1 already stored as StateLaunchUncertain: restore skips the rewrite, so the second resolve comes back unknown", func(t *testing.T) {
		store := &relaunchingStore{}
		const boot1, boot2 = "generation-1-token", "generation-2-token"
		ctx := context.Background()
		// A claim already StateLaunchUncertain from a still-earlier restore that crashed again
		// before ever relaunching it never passes back through supervisor.restore's
		// StateLaunching-to-StateLaunchUncertain rewrite — the one path that would have recorded
		// this hash through Recording — so this process's own BootTokens never records it at
		// all; written here directly to the bare store, bypassing Recording, to model exactly
		// that gap.
		if err := store.PutClaim(ctx, supervise.Claim{
			Token: claim.Token("a-claim"), Generation: 1, BootTokenHash: supervise.HashBootToken(boot1),
		}); err != nil {
			t.Fatalf("write generation 1's claim (already StateLaunchUncertain, as a still-earlier restore would have left it): %v", err)
		}

		tokens := api.NewBootTokens(store)
		recording := tokens.Recording(store)

		sup := newSupervisor(context.Background(), nil, "PROJECT", "", quietLogger())
		sup.machines[claim.Token("a-claim")] = &member{}
		const resolveTimeout = 20 * time.Millisecond
		resolve := sup.helloResolver(tokens, resolveTimeout)

		done := make(chan result, 1)
		go func() {
			_, generation, stale, known := resolve(boot1)
			done <- result{generation, stale, known}
		}()

		select {
		case <-done:
			t.Fatal("the resolver returned before restoration; generation 1's hello must be held")
		case <-time.After(10 * resolveTimeout):
		}

		if err := recording.PutClaim(ctx, supervise.Claim{
			Token: claim.Token("a-claim"), Generation: 2, BootTokenHash: supervise.HashBootToken(boot2),
		}); err != nil {
			t.Fatalf("write generation 2's claim: %v", err)
		}
		close(sup.restored)

		select {
		case got := <-done:
			if got.known {
				t.Fatalf("resolve(%q) known = true, want false: a claim already StateLaunchUncertain skips restore's rewrite, so this process never records its hash and the second resolve must come back unknown", boot1)
			}
		case <-time.After(time.Second):
			t.Fatal("the resolver never returned after restoration closed")
		}

		// Generation 2's own, freshly minted hello is accepted once restoration has already
		// closed.
		launched, generation, stale, known := resolve(boot2)
		if !known || stale || generation != 2 || launched != claim.Token("a-claim") {
			t.Fatalf("resolve(%q) = (%q, %d, stale=%t, known=%t), want (a-claim, 2, false, true)", boot2, launched, generation, stale, known)
		}
	})
}
