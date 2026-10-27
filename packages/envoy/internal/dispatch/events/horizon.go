// # Safe horizon
//
// Event ids come from one sequence, events_id_seq, and a transaction draws its id when it inserts
// its row, which becomes visible only when the transaction commits. Two writes that do not wait
// for each other can therefore commit out of id order: the write that drew id 7 can still be open
// after the write that drew id 8 has committed. A reader that pages the log by a global id cursor
// (id > cursor), read 8 and moved its cursor there would never read 7. Such a reader reads only
// up to the safe horizon H, the highest id at or below which every event is final: committed and
// visible to every later statement, or rolled back for good. A Horizon computes H from Postgres's
// own transaction bookkeeping.
//
// An observation is one statement in a transaction of its own, sent as one batch:
//
//	select coalesce(max(id), 0), pg_current_snapshot()::text, pg_current_xact_id()::text from events
//
// It yields A, the newest id visible to the statement's snapshot S, and X, the observer's own
// transaction id, which pg_current_xact_id assigns while the statement runs, after S was taken.
//
// A is final once no transaction id below X is still running. Take any transaction T that owns an
// id g <= A and had not committed when S was taken. T held its transaction id before nextval handed
// it g: that is the writer invariant, which Broker.Append must keep by taking its transaction id
// before it inserts. nextval handed out g before it handed out A, because events_id_seq has
// CACHE 1, so every backend draws from the one shared counter in time order. A's row had committed
// before S was taken, and X was assigned after S. So xid(T) < X. Postgres assigns transaction ids
// in increasing order, so once no transaction id below X is running, T has ended: its row is
// visible to every later statement, or it rolled back and g is a gap for good.
//
// The proof that no id below X is running comes from a later observation. Its snapshot S' has
// pg_snapshot_xmin(S') > X exactly when every transaction id below X had ended when S' was taken:
// a snapshot's xmin is the oldest transaction id still running, and no id below it is running. H is
// the A of the newest observation a later one proved safe this way, and it only grows until Reset.
// The check holds whatever order observations ran in: a snapshot taken while the earlier observer
// was still open has an xmin at or below that observer's X, and proves nothing.
//
// One snapshot never proves its own A, even with an empty in-progress list. A snapshot's xmax is
// one past the newest transaction id that had completed, and a transaction holding an id at or
// above xmax is not listed in its xip at all (Postgres documents that "there might be in-progress
// IDs higher than xmax"). Say T2 takes transaction id 100, T takes 101 and inserts event 7, then T2
// inserts event 8 and commits. A snapshot taken now has xmin = xmax = 101, lists nothing in
// progress and sees 8, while T is still open holding 7. The observer's own X, assigned after the
// snapshot, is what closes that hole: it is above 101, so no later snapshot proves the observation
// safe until T ends.
//
// # Costs
//
// Snapshots are instance-wide. Any transaction that holds a transaction id, in any database on the
// Postgres instance (Dispatch's own writes, another service's on the same Cloud SQL instance, an
// operator's open psql session, a prepared transaction), holds H back until it ends, so H trails
// the longest write transaction open on the instance. That delays live readers only; it blocks no
// writer. A pending observation older than five seconds logs a warning naming the oldest such
// transaction, so a stall can be found.
//
// Each observation consumes one transaction id, which brings anti-wraparound vacuum sooner. A
// Horizon observes only when a caller asks it to (Safe, Await) and runs no timer of its own, so a
// process whose readers are idle consumes none, and concurrent callers share one observation.
//
// # Primary only
//
// Observations, and every read capped at H, run on the primary. A hot standby refuses
// pg_current_xact_id, and a standby that has not yet replayed a committed row would serve a read
// at or below H without it.
package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// horizonPendingLimit bounds the observations waiting for a later one to prove them safe.
	// Any later observation can prove any earlier one, so when the list is full the newest
	// observation takes the last slot: a long stall costs no memory, only the latency of proving
	// the newest observation once the stall ends.
	horizonPendingLimit = 64
	// Await's re-observation backoff: 10 ms doubling to 250 ms.
	horizonBackoffMin = 10 * time.Millisecond
	horizonBackoffMax = 250 * time.Millisecond
	// A pending observation older than horizonStallAfter logs a warning, at most once every
	// horizonWarnEvery, so a transaction held open for minutes names itself without flooding
	// the log.
	horizonStallAfter = 5 * time.Second
	horizonWarnEvery  = 30 * time.Second
	// horizonDiagnosisTimeout bounds the stall warning's query of pg_stat_activity, which runs
	// after the observation and must never hold up a reader for long.
	horizonDiagnosisTimeout = 2 * time.Second
)

// horizonStallHolderSQL names the oldest open transaction holding a transaction id, a backend's
// or a prepared transaction's: the oldest running id is the one every snapshot's xmin waits on,
// so it is what keeps a pending observation from being proven safe.
const horizonStallHolderSQL = `
	select kind, pid, started, state, application_name, database, query from (
		select 'backend' as kind, pid, xact_start as started, coalesce(state, '') as state,
			coalesce(application_name, '') as application_name, coalesce(datname::text, '') as database,
			left(coalesce(query, ''), 90) as query, age(backend_xid) as age
		from pg_stat_activity
		where backend_xid is not null
		union all
		select 'prepared', 0, prepared, 'prepared', '', database::text,
			left('prepared transaction ' || gid || ' owned by ' || owner::text, 90), age(transaction)
		from pg_prepared_xacts
	) holder
	order by age desc
	limit 1
`

// HorizonDB is what a Horizon needs from the store: the shared pool satisfies it.
type HorizonDB interface {
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row // stall diagnosis only
}

// Horizon tracks the safe horizon of the event log (see the package documentation's "Safe
// horizon" section): one per process, shared by every reader that pages the log by a global id
// cursor. It is safe for concurrent use.
type Horizon struct {
	db HorizonDB

	// now, stallAfter, warnEvery and logger are the stall warning's clock, thresholds and sink;
	// NewHorizon sets the production values and a test replaces them. A nil logger is
	// slog.Default at the time of the warning.
	now        func() time.Time
	stallAfter time.Duration
	warnEvery  time.Duration
	logger     *slog.Logger

	mu sync.Mutex
	// generation counts Resets. An observation records only into the generation it started in,
	// so one in flight across a Reset cannot bring back what the Reset forgot.
	generation uint64
	// started counts observations ever started and is never reset: an observation's number is
	// its order, and Await waits for one numbered above the count when it was called.
	started  uint64
	inFlight *horizonFlight
	// safe is H. safeThrough is the number of the newest observation H covers: proven safe, or
	// with an A at or below H when recorded or later. Both are this generation's.
	safe        int64
	safeThrough uint64
	// newest is the highest A observed this generation (Pending).
	newest int64
	// pending holds the observations still waiting for a later one to prove them safe, oldest
	// first, each with an A above safe.
	pending []horizonObservation
	// warnedAt is when the last stall warning was logged.
	warnedAt time.Time
}

// horizonObservation is one observation: its number, A, X and when it was recorded.
type horizonObservation struct {
	number uint64
	newest int64
	xid    uint64
	at     time.Time
}

// horizonFlight is the one observation in flight, which concurrent callers share. Its fields
// are written by the caller running it before done closes and read by the others after.
type horizonFlight struct {
	number     uint64
	generation uint64
	done       chan struct{}
	err        error
	// abandoned is an observation that failed because its runner's context ended; a waiter
	// whose own context is still live runs another rather than return a cancellation not its own.
	abandoned bool
}

// horizonSnapshot is a pg_snapshot's text form, xmin:xmax:xip1,xip2,..., as xid8 values.
type horizonSnapshot struct {
	xmin, xmax uint64
	xip        []uint64
}

// NewHorizon returns a Horizon observing the event log through db, with H at 0.
func NewHorizon(db HorizonDB) *Horizon {
	return &Horizon{
		db:         db,
		now:        time.Now,
		stallAfter: horizonStallAfter,
		warnEvery:  horizonWarnEvery,
	}
}

// Safe runs one observation (shared with concurrent callers), checks the pending observations
// against it and returns H, the highest id at or below which every event is final (0 before any
// observation has been proven safe).
func (h *Horizon) Safe(ctx context.Context) (int64, error) {
	if err := h.observe(ctx, 0); err != nil {
		return 0, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.safe, nil
}

// Await returns H once an observation taken during this call is proven safe, so H is at or
// above every id committed before the call; it re-observes with the 10 ms -> 250 ms backoff and
// returns early only with ctx's error.
//
// The observation has to start after the call: one already in flight took its snapshot before
// the call and can miss an id committed just before it, so Await waits that one out rather than
// share it. An observation error is retried like an unproven observation, and reported with
// ctx's error if ctx ends first.
func (h *Horizon) Await(ctx context.Context) (int64, error) {
	h.mu.Lock()
	after := h.started
	h.mu.Unlock()
	wait := horizonBackoffMin
	var failed error
	for {
		err := h.observe(ctx, after)
		if err == nil {
			h.mu.Lock()
			proven, safe := h.safeThrough > after, h.safe
			h.mu.Unlock()
			if proven {
				return safe, nil
			}
		} else if ctx.Err() == nil {
			failed = err
		}
		if ctx.Err() == nil {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
			wait = min(2*wait, horizonBackoffMax)
		}
		if ctx.Err() != nil {
			if failed != nil {
				return 0, fmt.Errorf("await event horizon: %w (last observation: %v)", ctx.Err(), failed)
			}
			return 0, fmt.Errorf("await event horizon: %w", ctx.Err())
		}
	}
}

// Pending reports whether an observed id stands above H (a gap a reader must re-read for).
func (h *Horizon) Pending() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.newest > h.safe
}

// Reset forgets every observation and H; called when a reader detects a reset event log.
func (h *Horizon) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.generation++
	h.safe, h.safeThrough, h.newest = 0, 0, 0
	h.pending = nil
}

// observe returns once an observation numbered above after has ended, recorded or failed: it
// joins the one in flight, or runs one itself when none is. An observation numbered at or below
// after started before the caller needed it, so the caller waits it out and takes the next.
func (h *Horizon) observe(ctx context.Context, after uint64) error {
	for {
		h.mu.Lock()
		flight := h.inFlight
		run := flight == nil
		if run {
			h.started++
			flight = &horizonFlight{number: h.started, generation: h.generation, done: make(chan struct{})}
			h.inFlight = flight
		}
		h.mu.Unlock()
		if run {
			h.run(ctx, flight)
		} else {
			select {
			case <-flight.done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if flight.number <= after || (flight.abandoned && ctx.Err() == nil) {
			continue
		}
		return flight.err
	}
}

// run takes flight's observation and records it, unless a Reset came between. A stall warning it
// finds due runs after the waiters are released.
func (h *Horizon) run(ctx context.Context, flight *horizonFlight) {
	observation, snapshot, err := h.take(ctx)
	h.mu.Lock()
	h.inFlight = nil
	flight.err = err
	flight.abandoned = err != nil && ctx.Err() != nil
	var stalled *horizonObservation
	var safe int64
	if err == nil && flight.generation == h.generation {
		observation.number, observation.at = flight.number, h.now()
		h.record(observation, snapshot.xmin)
		stalled, safe = h.stalled(observation.at), h.safe
	}
	h.mu.Unlock()
	close(flight.done)
	if stalled != nil {
		h.warnStall(ctx, *stalled, safe)
	}
}

// take runs one observation: A, the snapshot S its statement read and X, the observer's own
// transaction id. synchronous_commit is off for it alone, since the observer writes nothing a
// crash could lose, and its commit need not wait for a WAL flush to end its transaction id.
func (h *Horizon) take(ctx context.Context) (horizonObservation, horizonSnapshot, error) {
	batch := &pgx.Batch{}
	batch.Queue(`begin`)
	batch.Queue(`set local synchronous_commit = off`)
	batch.Queue(`select coalesce(max(id), 0), pg_current_snapshot()::text, pg_current_xact_id()::text from events`)
	batch.Queue(`commit`)
	results := h.db.SendBatch(ctx, batch)
	var newest int64
	var snapshotText, xidText string
	_, err := results.Exec()
	if err == nil {
		_, err = results.Exec()
	}
	if err == nil {
		err = results.QueryRow().Scan(&newest, &snapshotText, &xidText)
	}
	if err == nil {
		_, err = results.Exec()
	}
	if closeErr := results.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return horizonObservation{}, horizonSnapshot{}, fmt.Errorf("observe event horizon: %w", err)
	}
	snapshot, err := parseHorizonSnapshot(snapshotText)
	if err != nil {
		return horizonObservation{}, horizonSnapshot{}, fmt.Errorf("observe event horizon: %w", err)
	}
	xid, err := strconv.ParseUint(xidText, 10, 64)
	if err != nil {
		return horizonObservation{}, horizonSnapshot{}, fmt.Errorf("observe event horizon: parse transaction id %q: %w", xidText, err)
	}
	return horizonObservation{newest: newest, xid: xid}, snapshot, nil
}

// record checks every pending observation against the new one's snapshot xmin, raises H to the
// A of each it proves safe, and keeps the new observation if its A stands above H. h.mu is held.
func (h *Horizon) record(observation horizonObservation, xmin uint64) {
	for _, pending := range h.pending {
		if xmin > pending.xid && pending.newest > h.safe {
			h.safe = pending.newest
		}
	}
	kept := h.pending[:0]
	for _, pending := range h.pending {
		if xmin > pending.xid || pending.newest <= h.safe {
			h.safeThrough = max(h.safeThrough, pending.number)
			continue
		}
		kept = append(kept, pending)
	}
	h.pending = kept
	h.newest = max(h.newest, observation.newest)
	if observation.newest <= h.safe {
		h.safeThrough = max(h.safeThrough, observation.number)
		return
	}
	if len(h.pending) == horizonPendingLimit {
		h.pending[len(h.pending)-1] = observation
		return
	}
	h.pending = append(h.pending, observation)
}

// stalled returns the oldest pending observation when it has waited stallAfter and no warning
// has been logged within warnEvery, and marks the warning logged. h.mu is held.
func (h *Horizon) stalled(now time.Time) *horizonObservation {
	if len(h.pending) == 0 || now.Sub(h.pending[0].at) < h.stallAfter {
		return nil
	}
	if !h.warnedAt.IsZero() && now.Sub(h.warnedAt) < h.warnEvery {
		return nil
	}
	h.warnedAt = now
	oldest := h.pending[0]
	return &oldest
}

// warnStall logs that oldest has waited too long to be proven safe, naming the oldest open
// transaction that holds a transaction id. The diagnosis runs past the caller's cancellation
// under its own short deadline, and a failed one is logged in the warning, never returned.
func (h *Horizon) warnStall(ctx context.Context, oldest horizonObservation, safe int64) {
	attrs := []any{
		"pending_for", h.now().Sub(oldest.at), "observed_id", oldest.newest, "safe_horizon", safe,
		"observer_xid", oldest.xid,
	}
	diagnosis, cancel := context.WithTimeout(context.WithoutCancel(ctx), horizonDiagnosisTimeout)
	defer cancel()
	var kind, state, application, database, query string
	var pid int32
	var started *time.Time
	err := h.db.QueryRow(diagnosis, horizonStallHolderSQL).
		Scan(&kind, &pid, &started, &state, &application, &database, &query)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		attrs = append(attrs, "holder", "none open now")
	case err != nil:
		attrs = append(attrs, "diagnosis_error", err)
	default:
		attrs = append(attrs, "holder", kind, "pid", pid)
		if started != nil {
			attrs = append(attrs, "xact_start", *started)
		}
		attrs = append(attrs, "state", state, "application_name", application, "database", database, "query", query)
	}
	logger := h.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("dispatch: event horizon held back by an open transaction; live event readers wait for it", attrs...)
}

// parseHorizonSnapshot parses a pg_snapshot's text form, xmin:xmax:xip1,xip2,... with an empty
// xip list when nothing is in progress. It refuses anything Postgres would not print: a missing
// field, a value that is not an unsigned integer, xmin above xmax, or an xip value outside
// [xmin, xmax) or out of ascending order.
func parseHorizonSnapshot(text string) (horizonSnapshot, error) {
	fields := strings.Split(text, ":")
	if len(fields) != 3 {
		return horizonSnapshot{}, fmt.Errorf("parse snapshot %q: want xmin:xmax:xip", text)
	}
	xmin, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return horizonSnapshot{}, fmt.Errorf("parse snapshot %q: xmin: %w", text, err)
	}
	xmax, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return horizonSnapshot{}, fmt.Errorf("parse snapshot %q: xmax: %w", text, err)
	}
	if xmin > xmax {
		return horizonSnapshot{}, fmt.Errorf("parse snapshot %q: xmin above xmax", text)
	}
	snapshot := horizonSnapshot{xmin: xmin, xmax: xmax}
	if fields[2] == "" {
		return snapshot, nil
	}
	for value := range strings.SplitSeq(fields[2], ",") {
		xid, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return horizonSnapshot{}, fmt.Errorf("parse snapshot %q: xip: %w", text, err)
		}
		if xid < xmin || xid >= xmax {
			return horizonSnapshot{}, fmt.Errorf("parse snapshot %q: in-progress id %d outside [xmin, xmax)", text, xid)
		}
		if n := len(snapshot.xip); n > 0 && xid <= snapshot.xip[n-1] {
			return horizonSnapshot{}, fmt.Errorf("parse snapshot %q: in-progress ids not ascending", text)
		}
		snapshot.xip = append(snapshot.xip, xid)
	}
	return snapshot, nil
}
