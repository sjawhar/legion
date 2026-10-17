package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The outbox runs its rows on lanes. A lane is one issue's rows of one kind of work — its lifecycle
// rows (starts, stops, the branch, the workspace) or its board rows (status writes, messages,
// notices), record.OutboxLane — run one at a time in the order they are due, on a goroutine of its
// own that lives only while the issue has such rows. A lane is woken by the write that commits one
// of its rows (projectRecords, through intake.OnCommit), by the timer it arms for its next row due
// later, and by the sweep, which every outboxLease lists the lanes with a row still in the outbox
// (record.Store.UnfinishedLanes) and wakes each: a row whose run ended without its finish or retry
// recorded — a lost database connection, a daemon that died mid-row — keeps its lease until it
// lapses, and the sweep after the lapse wakes its lane, which lists it due again and runs it. The
// registry lock (outbox.mu) guards only the lane map and the running flag, and is never held across
// a database call or an executor: a wake comes from inside a committed fact's hook, on the intake's
// goroutine, and the API's, and must return at once whatever a lane is doing; a lane blocked on
// GitHub must hold up nothing but its own issue.

// outboxLane is one issue's lane as the registry runs it: the goroutine that drains its due rows
// (runLane) and the one-slot wake that tells it a row of its kinds was written or came due.
type outboxLane struct {
	key  record.LaneKey
	wake chan struct{}
}

// timers is the clock the lanes' timers are armed on: the outbox's, or the wall clock for an outbox
// a test built by hand without one.
func (r *outbox) timers() supervise.Clock {
	if r.clock == nil {
		return supervise.RealClock{}
	}
	return r.clock
}

// nudge wakes the one-slot channel without ever blocking: a wake already waiting is the same wake.
func nudge(wake chan struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

// Run runs the lanes until ctx ends. It opens the registry, sweeps at once — a restart recovers
// every unfinished row, and the rows written while the outbox was not yet attached, without waiting
// for a wake — and then sweeps every outboxLease on the clock. A sweep whose read fails (a Postgres
// restart drops the pool's connections) is logged and tried again at the next tick, so the runner
// outlives the failure instead of ending with it. Once ctx ends it closes the registry, so no wake
// starts a lane, and waits for every lane to return: a row mid-run ends and settles first
// (runRow), so the daemon's stop is a real join.
func (r *outbox) Run(ctx context.Context) {
	if r.log == nil {
		r.log = slog.Default()
	}
	r.mu.Lock()
	r.running, r.laneCtx = true, ctx
	if r.lanes == nil {
		r.lanes = map[record.LaneKey]*outboxLane{}
	}
	r.mu.Unlock()
	clock := r.timers()
	poke := make(chan struct{}, 1)
	for {
		r.sweep(ctx)
		tick := clock.AfterFunc(outboxLease, func() { nudge(poke) })
		select {
		case <-ctx.Done():
			tick.Stop()
			r.mu.Lock()
			r.running = false
			r.mu.Unlock()
			r.wg.Wait()
			return
		case <-poke:
		}
	}
}

// sweep wakes every lane of the project with a row still in the outbox: due, running, backing off
// or held. A lane that has nothing to run goes back to waiting, so the sweep costs an idle lane one
// list; a lane whose rows a lapsed lease held back is the one it is for.
func (r *outbox) sweep(ctx context.Context) {
	var lanes []record.LaneKey
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		lanes, err = r.records.UnfinishedLanes(ctx, tx, r.dispatchProject)
		return err
	}); err != nil {
		if ctx.Err() == nil {
			r.log.Error("outbox sweep failed; the next tick tries again", "error", err)
		}
		return
	}
	for _, key := range lanes {
		r.wakeLane(key)
	}
}

// wake tells the lane that runs kind for issue that a row of it was written or made due. Before
// Run and after its stop there is no lane to tell: the boot sweep finds every row then.
func (r *outbox) wake(issue string, kind record.OutboxKind) {
	r.wakeLane(record.LaneKey{Issue: issue, Lane: record.LaneOf(kind)})
}

// wakeLane wakes the lane key names, starting it if it is not running. The wake is sent under the
// registry lock, where it never blocks (nudge), because a lane decides to exit under the same lock
// after finding no wake pending: a wake sent after that decision would be a wake to a lane that is
// gone, while one sent under the lock either reaches the lane before it decides or finds the lane
// already deleted and starts a new one.
func (r *outbox) wakeLane(key record.LaneKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return
	}
	lane, ok := r.lanes[key]
	if !ok {
		lane = &outboxLane{key: key, wake: make(chan struct{}, 1)}
		r.lanes[key] = lane
		r.wg.Add(1)
		go r.runLane(r.laneCtx, lane)
	}
	nudge(lane.wake)
}

// runLane is one lane's life: drain the rows due now, arm one timer for the row due next, and
// wait for a wake. It exits once nothing is due, nothing is due later and no wake is pending — a
// lane whose only rows another runner holds exits too, and the sweep starts it again once that
// lease lapses. A lane whose list failed, or that left a due row behind on a failed claim, does not
// exit: it waits for a wake or the sweep, which comes within outboxLease.
func (r *outbox) runLane(ctx context.Context, lane *outboxLane) {
	defer r.wg.Done()
	kinds := record.LaneKinds(lane.key.Lane)
	clock := r.timers()
	var next supervise.Cancel
	defer func() {
		if next != nil {
			next.Stop()
		}
	}()
	for {
		// The wake that brought the lane here is spent before the list, not after: a row committed
		// after the list sends another, which the lane sees, while one spent after the list would
		// make a lane whose claim just failed look again at once rather than wait for the sweep.
		select {
		case <-lane.wake:
		default:
		}
		drained, now := r.drain(ctx, lane.key.Issue, kinds)
		if ctx.Err() != nil {
			return
		}
		if next != nil {
			next.Stop()
			next = nil
		}
		later := false
		if drained {
			at, ok, err := r.nextDue(ctx, lane.key.Issue, kinds, now)
			switch {
			case err != nil:
				r.log.Error("outbox lane could not read when its issue is next due; it waits for a wake or the sweep",
					"issue", lane.key.Issue, "lane", lane.key.Lane, "error", err)
				drained = false
			case ok:
				later = true
				next = clock.AfterFunc(at.Sub(now), func() { nudge(lane.wake) })
			}
		}
		if drained && !later {
			r.mu.Lock()
			select {
			case <-lane.wake:
				r.mu.Unlock()
				continue
			default:
			}
			delete(r.lanes, lane.key)
			r.mu.Unlock()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-lane.wake:
		}
	}
}

// drain runs the lane's due rows until a list finds none: a row that ran may have made the next one
// claimable (an issue's second status write, once its first finished), so the list is repeated. It
// reports whether it got that far — every list and claim went through and the last list was empty
// — and the moment of the last list, which the lane's read of its next due row shares so no row
// that comes due between the two is missed by both. A list that fails is logged, and the lane
// waits for a wake or the sweep rather than exiting; so does a lane one of whose claims failed,
// since the row it left is due.
func (r *outbox) drain(ctx context.Context, issue string, kinds []record.OutboxKind) (drained bool, now time.Time) {
	for {
		now = r.now().UTC()
		rows, err := r.listDue(ctx, issue, kinds, now)
		if err != nil {
			if ctx.Err() == nil {
				r.log.Error("outbox lane could not list its due rows; it waits for a wake or the sweep", "issue", issue, "kinds", kinds, "error", err)
			}
			return false, now
		}
		if len(rows) == 0 {
			return true, now
		}
		for _, row := range rows {
			if ctx.Err() != nil {
				return false, now
			}
			if err := r.runRow(ctx, row); err != nil {
				return false, now
			}
		}
	}
}

// listDue is the rows a lane may run at now, in their order, leasing none (record.Store.DueRows);
// issue "" is every issue of the project, kinds nil every kind.
func (r *outbox) listDue(ctx context.Context, issue string, kinds []record.OutboxKind, now time.Time) ([]record.OutboxRow, error) {
	var rows []record.OutboxRow
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		rows, err = r.records.DueRows(ctx, tx, r.dispatchProject, issue, kinds, now)
		return err
	}); err != nil {
		return nil, err
	}
	return rows, nil
}

// nextDue is when the lane's next row comes due after now, if one does (record.Store.NextDue).
func (r *outbox) nextDue(ctx context.Context, issue string, kinds []record.OutboxKind, now time.Time) (time.Time, bool, error) {
	var at time.Time
	var ok bool
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		at, ok, err = r.records.NextDue(ctx, tx, issue, kinds, now)
		return err
	}); err != nil {
		return time.Time{}, false, err
	}
	return at, ok, nil
}

// runRow claims the listed row and, if the claim is its, runs it to its settle: the effect runs
// outside every transaction with the lease renewed under it, and the finish or retry fences on the
// lease token. It reports a claim that failed. A claim that returns nothing — another runner's
// lease, a row finished since the list, or one an older row of its issue since queued ahead of —
// is no one's failure and is passed over silently. A claimed row whose kind spends a GitHub budget
// a limit still stands on (deferral) runs nothing and is retried at that limit's end, so it never
// meets the limit again. The settle runs even once ctx has ended: the effect ran, so what it came
// to is recorded on a bounded context of its own (outboxSettleTimeout) rather than left to run
// again at the next boot.
func (r *outbox) runRow(ctx context.Context, listed record.OutboxRow) error {
	now := r.now().UTC()
	var row record.OutboxRow
	var claimed bool
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		row, claimed, err = r.records.ClaimRow(ctx, tx, listed.ID, now, outboxLease)
		return err
	}); err != nil {
		if ctx.Err() == nil {
			r.log.Error("outbox row claim failed", "row", listed.ID, "kind", listed.Kind, "issue", listed.Issue, "error", err)
		}
		return err
	}
	if !claimed {
		return nil
	}
	if limit, ok := r.deferral(row.Kind, r.now().UTC()); ok {
		cause := fmt.Errorf("deferred until %s behind row %d's GitHub rate limit: %w", limit.until.Format(time.RFC3339), limit.row, limit.err)
		if err := r.retryAt(ctx, row, limit.until, cause); err != nil {
			r.log.Error("outbox row deferral not recorded; it runs again when its lease expires", "row", row.ID, "error", err)
		}
		return nil
	}
	renewal := r.renewLease(ctx, row)
	err := r.execute(ctx, row)
	renewal.stop()
	settleCtx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		settleCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), outboxSettleTimeout)
		defer cancel()
	}
	r.settle(settleCtx, row, err)
	return nil
}

// leaseRenewal keeps a running row's lease live (renewLease): the tick armed next, whether the run
// has ended, and what the ticks have already said, so each is said once per run.
type leaseRenewal struct {
	mu      sync.Mutex
	stopped bool
	tick    supervise.Cancel
	lost    bool
	warned  map[int64]bool
}

// arm schedules the next tick unless the run has ended, so a tick in flight when stop is called
// re-arms nothing.
func (l *leaseRenewal) arm(schedule func() supervise.Cancel) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped {
		return
	}
	l.tick = schedule()
}

// stop ends the chain: the tick armed is cancelled, and one running re-arms nothing.
func (l *leaseRenewal) stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stopped = true
	if l.tick != nil {
		l.tick.Stop()
	}
}

func (l *leaseRenewal) ended() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stopped
}

// lostOnce marks the lease lost and reports whether this is the first tick to find it so.
func (l *leaseRenewal) lostOnce() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	first := !l.lost
	l.lost = true
	return first
}

// warnOnce marks the held row warned about and reports whether this is the first tick to do so.
func (l *leaseRenewal) warnOnce(held int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.warned[held] {
		return false
	}
	l.warned[held] = true
	return true
}

// renewLease starts the chain of ticks that keeps the row's lease live while it runs: every third of
// the lease each tick moves the lease a whole lease ahead, so a run of any length lapses only when
// the database could not be reached for the whole lease. A renewal that fails is logged and tried
// at the next tick; a lease found no longer the row's is logged once, and the run goes on to its
// end, since nothing cancels a row mid-effect — the finish or retry then fences on the token and
// records nothing. A lifecycle row's tick also reads the rows its issue holds behind it, and logs
// each once it has waited heldWarnAfter: the lane runs one row at a time, so a row that runs long
// is what an operator looks at when a stop or a start of the issue does not happen.
func (r *outbox) renewLease(ctx context.Context, row record.OutboxRow) *leaseRenewal {
	renewal := &leaseRenewal{warned: map[int64]bool{}}
	clock := r.timers()
	lifecycle := record.LaneOf(row.Kind) == record.LaneLifecycle
	var tick func()
	schedule := func() supervise.Cancel { return clock.AfterFunc(outboxLease/3, tick) }
	tick = func() {
		if renewal.ended() {
			return
		}
		now := r.now().UTC()
		r.renewOnce(ctx, row, renewal, now)
		if lifecycle {
			r.warnHeld(ctx, row, renewal, now)
		}
		renewal.arm(schedule)
	}
	renewal.arm(schedule)
	return renewal
}

// renewOnce is one tick's renewal of the row's lease. A tick that was in flight when the run ended
// may find the row gone because its own settle finished it; that is no lost lease, so a renewal that
// found nothing is reported only while the run is still on.
func (r *outbox) renewOnce(ctx context.Context, row record.OutboxRow, renewal *leaseRenewal, now time.Time) {
	var held bool
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		held, err = r.records.RenewOutboxLease(ctx, tx, row.ID, row.LeaseToken, now.Add(outboxLease))
		return err
	}); err != nil {
		if ctx.Err() == nil {
			r.log.Warn("outbox row lease not renewed; the next tick tries again", "row", row.ID, "kind", row.Kind, "issue", row.Issue, "error", err)
		}
		return
	}
	if !held && !renewal.ended() && renewal.lostOnce() {
		r.log.Warn("outbox row lease lost; the row runs on to its end and its finish or retry fences on the token",
			"row", row.ID, "kind", row.Kind, "issue", row.Issue)
	}
}

// warnHeld is one tick's look at the lifecycle rows of the row's issue held behind it
// (record.Store.HeldRows): each that has waited heldWarnAfter is logged, once per run of this row.
func (r *outbox) warnHeld(ctx context.Context, row record.OutboxRow, renewal *leaseRenewal, now time.Time) {
	var held []record.HeldRow
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		held, err = r.records.HeldRows(ctx, tx, r.dispatchProject, row.Issue, now)
		return err
	}); err != nil {
		if ctx.Err() == nil {
			r.log.Warn("outbox held rows not read; the next tick tries again", "row", row.ID, "issue", row.Issue, "error", err)
		}
		return
	}
	for _, waiting := range held {
		if waiting.WaitsOn != row.ID {
			continue
		}
		waited := now.Sub(waiting.NextAt)
		if waited < heldWarnAfter || renewal.ended() || !renewal.warnOnce(waiting.ID) {
			continue
		}
		r.log.Warn("outbox row held behind its issue's running lifecycle row",
			"row", waiting.ID, "kind", waiting.Kind, "issue", waiting.Issue, "waitsOn", row.ID, "waited", waited)
	}
}
