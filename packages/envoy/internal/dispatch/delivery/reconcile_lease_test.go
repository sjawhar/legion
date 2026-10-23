package delivery

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
)

// Every reconcile pass searches and lists GitHub against one installation's rate limit, so one
// process reconciles however many tick: with the delivery-reconcile lease held by another caller
// - the other envoy-dispatch process, inside a pass of its own - this process's pass asks GitHub
// for nothing and leaves the settings' freshness untouched.
func TestAReconcilePassWhoseLeaseIsHeldElsewhereAsksGitHubNothing(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	leases := deliveryTestLeases(t, pool)
	seedDeliverySettings(t, ctx, pool)
	fake := newFakeGitHub(t)
	reconcile := NewReconcile(pool, fake.newTestClient(), events.NewBroker())

	// 0: this caller stands for the other process's pass while it is inside it, which is all
	// this row is about, and records no pass of its own -- so what the passes below do is the
	// lease's doing alone and not the reconcile interval's.
	taken, release := make(chan struct{}), make(chan struct{})
	held := make(chan bool, 1)
	go func() {
		held <- duty.Exclusive(ctx, leases, deliveryReconcileLease, 0, func(context.Context) {
			close(taken)
			<-release
		})
	}()
	select {
	case <-taken:
	case <-time.After(20 * time.Second):
		t.Fatal("the other caller never took the delivery-reconcile lease")
	}

	reconcile.leasedPass(ctx, leases)
	if mints := fake.tokenMints.Load(); mints != 0 {
		t.Fatalf("the pass minted %d installation tokens under a lease held elsewhere; want none", mints)
	}
	settings, err := GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("read the settings after the skipped pass: %v", err)
	}
	if settings.LastReconcileAt != nil || settings.LastError != nil {
		t.Fatalf("the skipped pass recorded last_reconcile_at=%v last_error=%v; want neither",
			settings.LastReconcileAt, settings.LastError)
	}
	close(release)
	if !<-held {
		t.Fatal("the other caller never held the lease, so the pass was free to reconcile")
	}

	// The lease is the pass's, not the process's: the next pass takes it and reconciles.
	reconcile.leasedPass(ctx, leases)
	settings, err = GetSettings(ctx, pool)
	if err != nil {
		t.Fatalf("read the settings after the pass that ran: %v", err)
	}
	if settings.LastReconcileAt == nil && settings.LastError == nil {
		t.Fatal("the pass with the lease free recorded neither a reconcile nor a failure: it did not run")
	}
}
