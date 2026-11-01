package api

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// maintenanceTestInterval is the pace both runners keep here, far shorter than the server's
// minute so a handful of periods passes inside one test.
const maintenanceTestInterval = 300 * time.Millisecond

// keySweep is the statement a pass of the key sweep is recognised by (sweepRequestKeys).
const keySweep = "delete from request_keys"

// startOfPeriodAfterNext is the start of the period after the one running now, as duty.Exclusive
// buckets a tick's due time (periodBoundary: Unix nanoseconds less their remainder).
func startOfPeriodAfterNext(interval time.Duration) time.Time {
	ns := time.Now().UnixNano()
	return time.Unix(0, ns-ns%interval.Nanoseconds()).Add(interval)
}

// Two envoy-dispatch processes run the maintenance sweep on one database, each on a pool of its
// own (storetest.SecondProcess, as a second process has). The keys past their window and the
// deliveries a dead process owed need one pass per period across the two, not one per process:
// each pass records its period under the lease (duty.RunLeased), so the other process's tick
// inside a period already served makes no statement of its own.
func TestTheMaintenanceLoopSweepsOncePerPeriodAcrossTwoRunners(t *testing.T) {
	database := storetest.Open(t)
	second := storetest.SecondProcess(t, database)
	recorders := make([]*storetest.StatementRecorder, 0, 2)
	swept := func() int {
		return countStatements(recorders[0].Statements(), keySweep) +
			countStatements(recorders[1].Statements(), keySweep)
	}
	// The resume is the last step of a pass, run after the key sweep's delete has returned and
	// still under the lease, so no pass of the other runner is in flight while it runs: there the
	// key sweeps that ever started are exactly the passes that ran, this one included. The
	// recorder sees a delete as it starts, so a count taken anywhere else could include a pass
	// still in its round trip.
	var resumes atomic.Int64
	var unpaired atomic.Pointer[string]
	resume := func(context.Context) error {
		pass := resumes.Add(1)
		if sweeps := swept(); int64(sweeps) != pass {
			message := fmt.Sprintf("pass %d resumed deliveries after %d key sweeps; every pass runs both", pass, sweeps)
			unpaired.CompareAndSwap(nil, &message)
		}
		return nil
	}
	runners := make([]func(context.Context), 0, 2)
	for _, process := range []*store.Store{database, second} {
		recorder := &storetest.StatementRecorder{}
		pool := storetest.Traced(t, process.Pool, recorder, nil)
		leases := sweepLeases(t, &store.Store{Pool: pool})
		recorders = append(recorders, recorder)
		runners = append(runners, func(ctx context.Context) {
			RunMaintenanceSweep(ctx, pool, maintenanceTestInterval, leases, resume)
		})
	}

	// Both runners start a quarter of a period past a boundary, so every tick of both falls well
	// inside one bucket rather than straddling two: this row is about the two processes' ticks
	// inside one period, not about which bucket a tick landed in.
	started := startOfPeriodAfterNext(maintenanceTestInterval).Add(maintenanceTestInterval / 4)
	time.Sleep(time.Until(started))
	for _, run := range runners {
		runSweep(t, run)
	}

	const passes = 6
	waitUntil(t, 30*time.Second, "six completed passes across the two runners", func() bool { return resumes.Load() >= passes })
	if elapsed, least := time.Since(started), (passes-1)*maintenanceTestInterval; elapsed < least {
		t.Fatalf("%d passes over %v across the two runners; one pass per period of %v takes at least %v: both processes are sweeping every period",
			resumes.Load(), elapsed, maintenanceTestInterval, least)
	}
	if message := unpaired.Load(); message != nil {
		t.Fatal(*message)
	}
}
