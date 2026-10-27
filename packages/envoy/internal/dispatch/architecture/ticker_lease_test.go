package architecture

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// architectureLeases is the pool an import pass takes its lease on, the one cmd/dispatch gives
// the ticker.
func architectureLeases(t *testing.T, database *store.Store) duty.Leases {
	t.Helper()
	leases, err := database.Pool.Leases()
	if err != nil {
		t.Fatalf("open the leases pool: %v", err)
	}
	return leases
}

// Every import pass re-reads each enabled source from GitHub against one installation's rate
// limit, so one process imports however many tick: with the architecture-import lease held by
// another caller - the other envoy-dispatch process, inside an import of its own - this process's
// pass reads nothing from GitHub and writes no component.
func TestAnImportPassWhoseLeaseIsHeldElsewhereReadsNothing(t *testing.T) {
	ctx := context.Background()
	fake := &fakeSource{t: t, files: map[string]string{"core.md": "# Core\n"}}
	importer, database := newImporterFixture(t, fake)
	leases := architectureLeases(t, database)

	// 0: this caller stands for the other process's pass while it is inside it, which is all
	// this row is about, and records no pass of its own -- so what the passes below do is the
	// lease's doing alone and not the import interval's.
	taken, release := make(chan struct{}), make(chan struct{})
	held := make(chan bool, 1)
	go func() {
		held <- duty.Exclusive(ctx, leases, architectureImportLease, time.Time{}, 0, func(context.Context) {
			close(taken)
			<-release
		})
	}()
	select {
	case <-taken:
	case <-time.After(20 * time.Second):
		t.Fatal("the other caller never took the architecture-import lease")
	}

	syncAllLeased(ctx, importer, leases, time.Now())
	if trees, blobs := fake.reads(); trees != 0 || blobs != 0 {
		t.Fatalf("the pass read %d trees and %d blobs under a lease held elsewhere; want none", trees, blobs)
	}
	if rows := componentRows(t, database); len(rows) != 0 {
		t.Fatalf("the pass imported %v under a lease held elsewhere; want nothing", rows)
	}
	close(release)
	if !<-held {
		t.Fatal("the other caller never held the lease, so the pass was free to import")
	}

	// The lease is the pass's, not the process's: the next pass takes it and imports.
	syncAllLeased(ctx, importer, leases, time.Now())
	if trees, _ := fake.reads(); trees == 0 {
		t.Fatal("the pass read no tree with the lease free")
	}
	if rows := componentRows(t, database); len(rows) == 0 {
		t.Fatal("the pass imported no component with the lease free")
	}
}
