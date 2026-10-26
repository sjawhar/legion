package enroll

import (
	"context"
	"errors"
	"testing"
	"time"
)

// notServingAfter is the threshold every clock in this file records: a gap between beats longer
// than this is time no broker was serving.
const notServingAfter = 15 * time.Second

// servingRig is a Service on a fresh schema, the serving clock beating on its store, and one
// person's launcher credential to enroll sessions under.
type servingRig struct {
	svc   *Service
	clock *ServingClock
	cred  Credential
}

func newServingRig(t *testing.T) servingRig {
	t.Helper()
	svc := newService(t)
	return servingRig{
		svc:   svc,
		clock: &ServingClock{Store: svc.Store, Interval: 10 * time.Millisecond, NotServingAfter: notServingAfter, Lease: svc.Lease},
		cred:  mintCredential(t, svc, str("ada@example.com"), nil, "devbox"),
	}
}

// enroll creates a live session enrollment under the rig's credential.
func (r servingRig) enroll(t *testing.T, runtimeID string) string {
	t.Helper()
	e, err := r.svc.Create(context.Background(), r.cred, Enrollment{Kind: "session", RuntimeID: runtimeID, Operator: str("ada@example.com"), Thumbprint: "tp-" + runtimeID})
	if err != nil {
		t.Fatalf("Create(%s): %v", runtimeID, err)
	}
	return e.ID.String()
}

// exec runs one statement on the rig's store, failing t when it fails.
func (r servingRig) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := r.svc.Store.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// setLease sets enrollment id's lease to expr, an SQL expression.
func (r servingRig) setLease(t *testing.T, id, expr string) {
	t.Helper()
	r.exec(t, `update enrollments set lease_expires_at = `+expr+` where id=$1`, id)
}

func (r servingRig) lease(t *testing.T, id string) time.Time {
	t.Helper()
	var lease time.Time
	if err := r.svc.Store.Pool.QueryRow(context.Background(), `select lease_expires_at from enrollments where id=$1`, id).Scan(&lease); err != nil {
		t.Fatal(err)
	}
	return lease
}

// serving reads the broker_serving row: when a broker last served and the threshold it recorded
// (nil for none).
func (r servingRig) serving(t *testing.T) (time.Time, *time.Duration) {
	t.Helper()
	var last time.Time
	var threshold *time.Duration
	if err := r.svc.Store.Pool.QueryRow(context.Background(), `select last_serving_at, not_serving_after from broker_serving`).Scan(&last, &threshold); err != nil {
		t.Fatal(err)
	}
	return last, threshold
}

// now is Postgres's clock_timestamp(), the clock a beat reads.
func (r servingRig) now(t *testing.T) time.Time {
	t.Helper()
	var now time.Time
	if err := r.svc.Store.Pool.QueryRow(context.Background(), `select clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

// beat runs one Beat and answers the database's clock just before and just after it, the bounds
// of the time it read.
func (r servingRig) beat(t *testing.T) (before, after time.Time) {
	t.Helper()
	before = r.now(t)
	if err := r.clock.Beat(context.Background()); err != nil {
		t.Fatalf("Beat: %v", err)
	}
	return before, r.now(t)
}

func (r servingRig) live(t *testing.T, id string) bool {
	t.Helper()
	_, live, err := r.svc.Lookup(context.Background(), id)
	if err != nil {
		t.Fatalf("Lookup(%s): %v", id, err)
	}
	return live
}

// endLapsed runs EndLapsed and answers the ids it ended.
func (r servingRig) endLapsed(t *testing.T) []string {
	t.Helper()
	ended, err := r.svc.EndLapsed(context.Background())
	if err != nil {
		t.Fatalf("EndLapsed: %v", err)
	}
	ids := []string{}
	for _, e := range ended {
		ids = append(ids, e.ID)
	}
	return ids
}

// within fails t unless got lies in [low, high].
func within(t *testing.T, what string, got, low, high time.Time) {
	t.Helper()
	if got.Before(low) || got.After(high) {
		t.Fatalf("%s = %s, want it between %s and %s", what, got, low, high)
	}
}

// Until a broker has beaten, the row holds no threshold, so a lease is live exactly while it has
// not run out, however long ago the row says a broker served: a lapsed lease is refused and swept.
func TestBeforeAnyBeatALapsedLeaseIsLapsed(t *testing.T) {
	r := newServingRig(t)
	id := r.enroll(t, "box-unbeaten")
	r.exec(t, `update broker_serving set last_serving_at = now() - interval '1 hour'`)
	r.setLease(t, id, `now() - interval '1 minute'`)

	if r.live(t, id) {
		t.Fatal("Lookup answered live a lease that ran out before any broker beat")
	}
	if ended := r.endLapsed(t); len(ended) != 1 || ended[0] != id {
		t.Fatalf("EndLapsed ended %v, want just %s", ended, id)
	}
}

// A gap between beats no longer than the threshold is a broker still serving: a lease that ran out
// inside it is lapsed, and the next beat moves only the clock.
func TestABeatAfterAGapWithinTheThresholdMovesNoLease(t *testing.T) {
	r := newServingRig(t)
	r.beat(t)
	id := r.enroll(t, "box-short-gap")
	r.exec(t, `update broker_serving set last_serving_at = now() - interval '5 seconds'`)
	r.setLease(t, id, `now() - interval '1 second'`)
	last, _ := r.serving(t)
	lease := r.lease(t, id)

	if r.live(t, id) {
		t.Fatal("Lookup answered live a lease that ran out while a broker was serving")
	}
	before, after := r.beat(t)
	if got := r.lease(t, id); !got.Equal(lease) {
		t.Fatalf("lease after a beat within the threshold = %s, want it left at %s", got, lease)
	}
	moved, threshold := r.serving(t)
	within(t, "last_serving_at after the beat", moved, before, after)
	if !moved.After(last) || threshold == nil || *threshold != notServingAfter {
		t.Fatalf("broker_serving after the beat = %s, %v; want it moved past %s with threshold %s", moved, threshold, last, notServingAfter)
	}
	if ended := r.endLapsed(t); len(ended) != 1 || ended[0] != id {
		t.Fatalf("EndLapsed ended %v, want just %s", ended, id)
	}
}

// After a gap longer than the threshold, a lease that ran out after the last beat is live to Lookup
// and Renew and EndLapsed leaves it; one that ran out before the last beat, and a revoked one, are
// not. The next beat moves each unended lease still running at the last beat on by the gap, no
// further than one Lease past the beat, leaves the others, and turns the guard off: the restored
// lease is live on its own value.
func TestTheTimeNoBrokerServedIsGivenBackToTheLeasesItRanOut(t *testing.T) {
	r := newServingRig(t)
	ctx := context.Background()
	r.beat(t)
	restorable := r.enroll(t, "box-restorable")
	renewer := r.enroll(t, "box-renewer")
	before := r.enroll(t, "box-ran-out-before")
	revoked := r.enroll(t, "box-revoked")
	capped := r.enroll(t, "box-capped")
	if err := r.svc.Revoke(ctx, r.cred, revoked, "ada@example.com"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	r.exec(t, `update broker_serving set last_serving_at = now() - interval '10 minutes'`)
	r.setLease(t, restorable, `now() - interval '1 minute'`)
	r.setLease(t, renewer, `now() - interval '1 minute'`)
	r.setLease(t, revoked, `now() - interval '1 minute'`)
	r.setLease(t, before, `now() - interval '20 minutes'`)
	r.setLease(t, capped, `now() + interval '55 minutes'`)

	for id, want := range map[string]bool{restorable: true, renewer: true, before: false, revoked: false, capped: true} {
		if got := r.live(t, id); got != want {
			t.Fatalf("Lookup(%s) live = %v, want %v", id, got, want)
		}
	}
	if ended := r.endLapsed(t); len(ended) != 1 || ended[0] != before {
		t.Fatalf("EndLapsed ended %v, want just the lease that ran out before the last beat, %s", ended, before)
	}
	if _, err := r.svc.Renew(ctx, renewer, ""); err != nil {
		t.Fatalf("Renew of a lease that ran out while no broker served: %v", err)
	}
	if _, err := r.svc.Renew(ctx, before, ""); !errors.Is(err, ErrNotLive) {
		t.Fatalf("Renew of the ended enrollment = %v, want ErrNotLive", err)
	}

	last, _ := r.serving(t)
	old := map[string]time.Time{}
	for _, id := range []string{restorable, before, revoked, capped} {
		old[id] = r.lease(t, id)
	}
	beatBefore, beatAfter := r.beat(t)

	within(t, "the restored lease", r.lease(t, restorable), old[restorable].Add(beatBefore.Sub(last)), old[restorable].Add(beatAfter.Sub(last)))
	within(t, "the capped lease", r.lease(t, capped), beatBefore.Add(r.clock.Lease), beatAfter.Add(r.clock.Lease))
	within(t, "the renewed lease", r.lease(t, renewer), beatBefore, beatAfter.Add(r.clock.Lease))
	for _, id := range []string{before, revoked} {
		if got := r.lease(t, id); !got.Equal(old[id]) {
			t.Fatalf("lease of ended enrollment %s = %s after the beat, want it left at %s", id, got, old[id])
		}
	}
	moved, _ := r.serving(t)
	within(t, "last_serving_at after the beat", moved, beatBefore, beatAfter)

	var guard bool
	if err := r.svc.Store.Pool.QueryRow(ctx, `select enrollment_lease_live(now() - interval '1 second')`).Scan(&guard); err != nil {
		t.Fatal(err)
	}
	if guard {
		t.Fatal("enrollment_lease_live answered a lease that just ran out live after the beat gave the time back")
	}
	if !r.lease(t, restorable).After(r.now(t)) || !r.live(t, restorable) {
		t.Fatalf("the restored lease %s is not live on its own value", r.lease(t, restorable))
	}
	if ended := r.endLapsed(t); len(ended) != 0 {
		t.Fatalf("EndLapsed after the beat ended %v, want nothing", ended)
	}
}

// Two brokers beating at once give the time back once: the second waits on the row the first
// holds, then finds the gap the first closed.
func TestTwoBeatsAtOnceGiveTheTimeBackOnce(t *testing.T) {
	r := newServingRig(t)
	ctx := context.Background()
	r.beat(t)
	id := r.enroll(t, "box-concurrent")
	r.exec(t, `update broker_serving set last_serving_at = now() - interval '10 minutes'`)
	r.setLease(t, id, `now() - interval '1 minute'`)
	last, _ := r.serving(t)
	old := r.lease(t, id)

	holder, err := r.svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	var pid int
	if err := holder.QueryRow(ctx, `select pg_backend_pid() from broker_serving for update`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	start := r.now(t)
	done := make(chan error, 2)
	for range 2 {
		go func() { done <- r.clock.Beat(ctx) }()
	}
	awaitServingWaiters(t, r, pid, 2)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("Beat: %v", err)
		}
	}
	end := r.now(t)

	within(t, "the lease after two beats", r.lease(t, id), old.Add(start.Sub(last)), old.Add(end.Sub(last)))
}

// awaitServingWaiters waits until n backends wait, directly or behind another waiter, on holder's
// lock (as api's awaitWaiters does).
func awaitServingWaiters(t *testing.T, r servingRig, holder, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := r.svc.Store.Pool.QueryRow(context.Background(), `select count(*) from pg_stat_activity a
			where $1 = any(pg_blocking_pids(a.pid)) or exists (select 1 from pg_stat_activity b
				where $1 = any(pg_blocking_pids(b.pid)) and b.pid = any(pg_blocking_pids(a.pid)))`, holder).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d backends wait on broker_serving's lock, want %d", waiting, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The clock never moves backward: a beat that finds last_serving_at ahead of its own time leaves it
// there.
func TestABeatNeverMovesTheClockBackward(t *testing.T) {
	r := newServingRig(t)
	r.exec(t, `update broker_serving set last_serving_at = now() + interval '1 hour'`)
	ahead, _ := r.serving(t)
	r.beat(t)
	if got, threshold := r.serving(t); !got.Equal(ahead) || threshold == nil || *threshold != notServingAfter {
		t.Fatalf("broker_serving after the beat = %s, %v; want it left at %s with threshold %s", got, threshold, ahead, notServingAfter)
	}
}

// With the row gone a lease is live exactly while it has not run out, and the next beat recreates
// the row as of its own time.
func TestABeatRecreatesADeletedRow(t *testing.T) {
	r := newServingRig(t)
	r.beat(t)
	id := r.enroll(t, "box-no-row")
	r.exec(t, `delete from broker_serving`)
	r.setLease(t, id, `now() - interval '1 minute'`)
	if r.live(t, id) {
		t.Fatal("Lookup answered live a lapsed lease with no broker_serving row")
	}
	before, after := r.beat(t)
	last, threshold := r.serving(t)
	within(t, "the recreated last_serving_at", last, before, after)
	if threshold == nil || *threshold != notServingAfter {
		t.Fatalf("the recreated threshold = %v, want %s", threshold, notServingAfter)
	}
}

// Run beats as soon as it starts and returns once its context ends.
func TestRunBeatsAtOnceAndReturnsWhenItsContextEnds(t *testing.T) {
	r := newServingRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.clock.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, threshold := r.serving(t); threshold != nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("Run never beat")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}
