package enroll

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// notServingAfter is the threshold every clock in this file records unless a test says otherwise:
// a gap after the latest beat longer than this is time no broker was serving.
const notServingAfter = 15 * time.Second

// servingInterval is the rig clocks' Interval, and so the bound on each of their beats: far above
// what a beat takes, so no beat in this file fails on its bound unless a test holds it up.
const servingInterval = 5 * time.Second

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
		clock: &ServingClock{Enrollments: svc, Interval: servingInterval, NotServingAfter: notServingAfter},
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

// servingOf reads clock's broker_serving row: when its process last served and the threshold it
// recorded.
func (r servingRig) servingOf(t *testing.T, clock *ServingClock) (time.Time, time.Duration) {
	t.Helper()
	var last time.Time
	var threshold time.Duration
	if err := r.svc.Store.Pool.QueryRow(context.Background(), `select last_serving_at, not_serving_after from broker_serving where process=$1`,
		clock.processID()).Scan(&last, &threshold); err != nil {
		t.Fatalf("read the broker_serving row of process %s: %v", clock.processID(), err)
	}
	return last, threshold
}

// serving reads the rig clock's broker_serving row.
func (r servingRig) serving(t *testing.T) (time.Time, time.Duration) {
	t.Helper()
	return r.servingOf(t, r.clock)
}

// latest reads the latest broker_serving row's last_serving_at, when any broker last served.
func (r servingRig) latest(t *testing.T) time.Time {
	t.Helper()
	var last time.Time
	if err := r.svc.Store.Pool.QueryRow(context.Background(), `select max(last_serving_at) from broker_serving`).Scan(&last); err != nil {
		t.Fatal(err)
	}
	return last
}

// processes answers the process of every broker_serving row, in order.
func (r servingRig) processes(t *testing.T) []string {
	t.Helper()
	var processes []string
	if err := r.svc.Store.Pool.QueryRow(context.Background(), `select coalesce(array_agg(process order by process), '{}') from broker_serving`).Scan(&processes); err != nil {
		t.Fatal(err)
	}
	return processes
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

// beat runs one Beat of the rig clock and answers the database's clock just before and just after
// it, the bounds of the time it read.
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

// logBuffer is a log destination safe to write from several goroutines and read from the test.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureSlog sends slog's default logger to a buffer until t ends, and answers the buffer.
func captureSlog(t *testing.T) *logBuffer {
	t.Helper()
	logged := &logBuffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logged, nil))
	return logged
}

// Until a broker has beaten, broker_serving is empty, so a lease is live exactly while it has not
// run out: a lapsed lease is refused and swept.
func TestBeforeAnyBeatALapsedLeaseIsLapsed(t *testing.T) {
	r := newServingRig(t)
	id := r.enroll(t, "box-unbeaten")
	if processes := r.processes(t); len(processes) != 0 {
		t.Fatalf("broker_serving before any beat holds %v, want no row", processes)
	}
	r.setLease(t, id, `now() - interval '1 minute'`)

	if r.live(t, id) {
		t.Fatal("Lookup answered live a lease that ran out before any broker beat")
	}
	if ended := r.endLapsed(t); len(ended) != 1 || ended[0] != id {
		t.Fatalf("EndLapsed ended %v, want just %s", ended, id)
	}
}

// A gap after the latest beat no longer than the threshold is a broker still serving: a lease that
// ran out inside it is lapsed, and the next beat moves only the clock.
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
	if !moved.After(last) || threshold != notServingAfter {
		t.Fatalf("broker_serving after the beat = %s, %s; want it moved past %s with threshold %s", moved, threshold, last, notServingAfter)
	}
	if ended := r.endLapsed(t); len(ended) != 1 || ended[0] != id {
		t.Fatalf("EndLapsed ended %v, want just %s", ended, id)
	}
}

// After a gap past the latest beat longer than the threshold, a lease that ran out after that beat
// is live to Lookup and Renew and EndLapsed leaves it; one that ran out before the beat, and a
// revoked one, are not. The next beat moves each unended lease still running at the latest beat on
// by the gap, no further than one Lease past the beat, leaves the others, and turns the guard off:
// the restored lease is live on its own value.
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
		t.Fatalf("EndLapsed ended %v, want just the lease that ran out before the latest beat, %s", ended, before)
	}
	if _, err := r.svc.Renew(ctx, renewer, ""); err != nil {
		t.Fatalf("Renew of a lease that ran out while no broker served: %v", err)
	}
	if _, err := r.svc.Renew(ctx, before, ""); !errors.Is(err, ErrNotLive) {
		t.Fatalf("Renew of the ended enrollment = %v, want ErrNotLive", err)
	}

	last := r.latest(t)
	old := map[string]time.Time{}
	for _, id := range []string{restorable, before, revoked, capped} {
		old[id] = r.lease(t, id)
	}
	beatBefore, beatAfter := r.beat(t)

	within(t, "the restored lease", r.lease(t, restorable), old[restorable].Add(beatBefore.Sub(last)), old[restorable].Add(beatAfter.Sub(last)))
	within(t, "the capped lease", r.lease(t, capped), beatBefore.Add(r.svc.Lease), beatAfter.Add(r.svc.Lease))
	within(t, "the renewed lease", r.lease(t, renewer), beatBefore, beatAfter.Add(r.svc.Lease))
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

// Two brokers beating at once after a gap give the time back once: both find the gap, the second
// waits on the lock the first holds while it gives the gap back, then reads the row the first wrote
// and finds no gap.
func TestTwoBeatsAtOnceGiveTheTimeBackOnce(t *testing.T) {
	r := newServingRig(t)
	ctx := context.Background()
	other := &ServingClock{Enrollments: r.svc, Interval: servingInterval, NotServingAfter: notServingAfter}
	r.beat(t)
	if err := other.Beat(ctx); err != nil {
		t.Fatalf("Beat of the other broker: %v", err)
	}
	id := r.enroll(t, "box-concurrent")
	r.exec(t, `update broker_serving set last_serving_at = now() - interval '10 minutes'`)
	r.setLease(t, id, `now() - interval '1 minute'`)
	last := r.latest(t)
	old := r.lease(t, id)

	holder, err := r.svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	if _, err := holder.Exec(ctx, `select pg_advisory_xact_lock($1)`, servingLock); err != nil {
		t.Fatal(err)
	}
	start := r.now(t)
	done := make(chan error, 2)
	for _, clock := range []*ServingClock{r.clock, other} {
		go func() { done <- clock.Beat(ctx) }()
	}
	storetest.AwaitLockWaiters(t, r.svc.Store.Pool, holder, 2)
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
	for _, clock := range []*ServingClock{r.clock, other} {
		moved, _ := r.servingOf(t, clock)
		within(t, "last_serving_at after the beats", moved, start, end)
	}
}

// The clock never moves backward: a beat that finds its row's last_serving_at ahead of its own
// time leaves it there, and records its own threshold.
func TestABeatNeverMovesTheClockBackward(t *testing.T) {
	r := newServingRig(t)
	r.exec(t, `insert into broker_serving (process, last_serving_at, not_serving_after) values ($1, now() + interval '1 hour', interval '1 second')`, r.clock.processID())
	ahead, _ := r.serving(t)
	r.beat(t)
	if got, threshold := r.serving(t); !got.Equal(ahead) || threshold != notServingAfter {
		t.Fatalf("broker_serving after the beat = %s, %s; want it left at %s with threshold %s", got, threshold, ahead, notServingAfter)
	}
}

// With every row gone a lease is live exactly while it has not run out, and the next beat writes
// its process's row as of its own time.
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
	if threshold != notServingAfter {
		t.Fatalf("the recreated threshold = %s, want %s", threshold, notServingAfter)
	}
}

// A beat deletes the rows of processes more than an hour behind the latest row and keeps the rest;
// the latest row is never deleted, even when it is not the beating process's.
func TestABeatPrunesRowsAnHourBehindTheLatest(t *testing.T) {
	r := newServingRig(t)
	r.exec(t, `insert into broker_serving (process, last_serving_at, not_serving_after) values
		('gone', now() - interval '2 hours', interval '15 seconds'),
		('recent', now() - interval '30 minutes', interval '15 seconds')`)
	r.beat(t)
	if got, want := sortedCopy(r.processes(t)), sortedCopy([]string{r.clock.processID(), "recent"}); !slices.Equal(got, want) {
		t.Fatalf("broker_serving after the beat holds %v, want %v", got, want)
	}

	r.exec(t, `delete from broker_serving`)
	r.exec(t, `insert into broker_serving (process, last_serving_at, not_serving_after) values ('ahead', now() + interval '2 hours', interval '15 seconds')`)
	r.beat(t)
	if got := r.processes(t); !slices.Contains(got, "ahead") {
		t.Fatalf("broker_serving after the beat holds %v, want it to keep the latest row, ahead", got)
	}
}

// sortedCopy answers a sorted copy of s.
func sortedCopy(s []string) []string {
	s = slices.Clone(s)
	slices.Sort(s)
	return s
}

// A beat counts the brokers beating at its own instant and hands that count to OnReplicas: its
// own row and every other row whose gap since its last beat is within that row's own threshold.
// A row that stopped beating is not one of them, however recently it is still in the table, so a
// fleet that loses a pod stops counting it a threshold later rather than at the next prune, an
// hour on.
func TestABeatCountsTheBrokersStillBeating(t *testing.T) {
	r := newServingRig(t)
	var counted []int
	r.clock.OnReplicas = func(replicas int) { counted = append(counted, replicas) }
	r.exec(t, `insert into broker_serving (process, last_serving_at, not_serving_after) values
		('peer-1', now(), interval '15 seconds'),
		('peer-2', now(), interval '15 seconds'),
		('peer-3', now(), interval '15 seconds'),
		('peer-4', now(), interval '15 seconds')`)
	r.beat(t)
	if want := []int{5}; !slices.Equal(counted, want) {
		t.Fatalf("a beat beside four beating peers counted %v, want %v: the four and this process's own row", counted, want)
	}
	r.exec(t, `update broker_serving set last_serving_at = now() - interval '1 minute' where process like 'peer-%'`)
	r.beat(t)
	if want := []int{5, 1}; !slices.Equal(counted, want) {
		t.Fatalf("a beat beside four peers a minute past their own %s threshold counted %v, want %v", notServingAfter, counted, want)
	}
}

// A broker beating alone counts one, itself, and says so: holding a lone pod to the rate two
// pods get is api.MinReplicas's work and not the count's, so the count a process hands over is
// the fleet it really sees.
func TestALoneBrokersBeatCountsItself(t *testing.T) {
	r := newServingRig(t)
	var counted []int
	r.clock.OnReplicas = func(replicas int) { counted = append(counted, replicas) }
	r.beat(t)
	r.beat(t)
	if want := []int{1, 1}; !slices.Equal(counted, want) {
		t.Fatalf("a lone broker's two beats counted %v, want %v", counted, want)
	}
}

// A beat held up on its own row (another transaction holds it) fails within about its bound and
// Run logs that it timed out, while another broker beats on: that broker keeps the latest row
// current across more than the threshold, so the guard stays off and a lease that runs out in the
// meantime is lapsed, not held live as if no broker were serving.
func TestAHeldServingLockFailsTheBeatAndAnotherBrokerKeepsTheGuardOff(t *testing.T) {
	r := newServingRig(t)
	ctx := context.Background()
	// Every bound here is the clock's own, and a loaded machine must still meet them: Beat bounds
	// itself, its lock wait and its statement by Interval (ServingClock.Beat), so at 200 ms a
	// machine with more runnable threads than cores times the setup's beats out, and the serving
	// broker's beats with them, which turns the guard on and makes the lapsed lease below read
	// live. Both were seen in 50 runs under eight busy loops on two cores. A second of interval
	// and four of threshold keep every relation this row reads and give that machine room
	// (INFRA-985).
	const interval, threshold = time.Second, 4 * time.Second
	stuck := &ServingClock{Enrollments: r.svc, Interval: interval, NotServingAfter: threshold}
	serving := &ServingClock{Enrollments: r.svc, Interval: interval, NotServingAfter: threshold}
	for _, clock := range []*ServingClock{stuck, serving} {
		if err := clock.Beat(ctx); err != nil {
			t.Fatalf("Beat: %v", err)
		}
	}
	id := r.enroll(t, "box-held")
	logged := captureSlog(t)

	holder, err := r.svc.Store.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Rollback(ctx) })
	if _, err := holder.Exec(ctx, `select from broker_serving where process=$1 for update`, stuck.processID()); err != nil {
		t.Fatal(err)
	}
	stuckLast, _ := r.servingOf(t, stuck)
	start := r.now(t)
	r.setLease(t, id, `now() + interval '100 milliseconds'`)

	began := time.Now()
	err = stuck.Beat(ctx)
	// Beat bounds itself by Interval, so a held row fails at about that; the bound here is a
	// hang guard around it rather than a second measurement of the same clock.
	if took := time.Since(began); err == nil || took >= 4*interval || !beatTimedOut(err) {
		t.Fatalf("Beat on a held row = %v after %s, want a timeout within %s", err, took, 4*interval)
	}

	runs, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, clock := range []*ServingClock{stuck, serving} {
		wg.Go(func() { clock.Run(runs) })
	}
	// The event the assertions below read: the serving broker has beaten past start+threshold, by
	// which time the lease r.setLease shortened above has run out. No fixed sleep bounds that on a
	// loaded machine.
	r.beatsPast(t, serving, start.Add(threshold))
	live := r.live(t, id)
	ended := r.endLapsed(t)
	cancel()
	wg.Wait()

	if live {
		t.Fatal("Lookup answered live a lease that ran out while another broker was serving")
	}
	if !slices.Contains(ended, id) {
		t.Fatalf("EndLapsed ended %v, want it to end %s", ended, id)
	}
	if last, _ := r.servingOf(t, serving); !last.After(start.Add(threshold)) {
		t.Fatalf("the serving broker's last_serving_at = %s, want it past %s: it kept beating across more than the threshold", last, start.Add(threshold))
	}
	if last, _ := r.servingOf(t, stuck); !last.Equal(stuckLast) {
		t.Fatalf("the stuck broker's last_serving_at = %s, want it left at %s while its row is held", last, stuckLast)
	}
	// Only the stuck clock's warnings are this test's subject, and what it claims is that Run
	// says the beat timed out: the stuck row is held for the whole of Run, so every beat of that
	// clock times out and warns, and how many of them a machine fits into the window is the
	// machine's business, not this test's. Holding it to exactly one failed under load, which
	// is a count of beats rather than the behaviour (INFRA-985).
	warnings := []string{}
	for line := range strings.Lines(logged.String()) {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "process="+stuck.processID()) {
			warnings = append(warnings, line)
		}
	}
	if len(warnings) == 0 {
		t.Fatal("Run logged no warning for the stuck broker, want it to say the serving clock could not beat, with timed_out=true")
	}
	for _, warning := range warnings {
		if !strings.Contains(warning, "the serving clock could not beat") || !strings.Contains(warning, "timed_out=true") {
			t.Fatalf("Run logged warning %q for the stuck broker, want every one of them to say the serving clock could not beat, with timed_out=true", warning)
		}
	}
}

// beatsPastCap and beatsPastMargin bound the wait below: the clock under test needs more than its
// threshold of real time to get past want, and a run whose binary deadline leaves less than the
// margin fails by name rather than letting go test's own timeout fire with no test named.
const beatsPastCap = 30 * time.Second
const beatsPastMargin = 10 * time.Second

// beatsPast waits for clock's broker_serving row to pass want.
func (r servingRig) beatsPast(t *testing.T, clock *ServingClock, want time.Time) {
	t.Helper()
	wait := beatsPastCap
	if deadline, bounded := t.Deadline(); bounded {
		left := time.Until(deadline)
		if left <= beatsPastMargin {
			t.Fatalf("the test binary's deadline leaves %s, not more than %s: this looks like a misconfigured run; raise -timeout", left.Round(time.Millisecond), beatsPastMargin)
		}
		wait = min(wait, left-beatsPastMargin)
	}
	for deadline := time.Now().Add(wait); ; time.Sleep(10 * time.Millisecond) {
		if last, _ := r.servingOf(t, clock); last.After(want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the serving broker's last_serving_at did not pass %s within %s", want, wait)
		}
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
		if processes := r.processes(t); slices.Contains(processes, r.clock.processID()) {
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

// beforeTheSharedRule is the gap condition ServingClock.Beat held inline, in SQL of its own,
// before migration 0024 gave the rule one definition: $1 is the beat's instant and latest is the
// latest broker_serving row, left joined so that an empty table is no outage.
const beforeTheSharedRule = `coalesce($1::timestamptz - latest.last_serving_at > latest.not_serving_after, false)`

// beforeTheSharedRuleLiveness is enrollment_lease_live's body as migration 0019 wrote it, with
// now() written as $1 and the lease as $2, so that this instant is a parameter here too.
const beforeTheSharedRuleLiveness = `($2::timestamptz > $1::timestamptz or exists (
		select from (
			select last_serving_at, not_serving_after from broker_serving order by last_serving_at desc limit 1
		) old
		where $1::timestamptz - old.last_serving_at > old.not_serving_after
			and $2::timestamptz > old.last_serving_at))`

// outageRow is one broker_serving row of the matrix below, placed behind the instant the case is
// measured at.
type outageRow struct {
	process   string
	behind    time.Duration // how far behind that instant this process last served
	threshold time.Duration // its not_serving_after
}

// The one rule migration 0024 states, broker_outage_since, answers what the beat's own inline
// condition answered and what enrollment_lease_live answers, over a matrix of rows, thresholds,
// leases and instants: a gap below the threshold, a gap exactly on it, a gap one microsecond past
// it, no row at all, and a lease before, on and after the latest beat. The case's instant is a
// parameter and the rows are placed against it, so the boundary is exact rather than whatever a
// wall clock left between two statements. The whole case runs in one transaction that never
// commits, so now() - the instant enrollment_lease_live measures from - is that same parameter,
// and the rows it inserts are gone when it ends.
//
// This one test does three jobs:
//
//  1. it is the test that fails if the two statements of the rule can disagree (the beat's old
//     condition against the shared function), and it places a gap exactly on the threshold;
//  2. it is the proof that the build an image pin replaces still reads and beats against the
//     schema this migration leaves: enrollment_lease_live at its old one-argument signature and
//     the old inline SQL, both run here over the post-0024 schema, answer what they answered
//     before it. A roll runs pods of both builds, and 0024 touches no table, so nothing a pod of
//     either build writes is at stake;
//  3. it is the target of this change's named revert: >= in place of > inside
//     broker_outage_since, which turns the row whose gap lies exactly on the threshold red.
func TestTheOutageRuleAgreesWithTheBeatsOldCondition(t *testing.T) {
	r := newServingRig(t)
	for _, c := range []struct {
		name       string
		rows       []outageRow
		lease      time.Duration // the lease under test, as an offset from the case's instant
		wantOutage bool
		wantLive   bool
	}{
		{name: "no row at all and a lease that has run out", lease: -time.Second},
		{name: "no row at all and a lease still running", lease: time.Second, wantLive: true},
		{
			name:  "a gap below the threshold",
			rows:  []outageRow{{process: "p1", behind: 10 * time.Second, threshold: notServingAfter}},
			lease: -time.Second,
		},
		{
			name:  "a gap exactly on the threshold",
			rows:  []outageRow{{process: "p1", behind: notServingAfter, threshold: notServingAfter}},
			lease: -time.Second,
		},
		{
			name:       "a gap one microsecond past the threshold",
			rows:       []outageRow{{process: "p1", behind: notServingAfter + time.Microsecond, threshold: notServingAfter}},
			lease:      -time.Second,
			wantOutage: true,
			wantLive:   true,
		},
		{
			name:       "a lease that ran out before the latest beat",
			rows:       []outageRow{{process: "p1", behind: time.Minute, threshold: notServingAfter}},
			lease:      -90 * time.Second,
			wantOutage: true,
		},
		{
			name:       "a lease that ran out exactly at the latest beat",
			rows:       []outageRow{{process: "p1", behind: time.Minute, threshold: notServingAfter}},
			lease:      -time.Minute,
			wantOutage: true,
		},
		{
			name:       "a lease that ran out after the latest beat",
			rows:       []outageRow{{process: "p1", behind: time.Minute, threshold: notServingAfter}},
			lease:      -30 * time.Second,
			wantOutage: true,
			wantLive:   true,
		},
		{
			name:       "a lease still running while no broker serves",
			rows:       []outageRow{{process: "p1", behind: time.Minute, threshold: notServingAfter}},
			lease:      time.Second,
			wantOutage: true,
			wantLive:   true,
		},
		{
			name: "the latest row's own threshold decides, not an older row's",
			rows: []outageRow{
				{process: "older", behind: time.Minute, threshold: notServingAfter},
				{process: "latest", behind: 20 * time.Second, threshold: 30 * time.Second},
			},
			lease: -time.Second,
		},
		{
			name: "an older row within its own generous threshold is not read",
			rows: []outageRow{
				{process: "older", behind: 5 * time.Minute, threshold: time.Hour},
				{process: "latest", behind: 20 * time.Second, threshold: notServingAfter},
			},
			lease:      -10 * time.Second,
			wantOutage: true,
			wantLive:   true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			tx, err := r.svc.Store.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			var at time.Time
			if err := tx.QueryRow(ctx, `select now()`).Scan(&at); err != nil {
				t.Fatal(err)
			}
			for _, row := range c.rows {
				if _, err := tx.Exec(ctx, `insert into broker_serving (process, last_serving_at, not_serving_after)
					values ($1, $2::timestamptz - $3::interval, $4::interval)`, row.process, at, row.behind, row.threshold); err != nil {
					t.Fatalf("insert the broker_serving row of %s: %v", row.process, err)
				}
			}
			lease := at.Add(c.lease)
			var outageSince, latest *time.Time
			var oldOutage, live, oldLive, atIsNow bool
			if err := tx.QueryRow(ctx, `select broker_outage_since($1::timestamptz), `+beforeTheSharedRule+`,
					enrollment_lease_live($2::timestamptz), `+beforeTheSharedRuleLiveness+`,
					latest.last_serving_at, now() = $1::timestamptz
				from (select 1) beat
				left join lateral (
					select last_serving_at, not_serving_after from broker_serving order by last_serving_at desc limit 1
				) latest on true`, at, lease).Scan(&outageSince, &oldOutage, &live, &oldLive, &latest, &atIsNow); err != nil {
				t.Fatal(err)
			}
			if !atIsNow {
				t.Fatalf("the transaction's now() is not %s, the instant its rows are placed against: enrollment_lease_live would measure from another instant", at)
			}
			outage := outageSince != nil
			if outage != oldOutage {
				t.Fatalf("broker_outage_since(%s) is not null = %t, while the beat's own old condition at that instant = %t: the shared rule and the condition it replaced disagree",
					at, outage, oldOutage)
			}
			if outage != c.wantOutage {
				t.Fatalf("broker_outage_since(%s) is not null = %t, want %t", at, outage, c.wantOutage)
			}
			if outage && (latest == nil || !outageSince.Equal(*latest)) {
				t.Fatalf("broker_outage_since(%s) = %s, want the latest row's last_serving_at %v: that is the instant the beat gives the gap back from",
					at, *outageSince, latest)
			}
			if live != oldLive {
				t.Fatalf("enrollment_lease_live(%s) = %t, while enrollment_lease_live as migration 0019 wrote it = %t: a pod of the build before 0024 would read this lease differently",
					lease, live, oldLive)
			}
			if live != c.wantLive {
				t.Fatalf("enrollment_lease_live(%s) = %t, want %t", lease, live, c.wantLive)
			}
		})
	}
}
