package outbox

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// countingDocs counts the compactions asked of it. Only CompactAll is implemented: the embedded
// interface leaves every other method nil, which is what the compaction tick calls - nothing.
type countingDocs struct {
	docs.API
	compactions atomic.Int64
}

func (d *countingDocs) CompactAll(context.Context) error {
	d.compactions.Add(1)
	return nil
}

// leasesOf is the pool a compaction takes its lease on, the one cmd/dispatch gives the outbox.
func leasesOf(t *testing.T, database *store.Store) *pgxpool.Pool {
	t.Helper()
	leases, err := database.Pool.Leases()
	if err != nil {
		t.Fatalf("open the leases pool: %v", err)
	}
	return leases
}

// The daily compaction walks every document, so it runs once however many processes tick: with
// the document-compaction lease held by another caller - the other envoy-dispatch process, inside
// a compaction of its own - this process's tick compacts nothing.
func TestTheCompactionTickWhoseLeaseIsHeldElsewhereCompactsNothing(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	leases := leasesOf(t, database)
	compacting := &countingDocs{}
	deps := Deps{Store: database, Docs: compacting, Leases: leases}

	// 0: this caller stands for the other process's pass while it is inside it, which is all
	// this row is about, and records no pass of its own -- so what the ticks below do is the
	// lease's doing alone and not the compaction period's.
	taken, release := make(chan struct{}), make(chan struct{})
	held := make(chan bool, 1)
	go func() {
		held <- duty.Exclusive(ctx, leases, documentCompactionLease, time.Time{}, 0, func(context.Context) {
			close(taken)
			<-release
		})
	}()
	select {
	case <-taken:
	case <-time.After(20 * time.Second):
		t.Fatal("the other caller never took the document-compaction lease")
	}

	compactDocuments(ctx, deps, time.Now())
	if got := compacting.compactions.Load(); got != 0 {
		t.Fatalf("the tick compacted %d times under a lease held elsewhere; want none", got)
	}
	close(release)
	if !<-held {
		t.Fatal("the other caller never held the lease, so the tick was free to compact")
	}

	// The lease is the pass's, not the process's: the tick after it is given back compacts.
	compactDocuments(ctx, deps, time.Now())
	if got := compacting.compactions.Load(); got != 1 {
		t.Fatalf("the tick compacted %d times with the lease free; want once", got)
	}
}
