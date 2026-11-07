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
//	select m.id, e.created_at, pg_snapshot_xmin(pg_current_snapshot())::text, pg_current_xact_id()::text
//	from (select coalesce(max(id), 0) as id from events) m left join events e on e.id = m.id
//
// It yields A, the newest id visible to the statement's snapshot S, the created_at of the event at
// A (which persisting H stamps it with), the xmin of S, and X, the observer's own transaction id,
// which pg_current_xact_id assigns while the statement runs, after S was taken.
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
// transaction, so a stall can be found. An observation that fails logs its error, and an Await
// whose every observation has failed for ten seconds returns that error to its caller rather
// than wait on.
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
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// horizonPendingLimit bounds the observations waiting for a later one to prove them safe. An
	// entry is 48 bytes, so the list stays under 48 KiB. Writes that overlap without a break keep
	// every observation unproven for D, the length of the open transactions they overlap. While
	// fewer than horizonPendingLimit observations come in D, every one is kept, and H reaches each
	// as soon as a later snapshot proves it. Past that, thinPending spreads the kept observations
	// evenly over D rather than starve the newest, so H trails by at most about
	// 4D/horizonPendingLimit more.
	horizonPendingLimit = 1024
	// Await's re-observation backoff: 10 ms doubling to 250 ms.
	horizonBackoffMin = 10 * time.Millisecond
	horizonBackoffMax = 250 * time.Millisecond
	// horizonAwaitFailFor is how long Await keeps re-observing while every observation fails
	// before it returns the last one's error; one observation that succeeds starts it again. Its
	// long-lived callers (the delivery hub's event seed, the event relay's start and its reseed)
	// wait on a lifetime, not a request, so without it an observation that never succeeds (a
	// horizon pool that cannot dial, say) would leave them waiting for ever with no error to log
	// or retry on. Ten seconds is forty retries at Await's longest backoff: long enough to ride
	// out a redial, short enough that the caller's own warning and retry come soon.
	horizonAwaitFailFor = 10 * time.Second
	// A pending observation older than horizonStallAfter logs a warning, at most once every
	// horizonWarnEvery, so a transaction held open for minutes names itself without flooding
	// the log.
	horizonStallAfter = 5 * time.Second
	horizonWarnEvery  = 30 * time.Second
	// horizonDiagnosisTimeout bounds the stall warning's query of pg_stat_activity, which runs
	// after the observation and must never hold up a reader for long.
	horizonDiagnosisTimeout = 2 * time.Second
	// horizonPersistEvery bounds how often a Horizon that persists (PersistProven) tries to write
	// its H to event_horizon: at most once a second, by the observation that finds H above what it
	// wrote, whether the last try succeeded or failed.
	horizonPersistEvery = time.Second
	// horizonPersistTimeout bounds one such write.
	horizonPersistTimeout = 2 * time.Second
)

// persistHorizonSQL writes H ($1) to event_horizon with $2, the created_at of the event at H as the
// observation that saw it read it, keeping the higher value, unless the row holds a value of a log
// since reset (no event at its safe carries its safe_at), which H replaces. It writes only while
// the event at H still carries $2: after a reset of the log, the row at that id, if any, is a new
// log's, whose created_at is not the one observed, and an H of the old log is never stamped as a
// value of the new one.
const persistHorizonSQL = `
	insert into event_horizon (id, safe, safe_at)
	select 1, e.id, e.created_at from events e where e.id = $1 and e.created_at = $2
	on conflict (id) do update set safe = excluded.safe, safe_at = excluded.safe_at, updated_at = now()
	where event_horizon.safe < excluded.safe
		or not exists (select 1 from events e where e.id = event_horizon.safe and e.created_at = event_horizon.safe_at)
`

// persistedHorizonSQL reads the persisted H when it is a value of the log as it stands now.
const persistedHorizonSQL = `
	select coalesce((select h.safe from event_horizon h
		join events e on e.id = h.safe and e.created_at = h.safe_at
		where h.id = 1), 0)
`

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
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row // the stall diagnosis, WriterFloor and Persisted
}

// Horizon tracks the safe horizon of the event log (see the package documentation's "Safe
// horizon" section): one per process, shared by every reader that pages the log by a global id
// cursor. It is safe for concurrent use.
type Horizon struct {
	db HorizonDB

	// now, stallAfter, warnEvery and logger are the stall and failure warnings' clock, thresholds
	// and sink, and now is also the clock Await measures horizonAwaitFailFor by; NewHorizon sets
	// the production values and a test replaces them. A nil logger is slog.Default at the time of
	// the warning.
	now        func() time.Time
	stallAfter time.Duration
	warnEvery  time.Duration
	logger     *slog.Logger

	mu sync.Mutex
	// generation counts Resets. An observation records only into the generation it started in,
	// so one in flight across a Reset cannot bring back what the Reset forgot.
	generation uint64
	// started counts observations ever started and is never reset: an observation's number is
	// its order, and Safe and Await each wait for one numbered above the count when called.
	started  uint64
	inFlight *horizonFlight
	// safe is H. safeThrough is the number of the newest observation H covers: proven safe, or
	// with an A at or below H when recorded or later. Both are this generation's.
	safe        int64
	safeThrough uint64
	// safeAt is the created_at of the event at safe, as the observation that saw it read it.
	safeAt time.Time
	// newest is the highest A observed this generation (Behind).
	newest int64
	// pending holds the observations still waiting for a later one to prove them safe, oldest
	// first, each with an A above safe.
	pending []horizonObservation
	// warnedAt is when the last stall warning was logged, and failWarnedAt when the last failed
	// observation was.
	warnedAt     time.Time
	failWarnedAt time.Time
	// persist is whether H is written to event_horizon as it advances (PersistProven);
	// persistedSafe is the H last written this generation, persistedAt when the last write was
	// tried, succeeded or failed, and persisting whether a write is under way. persistWarnedAt is
	// when a failed write was last logged.
	persist         bool
	persistedSafe   int64
	persistedAt     time.Time
	persisting      bool
	persistWarnedAt time.Time
}

// horizonObservation is one observation: its number, A, X and when it was recorded.
type horizonObservation struct {
	number uint64
	newest int64
	// newestAt is the created_at of the event at newest as the observation read it, zero when the
	// log was empty: what persisting an H of it stamps (persistHorizonSQL).
	newestAt time.Time
	xid      uint64
	at       time.Time
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

// NewHorizon returns a Horizon observing the event log through db, with H at 0.
func NewHorizon(db HorizonDB) *Horizon {
	return &Horizon{
		db:         db,
		now:        time.Now,
		stallAfter: horizonStallAfter,
		warnEvery:  horizonWarnEvery,
	}
}

// PersistProven has the Horizon write its H to event_horizon as it advances, at most once every
// horizonPersistEvery, so a process that starts later can read the log above it (Persisted). Call it
// before the Horizon is shared.
func (h *Horizon) PersistProven() { h.persist = true }

// Persisted answers the H some process persisted (PersistProven), when it is a value of the event
// log as it stands now, and 0 when none is. Every event at or below it is final: finality is
// permanent, whichever process proved it. It runs one statement and waits on no transaction.
func (h *Horizon) Persisted(ctx context.Context) (int64, error) {
	var safe int64
	if err := h.db.QueryRow(ctx, persistedHorizonSQL).Scan(&safe); err != nil {
		return 0, fmt.Errorf("read the persisted event horizon: %w", err)
	}
	return safe, nil
}

// Safe runs one observation that starts after the call (shared with concurrent callers), checks
// the pending observations against it and returns H, the highest id at or below which every
// event is final (0 before any observation has been proven safe).
//
// Like Await, Safe never settles for an observation already in flight when it is called: that
// one took its snapshot before the call and can miss an id committed just before it, and if its
// A were at or below H it would leave Behind false for the H it answers, so the reader would arm
// no re-read for that id. Safe waits it out and joins or runs the next. Its own observation saw
// every id committed before the call, so each such id is at or below H, or stands above it and
// keeps Behind true until H reaches it.
func (h *Horizon) Safe(ctx context.Context) (int64, error) {
	h.mu.Lock()
	after := h.started
	h.mu.Unlock()
	if err := h.observe(ctx, after); err != nil {
		return 0, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.safe, nil
}

// Await returns H once an observation taken during this call is proven safe, so H is at or
// above every id committed before the call; it re-observes with the 10 ms -> 250 ms backoff. It
// returns early with ctx's error, or with an observation's error once every observation has
// failed for horizonAwaitFailFor.
//
// The observation has to start after the call: one already in flight took its snapshot before
// the call and can miss an id committed just before it, so Await waits that one out rather than
// share it. An observation error is retried like an unproven observation until observations have
// failed for horizonAwaitFailFor with no success between, measured from the first failure since
// the last success, and is then returned; if ctx ends first, the last one is reported with ctx's
// error. A failure the caller's own ctx caused counts for neither.
func (h *Horizon) Await(ctx context.Context) (int64, error) {
	h.mu.Lock()
	after := h.started
	h.mu.Unlock()
	wait := horizonBackoffMin
	var failed error
	var failingSince time.Time
	for {
		err := h.observe(ctx, after)
		if err == nil {
			failed = nil
			h.mu.Lock()
			proven, safe := h.safeThrough > after, h.safe
			h.mu.Unlock()
			if proven {
				return safe, nil
			}
		} else if ctx.Err() == nil {
			now := h.now()
			if failed == nil {
				failingSince = now
			}
			failed = err
			if failingFor := now.Sub(failingSince); failingFor >= horizonAwaitFailFor {
				return 0, fmt.Errorf("await event horizon: every observation failed for %s: %w", failingFor, err)
			}
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

// Behind reports whether an observed id stands above safe, the H its caller read the log
// through: a gap that caller must re-read for. The caller passes the H it read through and not
// the one the Horizon holds now, because between its Safe and this call another reader's
// observation can prove the ids it could not read final, which raises H to the newest id
// observed. Measured against that H nothing stands above, so the events in (safe, newest], which
// no reader has read, would wait for the next commit to be read.
func (h *Horizon) Behind(safe int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.newest > safe
}

// Proven returns H as already proven, running no observation: 0 before any observation has been
// proven safe, and again after a Reset. The proof that set it ran before the call, so every event
// at or below the id it returns was final before the call too. Safe and Await answer an
// observation taken during the call, so an id at or below theirs may have committed during it; a
// reader that must miss nothing committed after the call, and may be sent some of what committed
// just before it, starts at or below Proven instead.
func (h *Horizon) Proven() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.safe
}

// Mark is one observation of the event log that a reader keeps for itself: Newest, the newest id
// its snapshot saw, NewestAt, that event's created_at (zero for an empty log), and Xid, the
// transaction id the observation took after that snapshot.
type Mark struct {
	Newest   int64
	NewestAt time.Time
	Xid      uint64
}

// markSQL is one mark's statement: the newest event and its created_at, as the statement's
// snapshot sees them, and the transaction id pg_current_xact_id assigns while the statement runs,
// after that snapshot was taken.
const markSQL = `
	select coalesce(newest.id, 0), newest.created_at, pg_current_xact_id()::text
	from (select 1) one
	left join lateral (select id, created_at from events order by id desc limit 1) newest on true
`

// Mark takes one observation now and answers it, without recording it among the observations H is
// proven from, and without waiting on any transaction. It consumes one transaction id.
//
// A mark bounds the ids a writer can still hold open (WriterFloor): a transaction whose own id
// came after Xid drew every event id it holds after the mark's snapshot, by the writer invariant,
// and so above Newest. That holds only while the log the mark saw stands: a reset restarts the ids
// at 1, and a writer of the new log draws ids at or below Newest after the mark. NewestAt is what
// tells the two logs apart (WriterFloor).
func (h *Horizon) Mark(ctx context.Context) (Mark, error) {
	batch := &pgx.Batch{}
	batch.Queue(`begin`)
	batch.Queue(`set local synchronous_commit = off`)
	batch.Queue(markSQL)
	batch.Queue(`commit`)
	results := h.db.SendBatch(ctx, batch)
	var mark Mark
	var newestAt *time.Time
	var xidText string
	_, err := results.Exec()
	if err == nil {
		_, err = results.Exec()
	}
	if err == nil {
		err = results.QueryRow().Scan(&mark.Newest, &newestAt, &xidText)
	}
	if err == nil {
		_, err = results.Exec()
	}
	if closeErr := results.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Mark{}, fmt.Errorf("mark the event log: %w", err)
	}
	if newestAt != nil {
		mark.NewestAt = *newestAt
	}
	if mark.Xid, err = strconv.ParseUint(xidText, 10, 64); err != nil {
		return Mark{}, fmt.Errorf("mark the event log: parse transaction id %q: %w", xidText, err)
	}
	return mark, nil
}

// oldestEventWriterSQL answers the oldest transaction id held in this database by a transaction
// that can hold an event id not yet final: a backend that holds a lock on events of a mode an
// insert takes (RowExclusiveLock or stronger), whose own transaction id it reads from that
// backend's granted ExclusiveLock on its id, or a prepared transaction of this database; null when
// there is none. It is widened to the 64-bit id marks carry.
//
// An insert into events locks the table, RowExclusiveLock, before it draws the row's id from
// events_id_seq, and keeps that lock until its transaction ends, and Broker.Append takes its
// transaction id before the insert (the writer invariant). So a transaction holding an event id
// that is not yet final holds both now, and one that holds a transaction id alone (a read in a
// transaction, a write to another table, an operator's open session) holds no event id: any it
// draws later is drawn after this read's marks. That is what lets WriterFloor answer at once
// where the horizon, which waits on every transaction id on the instance, waits. pg_locks shows
// every backend's locks to every role, which pg_stat_activity's backend_xid does not. pg_locks
// gives the 32-bit id; it is widened against the snapshot's xmax, which no running id is more
// than 2^31 from.
const oldestEventWriterSQL = `
	with next as (
		select pg_snapshot_xmax(pg_current_snapshot())::text::numeric as nx
	), held as (
		select x.transactionid::text::numeric as x
		from pg_locks t
		join pg_locks x on x.pid = t.pid and x.locktype = 'transactionid' and x.mode = 'ExclusiveLock' and x.granted
		where t.locktype = 'relation' and t.database = (select oid from pg_database where datname = current_database())
			and t.relation = 'events'::regclass and t.granted and t.pid <> pg_backend_pid()
			and t.mode in ('RowExclusiveLock', 'ShareUpdateExclusiveLock', 'ShareLock', 'ShareRowExclusiveLock', 'ExclusiveLock', 'AccessExclusiveLock')
		union all
		select transaction::text::numeric from pg_prepared_xacts where database = current_database()
	)
	select min(x + 4294967296 * (floor(nx / 4294967296)
		+ case
			when x - mod(nx, 4294967296) > 2147483648 then -1
			when mod(nx, 4294967296) - x > 2147483648 then 1
			else 0
		end))::text
	from held, next
`

// WriterFloor answers the highest id at or below which no event can still be open, read from the
// marks given, the newest first, and from H: the Newest of the first mark older than every
// transaction that holds a write lock on events in this database now (oldestEventWriterSQL), or H
// when no mark is, so H is the floor too when no mark is given. A transaction holding an event id
// that is not yet final drew it after it took its transaction id and holds its lock on events
// until it ends. If it drew that id before a mark's snapshot, at or below the mark's Newest, it
// took its transaction id before the mark, below the mark's Xid, and holds its lock now; so once
// no such transaction is older than the mark, every id at or below the mark's Newest is final.
// ok is false when no mark qualified, so the floor is H alone.
//
// A mark counts only while the log it saw stands: the row at its Newest still carries its
// NewestAt, or it saw an empty log. A mark from before a reset of the log is passed over. So is H
// when it stands above the first mark's Newest, which only a reset the horizon has not yet
// forgotten (Reset) leaves it: the first mark is the newest, and ids only grow until a reset.
func (h *Horizon) WriterFloor(ctx context.Context, marks ...Mark) (floor int64, ok bool, err error) {
	var oldestText *string
	if err := h.db.QueryRow(ctx, oldestEventWriterSQL).Scan(&oldestText); err != nil {
		return 0, false, fmt.Errorf("read the oldest transaction that can write the event log: %w", err)
	}
	var oldest uint64
	if oldestText != nil {
		if oldest, err = strconv.ParseUint(*oldestText, 10, 64); err != nil {
			return 0, false, fmt.Errorf("read the oldest transaction that can write the event log: parse %q: %w", *oldestText, err)
		}
	}
	floor = h.Proven()
	if len(marks) > 0 && floor > marks[0].Newest {
		floor = 0
	}
	for _, mark := range marks {
		if mark.Xid == 0 || (oldestText != nil && oldest <= mark.Xid) {
			continue
		}
		if mark.Newest > 0 {
			var stands bool
			if err := h.db.QueryRow(ctx, `select exists (select 1 from events where id = $1 and created_at = $2)`, mark.Newest, mark.NewestAt).Scan(&stands); err != nil {
				return 0, false, fmt.Errorf("check a mark's log against the event log: %w", err)
			}
			if !stands {
				continue
			}
		}
		return max(floor, mark.Newest), true, nil
	}
	return floor, false, nil
}

// Reset forgets every observation and H; called when a reader detects a reset event log.
func (h *Horizon) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.generation++
	h.safe, h.safeThrough, h.newest = 0, 0, 0
	h.safeAt = time.Time{}
	h.pending = nil
	h.persistedSafe = 0
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
// finds due runs on a goroutine of its own: its diagnosis query can take up to
// horizonDiagnosisTimeout, which would otherwise hold up the relay, the hub or the handler that
// ran the observation. stalled has already marked the warning logged, so the rate limit holds
// however long the diagnosis takes.
//
// An observation that failed while ctx was still live logs its error, at most once every
// warnEvery: Await retries it every 250 ms for up to horizonAwaitFailFor, and its long-lived
// callers retry after that, so without the warning readers would wait with nothing in the log to
// say why. One that failed because ctx ended is the caller's own doing, and logs nothing.
func (h *Horizon) run(ctx context.Context, flight *horizonFlight) {
	observation, xmin, err := h.take(ctx)
	h.mu.Lock()
	h.inFlight = nil
	flight.err = err
	flight.abandoned = err != nil && ctx.Err() != nil
	var stalled *horizonObservation
	var safe, toPersist int64
	var toPersistAt time.Time
	var generation uint64
	if err == nil && flight.generation == h.generation {
		observation.number, observation.at = flight.number, h.now()
		h.record(observation, xmin)
		stalled, safe = h.stalled(observation.at), h.safe
		if h.persist && !h.persisting && h.safe > h.persistedSafe && observation.at.Sub(h.persistedAt) >= horizonPersistEvery {
			h.persisting, toPersist, toPersistAt, generation = true, h.safe, h.safeAt, h.generation
		}
	}
	warnFailure := false
	if err != nil && ctx.Err() == nil {
		now := h.now()
		if h.failWarnedAt.IsZero() || now.Sub(h.failWarnedAt) >= h.warnEvery {
			h.failWarnedAt, warnFailure = now, true
		}
	}
	h.mu.Unlock()
	close(flight.done)
	if warnFailure {
		h.log().Warn("dispatch: event horizon observation failed; live event readers cannot advance until one succeeds",
			"error", err)
	}
	if stalled != nil {
		go h.warnStall(*stalled, observation.at.Sub(stalled.at), safe)
	}
	if toPersist > 0 {
		h.writePersisted(toPersist, toPersistAt, generation)
	}
}

// writePersisted writes safe to event_horizon (persistHorizonSQL) with safeAt, the created_at of
// its event as observed, on the observer's goroutine after the observation's waiters have their
// answer. It writes nothing once a Reset has moved the generation past the one safe was proven in,
// checked under h.mu just before the write, and the statement itself writes nothing once the event
// at safe is another log's, which covers a reset of the log after that check. A try, successful or
// not, is the last one for horizonPersistEvery. A failed write is logged at most once every
// warnEvery and leaves the row as it was: a later process then reads the log above an older value.
func (h *Horizon) writePersisted(safe int64, safeAt time.Time, generation uint64) {
	h.mu.Lock()
	if generation != h.generation {
		h.persisting = false
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), horizonPersistTimeout)
	defer cancel()
	batch := &pgx.Batch{}
	batch.Queue(persistHorizonSQL, safe, safeAt)
	results := h.db.SendBatch(ctx, batch)
	_, err := results.Exec()
	if closeErr := results.Close(); err == nil {
		err = closeErr
	}
	h.mu.Lock()
	now := h.now()
	h.persisting, h.persistedAt = false, now
	warn := false
	if err == nil && generation == h.generation {
		h.persistedSafe = safe
	} else if err != nil && (h.persistWarnedAt.IsZero() || now.Sub(h.persistWarnedAt) >= h.warnEvery) {
		h.persistWarnedAt, warn = now, true
	}
	h.mu.Unlock()
	if warn {
		h.log().Warn("dispatch: could not persist the event horizon; a process that starts later may read more of the event log", "error", err)
	}
}

// take runs one observation: A with its event's created_at, the xmin of the snapshot S its
// statement read and X, the observer's own transaction id. synchronous_commit is off for it alone, since the observer
// writes nothing a crash could lose, and its commit need not wait for a WAL flush to end its
// transaction id.
func (h *Horizon) take(ctx context.Context) (horizonObservation, uint64, error) {
	batch := &pgx.Batch{}
	batch.Queue(`begin`)
	batch.Queue(`set local synchronous_commit = off`)
	batch.Queue(`
		select m.id, e.created_at, pg_snapshot_xmin(pg_current_snapshot())::text, pg_current_xact_id()::text
		from (select coalesce(max(id), 0) as id from events) m left join events e on e.id = m.id
	`)
	batch.Queue(`commit`)
	results := h.db.SendBatch(ctx, batch)
	var newest int64
	var newestAt *time.Time
	var xminText, xidText string
	_, err := results.Exec()
	if err == nil {
		_, err = results.Exec()
	}
	if err == nil {
		err = results.QueryRow().Scan(&newest, &newestAt, &xminText, &xidText)
	}
	if err == nil {
		_, err = results.Exec()
	}
	if closeErr := results.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return horizonObservation{}, 0, fmt.Errorf("observe event horizon: %w", err)
	}
	xmin, err := strconv.ParseUint(xminText, 10, 64)
	if err != nil {
		return horizonObservation{}, 0, fmt.Errorf("observe event horizon: parse snapshot xmin %q: %w", xminText, err)
	}
	xid, err := strconv.ParseUint(xidText, 10, 64)
	if err != nil {
		return horizonObservation{}, 0, fmt.Errorf("observe event horizon: parse transaction id %q: %w", xidText, err)
	}
	observation := horizonObservation{newest: newest, xid: xid}
	if newestAt != nil {
		observation.newestAt = *newestAt
	}
	return observation, xmin, nil
}

// record checks every pending observation against the new one's snapshot xmin, raises H to the
// A of each it proves safe, and keeps the new observation if its A stands above H. h.mu is held.
func (h *Horizon) record(observation horizonObservation, xmin uint64) {
	for _, pending := range h.pending {
		if xmin > pending.xid && pending.newest > h.safe {
			h.safe, h.safeAt = pending.newest, pending.newestAt
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
	if len(h.pending) >= horizonPendingLimit {
		h.thinPending()
	}
	h.pending = append(h.pending, observation)
}

// thinPending makes room in the full pending list. Any later observation can prove any earlier
// one, so dropping one costs only latency: when a snapshot proves a dropped observation, H stops
// at the A of the kept one before it. The kept observations are therefore spread evenly in time,
// not by position. Walking from the oldest, which it keeps (the stall warning reports its age),
// it keeps an observation only when the next one was recorded more than step after the last one
// kept, and it always keeps the newest. step is four times the list's span per slot,
// 4·span/horizonPendingLimit, rounded up to at least a nanosecond.
//
// Two observations it keeps next to each other were then recorded at most step apart, unless
// they were next to each other already. So any two neighbours in the list are at most the
// largest step any thinning used, or the time between two observations, apart, and each step is
// at most 4/horizonPendingLimit of the oldest observation's wait, the length of the open
// transactions holding H back. Of any three observations it keeps in a row, the third was
// recorded more than step after the first, so at most 2·span/step + 2, about half the limit,
// survive, and the newest observation always gets a slot. h.mu is held.
func (h *Horizon) thinPending() {
	last := len(h.pending) - 1
	span := h.pending[last].at.Sub(h.pending[0].at)
	step := max((4*span+horizonPendingLimit-1)/horizonPendingLimit, 1)
	kept := h.pending[:1]
	for i := 1; i < last; i++ {
		if h.pending[i+1].at.Sub(kept[len(kept)-1].at) > step {
			kept = append(kept, h.pending[i])
		}
	}
	h.pending = append(kept, h.pending[last])
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

// warnStall logs that oldest has waited pendingFor to be proven safe, naming the oldest open
// transaction that holds a transaction id. It runs on a goroutine of its own (see run), and the
// diagnosis runs on a context of its own under a short deadline, never one derived from the
// caller's: that one carries the caller's store hold mark (store.WithTransactionTracking,
// store.ForConcurrentRead), with which a Horizon on the shared pool (the fallback when the
// horizon pool cannot open) would have the diagnosis's QueryRow refused as a nested acquire. A
// failed diagnosis is logged in the warning, never returned.
func (h *Horizon) warnStall(oldest horizonObservation, pendingFor time.Duration, safe int64) {
	attrs := []any{
		"pending_for", pendingFor, "observed_id", oldest.newest, "safe_horizon", safe,
		"observer_xid", oldest.xid,
	}
	diagnosis, cancel := context.WithTimeout(context.Background(), horizonDiagnosisTimeout)
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
	h.log().Warn("dispatch: event horizon held back by an open transaction; live event readers wait for it", attrs...)
}

// log is the warnings' sink: logger, or slog.Default at the time of the warning when it is nil.
func (h *Horizon) log() *slog.Logger {
	if h.logger == nil {
		return slog.Default()
	}
	return h.logger
}
