package api

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The cadence the relay and the delivery hub share (eventGapReread), step by step: a gap only the
// read's own observation holds is re-read at once; a held one waits for the next notification,
// read without the coalescing window, or the fallback, which doubles while the horizon stands
// still and drops the eager read until it moves; an immediate re-read is never asked twice
// without the horizon moving between; and no gap schedules nothing.
func TestEventGapRereadCadence(t *testing.T) {
	var gaps eventGapReread
	defer gaps.stop()
	for step, want := range []struct {
		safe     int64
		gap      events.Gap
		now      bool
		fallback time.Duration
		eager    bool
	}{
		{safe: 5, gap: events.GapUnproven, now: true, eager: true},
		{safe: 5, gap: events.GapHeld, fallback: eventRelayGapMin, eager: true},
		{safe: 5, gap: events.GapHeld, fallback: 2 * eventRelayGapMin},
		{safe: 5, gap: events.GapUnproven, fallback: 4 * eventRelayGapMin},
		{safe: 8, gap: events.GapHeld, fallback: eventRelayGapMin, eager: true},
		{safe: 9, gap: events.GapUnproven, now: true, eager: true},
		{safe: 9, gap: events.GapNone},
	} {
		gaps.schedule(want.safe, want.gap)
		due := gaps.due()
		var fallback time.Duration
		if gaps.timer != nil {
			fallback = gaps.wait
		}
		now := due != nil && due == gapRereadNow
		if now != want.now || fallback != want.fallback || gaps.eager != want.eager || (want.gap == events.GapNone && due != nil) {
			t.Fatalf("step %d (safe %d, gap %d): now %v, fallback %s, eager %v; want %v, %s, %v",
				step+1, want.safe, want.gap, now, fallback, gaps.eager, want.now, want.fallback, want.eager)
		}
	}
}

// cadenceXid is the base of the transaction ids a cadenceHorizonDB script hands out: far above
// any the test Postgres has assigned, so a scripted snapshot proves every real observation before
// it and no real snapshot proves a scripted one.
const cadenceXid = uint64(1) << 40

// A relay with nothing to publish observes nothing; an event's notification wakes one read, whose
// observation sees the event but does not prove itself, and the relay observes again at once
// rather than after a delay: with no older transaction id running, that second observation proves
// the first and the event is published. The script's snapshots never prove themselves (cadenceRow),
// so this holds the relay to the cadence whatever the test Postgres was doing. The fallback timer
// is an hour here, so only the immediate second observation can publish it, and the relay is idle
// again once it has.
func TestEventStreamRelayProvesItsOwnObservationAtOnce(t *testing.T) {
	_, database, deps := newTestServer(t, testServerOptions{})
	relay, horizon := newCadenceRelay(t, deps, database, time.Millisecond)
	subscription, cancel, _, _, err := relay.subscribe(context.Background())
	if err != nil {
		t.Fatalf("subscribe to the relay: %v", err)
	}
	defer cancel()
	horizon.settle(t)
	if observed := horizon.observedDuring(600 * time.Millisecond); observed != 0 {
		t.Fatalf("an idle relay took %d observations in 600 ms, want none", observed)
	}

	horizon.arm(eventHead(t, database)+1,
		cadenceObservation{xmin: cadenceXid, xid: cadenceXid + 1},     // sees the event: unproven
		cadenceObservation{xmin: cadenceXid + 2, xid: cadenceXid + 2}, // nothing older runs: proves it
	)
	id := commitCadenceEvent(t, database)
	requireRelayPublishes(t, subscription, 5*time.Second, id)
	if left := horizon.left(); left != 0 {
		t.Fatalf("the relay published after %d of its 2 scripted observations", 2-left)
	}
	if observed := horizon.observedDuring(600 * time.Millisecond); observed != 0 {
		t.Fatalf("the relay took %d observations in 600 ms after it published, want none", observed)
	}
}

// A gap an older transaction holds open is closed by that transaction's commit notification: the
// read it wakes skips the coalescing window, since the events it publishes have waited already.
// Here a write holds the lower id open while the higher one commits; the relay's read of the
// higher one, and the immediate observation after it, find the write still running (their
// snapshots' xmin is the write's id), so the gap is held. The write's commit notifies, and the
// read it wakes proves both and publishes the two events in id order. The coalescing window is
// two seconds and the fallback timer an hour, so only a read the notification woke at once can
// publish them within the second the test allows. The scripted proof applies only to an
// observation taken once the lower id is committed, so the script never claims the write ended
// before it did.
func TestEventStreamRelayClosesAHeldGapOnTheHoldersCommitNotification(t *testing.T) {
	const coalesce = 2 * time.Second
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createInteractionIssue(t, handler, "TEST", "Issue", "A spec")
	other := createInteractionIssue(t, handler, "OTHER", "Other issue", "A spec")
	waitForQuietEventLog(t, database, 500*time.Millisecond)
	relay, horizon := newCadenceRelay(t, deps, database, coalesce)
	subscription, cancel, _, _, err := relay.subscribe(context.Background())
	if err != nil {
		t.Fatalf("subscribe to the relay: %v", err)
	}
	defer cancel()
	// The listener's first LISTEN wakes one catch-up read, a coalescing window later; once it has
	// run no wake is pending, so the next read is the one the higher id's commit wakes.
	time.Sleep(coalesce + 500*time.Millisecond)
	horizon.settle(t)

	var low atomic.Int64
	committed := func() bool {
		var visible bool
		id := low.Load()
		return id != 0 && database.Pool.QueryRow(context.Background(), `select exists(select 1 from events where id = $1)`, id).Scan(&visible) == nil && visible
	}
	horizon.arm(eventHead(t, database)+1,
		cadenceObservation{xmin: cadenceXid, xid: cadenceXid + 1},                      // the write (cadenceXid) runs: unproven
		cadenceObservation{xmin: cadenceXid, xid: cadenceXid + 2},                      // it still runs: held
		cadenceObservation{xmin: cadenceXid + 3, xid: cadenceXid + 3, when: committed}, // it has committed: proves both
	)
	lowID, high, commitLow := appendOutOfOrder(t, database, events.NewBroker(), issue.Key, other.Key)
	low.Store(lowID)
	horizon.awaitTaken(t, 2, coalesce+10*time.Second)
	select {
	case event := <-subscription:
		t.Fatalf("the relay published event %d while the write holding %d is open", event.ID, lowID)
	case <-time.After(200 * time.Millisecond):
	}

	commitLow()
	requireRelayPublishes(t, subscription, time.Second, lowID, high)
	if left := horizon.left(); left != 0 {
		t.Fatalf("the relay published after %d of its 3 scripted observations", 3-left)
	}
}

// waitForQuietEventLog waits until the event log's head has stood still for quiet, so no write
// the test's setup made is still to notify.
func waitForQuietEventLog(t *testing.T, database *store.Store, quiet time.Duration) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	head, since := eventHead(t, database), time.Now()
	for time.Since(since) < quiet {
		if time.Now().After(deadline) {
			t.Fatal("the event log was still growing after 10 s")
		}
		time.Sleep(20 * time.Millisecond)
		if now := eventHead(t, database); now != head {
			head, since = now, time.Now()
		}
	}
}

// newCadenceRelay is a relay on a server of its own whose horizon observes through a
// cadenceHorizonDB, with coalesce as its coalescing window and an hour for its fallback timer.
func newCadenceRelay(t *testing.T, deps Deps, database *store.Store, coalesce time.Duration) (*eventStreamRelay, *cadenceHorizonDB) {
	t.Helper()
	horizon := &cadenceHorizonDB{HorizonDB: database.Pool, taken: make(chan int, 16)}
	served := directServer(deps)
	served.horizonOnce.Do(func() { served.eventHorizon = events.NewHorizon(horizon) })
	lifetime, stop := context.WithCancel(t.Context())
	relay := newEventStreamRelay(served, lifetime)
	relay.coalesce, relay.gapFallback = coalesce, time.Hour
	t.Cleanup(func() {
		stop()
		relay.wait()
	})
	return relay, horizon
}

// cadenceHorizonDB observes through the real pool, so the relay starts and reads as it does in
// production, and counts every observation. Once armed, each observation that sees an id at or
// above from has its snapshot and transaction id replaced by the next scripted observation, so
// the test decides which observation proves which, whatever else holds a transaction id on the
// shared test Postgres; taken receives each scripted observation's position, from 1, as it is
// taken.
type cadenceHorizonDB struct {
	events.HorizonDB
	observations atomic.Int64
	taken        chan int

	mu       sync.Mutex
	from     int64
	script   []cadenceObservation
	position int
}

// cadenceObservation is one scripted snapshot xmin and transaction id. when, if set, holds it back
// until it answers true: an observation taken before then keeps its real values.
type cadenceObservation struct {
	xmin, xid uint64
	when      func() bool
}

func (db *cadenceHorizonDB) arm(from int64, script ...cadenceObservation) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.from, db.script = from, script
}

// left is how many scripted observations are still to be taken.
func (db *cadenceHorizonDB) left() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return len(db.script)
}

// settle waits until the relay has taken no observation for 300 ms: its start and the catch-up
// its listener's connection runs are over.
func (db *cadenceHorizonDB) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for db.observedDuring(300*time.Millisecond) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the relay kept observing for 10 s after it started")
		}
	}
}

// observedDuring is how many observations were taken during the next d.
func (db *cadenceHorizonDB) observedDuring(d time.Duration) int64 {
	before := db.observations.Load()
	time.Sleep(d)
	return db.observations.Load() - before
}

// awaitTaken waits up to within for the scripted observation at position to be taken.
func (db *cadenceHorizonDB) awaitTaken(t *testing.T, position int, within time.Duration) {
	t.Helper()
	timeout := time.After(within)
	for {
		select {
		case taken := <-db.taken:
			if taken >= position {
				return
			}
		case <-timeout:
			t.Fatalf("scripted observation %d not taken within %s", position, within)
		}
	}
}

func (db *cadenceHorizonDB) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	db.observations.Add(1)
	return cadenceResults{BatchResults: db.HorizonDB.SendBatch(ctx, batch), db: db}
}

// cadenceResults is an observation's results with its one row rewritten (cadenceRow).
type cadenceResults struct {
	pgx.BatchResults
	db *cadenceHorizonDB
}

func (r cadenceResults) QueryRow() pgx.Row {
	return cadenceRow{Row: r.BatchResults.QueryRow(), db: r.db}
}

// cadenceRow scans an observation's A and its created_at, its snapshot's xmin and xmax, whether
// that snapshot's in-progress list is empty and X, and rewrites the snapshot and X with the next
// scripted observation when the script is armed and A is at or above its from. A scripted
// observation never proves itself: its snapshot keeps an id in progress and its xmax is below X,
// so the script alone decides which observation proves which, whatever the test Postgres is doing.
type cadenceRow struct {
	pgx.Row
	db *cadenceHorizonDB
}

func (row cadenceRow) Scan(dest ...any) error {
	if err := row.Row.Scan(dest...); err != nil {
		return err
	}
	db := row.db
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.from == 0 || len(db.script) == 0 || *dest[0].(*int64) < db.from {
		return nil
	}
	next := db.script[0]
	if next.when != nil && !next.when() {
		return nil
	}
	db.script = db.script[1:]
	db.position++
	*dest[2].(*string) = strconv.FormatUint(next.xmin, 10)
	*dest[3].(*string) = strconv.FormatUint(next.xid-1, 10)
	*dest[4].(*bool) = false
	*dest[5].(*string) = strconv.FormatUint(next.xid, 10)
	db.taken <- db.position
	return nil
}

// commitCadenceEvent commits one ownerless event in a transaction of its own, taking its
// transaction id first as Broker.Append does, and returns its id. Its insert notifies the relay.
func commitCadenceEvent(t *testing.T, database *store.Store) int64 {
	t.Helper()
	ctx := t.Context()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the event's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var id int64
	if _, err := tx.Exec(ctx, `select pg_current_xact_id()`); err != nil {
		t.Fatalf("take the event's transaction id: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		insert into events (seq, type, actor, payload, notify)
		values (0, 'relay.cadence', '{"kind":"user","id":"alice"}', '{}', false)
		returning id
	`).Scan(&id); err != nil {
		t.Fatalf("insert the event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the event: %v", err)
	}
	return id
}

// requireRelayPublishes requires the relay to publish exactly ids, in order, each within within
// of the call.
func requireRelayPublishes(t *testing.T, subscription <-chan model.Event, within time.Duration, ids ...int64) {
	t.Helper()
	timeout := time.After(within)
	for _, id := range ids {
		select {
		case event, open := <-subscription:
			if !open {
				t.Fatal("the relay's subscription ended")
			}
			if event.ID != id {
				t.Fatalf("the relay published event %d, want %d (of %v)", event.ID, id, ids)
			}
		case <-timeout:
			t.Fatalf("the relay did not publish event %d (of %v) within %s", id, ids, within)
		}
	}
}
