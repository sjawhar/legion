package duty

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Leases is the slice of a connection pool a lease needs, named here so this package depends on no
// store: *pgxpool.Pool satisfies it, and the one the server passes is store.Pool.Leases, a pool of
// its own so a pass that runs for minutes holds none of the shared pool's connections.
type Leases interface {
	// Acquire takes one connection the caller holds until it releases it: where a leased pass
	// keeps its advisory lock, which Postgres scopes to the session that took it.
	Acquire(ctx context.Context) (*pgxpool.Conn, error)
}

// leaseNamespace is the first key of every loop lease ("LOOP"), and the reason a lease is taken in
// the two-key form at all: Postgres keeps the two-key space apart from the single-key one, which
// internal/dispatch/docs already fills - a document's room, a project's rank allocation, an agent's
// artifacts, the events' commit order and the migration runner are each a single key (the
// enumeration in docs/copied_asks.go, at lockProjectCopies) - so a lease can never collide with one
// of those, whatever its name hashes to. Within the two-key space the only other namespace is the
// copy locks' (docs.copiedAsksLockNamespace), so a lease's second key, its name's hashtext,
// separates the leases from each other and from nothing else.
const leaseNamespace int32 = 0x4C4F4F50

// takeLease tries for a lease without queueing behind its holder, and freeLease gives it back. Both
// are session-level - not the pg_advisory_xact_lock the copy locks take - because a pass holds its
// lease outside any transaction; see Exclusive.
const (
	takeLease = `select pg_try_advisory_lock($1, hashtext($2))`
	freeLease = `select pg_advisory_unlock($1, hashtext($2))`
)

// readPeriod answers two things, both from the database's clock: the aligned start of the period
// the tick about to run falls in - to_timestamp(floor(epoch / period) * period) - and whether a
// pass has already served that period. recordPass stores that boundary, with the period it was
// computed from, once the pass has run (migration 0115).
//
// Boundaries rather than a window measured back from now, because a loop whose period is its
// ticker's interval skipped about a third of its ticks under one: the ticker fires every
// interval, the row is stamped a few milliseconds later when the lease is taken, and
// "began_at > now() - period" then reduces to a comparison of this tick's lease-acquire delay
// against the previous tick's, which is a coin toss (measured on a lone process against real
// Postgres: ran 7 skipped 3, ran 6 skipped 4, ran 6 skipped 4 over ten one-second ticks). Both
// sides of this comparison are boundaries rather than instants, so it cannot turn on that delay:
// a tick one period after the last one is always at the next boundary, and no tolerance constant
// has to be chosen to make that true.
//
// The period is stored and compared beside the boundary because a boundary means nothing on
// another period's scale. docs.RunReferenceExcerptReconciliation varies its own period between
// ticks - two seconds while drift remains, five minutes when none does - and its two-second
// boundary lies at or after the current five-minute one for all but the first two seconds of
// every five-minute bucket, so a comparison that ignored the scale would refuse that loop's
// interval tick about 149 times in 150 (measured; and under the bucket-number form this
// replaces, which was scale-dependent in the same way, it refused every later tick for ever).
// A record made on another scale therefore serves nothing: the first tick of a changed period
// runs, and the loop is on its own scale from then on. That costs one pass at each change, which
// is the direction to err in - the other direction is a loop that stops.
//
// Everything is read in the database because two processes' wall clocks differ by whatever their
// hosts' do, and the database's is the one clock every pass is judged by. The period crosses as
// the whole number of nanoseconds a Go duration is, stored and compared as that integer, and the
// seconds the boundary arithmetic needs are derived from it in numeric: a scale equality decided
// by a float's rounding would be this same defect in a new coat.
const (
	readPeriod = `with tick as (
			select to_timestamp(
				floor(extract(epoch from now()) / ($2::numeric / 1000000000))
				* ($2::numeric / 1000000000)) as boundary)
		select tick.boundary, exists (
			select 1 from duty_passes
			where lease = $1 and duty_passes.period_ns = $2
			  and duty_passes.boundary >= tick.boundary) from tick`
	recordPass = `insert into duty_passes (lease, boundary, period_ns) values ($1, $2, $3)
		on conflict (lease) do update set boundary = excluded.boundary,
			period_ns = excluded.period_ns`
)

// teardownTimeout bounds the three calls a pass makes once its own work is over: the record of the
// pass, the unlock that gives the lease back, and the close of a connection whose unlock failed.
// Each runs on a context of its own rather than the pass's, because the pass's is already
// cancelled at shutdown and a lease must still be given back then - but a context that merely
// cannot be cancelled is also one with no deadline, and that is a second failure. On a half-open
// connection, one whose network path has gone away without an RST, a send draws neither an answer
// nor an error, so an unbounded call waits for ever: it wedges the goroutine of the loop that
// called Exclusive, which makes these calls on its own stack, and the Hijack and Close that exist
// precisely to destroy a connection that may still hold the session lease never run, leaving the
// lease with a session this process can no longer reach.
//
// Five seconds is far longer than any of the three takes on a connection that works, and that is
// the point: it has to exceed a slow round trip, not a slow query. The record is one upsert on
// duty_passes' primary key (lease, migration 0115); the unlock is pg_advisory_unlock on a lock
// this very session holds, which touches Postgres's in-memory lock table and no relation at all;
// and the close is a Terminate message and a socket close. It is the same five seconds
// pglisten.DefaultPingTimeout gives the keepalive ping and the close of a connection that
// listener gives up on (internal/dispatch/pglisten/listener.go), chosen there against this same
// half-open connection.
const teardownTimeout = 5 * time.Second

// errLeaseNotHeld is pg_advisory_unlock answering false: the session this pass ran on did not hold
// the lease it was about to give back, so what that connection holds is unknown.
var errLeaseNotHeld = errors.New("the unlock reports this session did not hold the lease")

// Exclusive runs pass exactly when this process takes the lease named name and that loop's period
// is not already served, and reports whether it ran. It is the once-per-period guard a periodic
// maintenance loop needs once two envoy-dispatch processes serve one database: each process fires
// a pass of its own on a ticker of its own, and the two tickers are out of phase - each draws its
// phase at boot (RunJittered), and a rolling update starts the second process minutes after the
// first - so their passes almost never overlap and a guard that only excluded concurrent passes
// would let every process run the whole pass every period.
//
// period is the loop's own interval, the time it means to leave between passes, and is compared
// against the database's record of when this lease's last pass began (duty_passes, migration
// 0115). A period of zero or less is no period at all - mutual exclusion and nothing else - which
// is what the delivery consumers' reconciliation wants: it is not a periodic loop, since it runs
// on every bind poll, and it carries a throttle of its own (delivery.bindThrottle). Such a pass
// reads and records nothing.
//
// The lease is a session-level two-key advisory lock, leaseNamespace and the name's hashtext, taken
// on a connection of its own with pg_try_advisory_lock: a lease the database reports held elsewhere
// is a skip and not a queue, because the other process is running that very pass and waiting for it
// would only run the same work again, late. The pass is called on ctx, and the lease is given back
// when it returns, however it returns.
//
// Everything the period decides happens under the lease: the record is read after the lease is
// taken and written before it is given back, so the two processes' passes are ordered by the one
// thing both wait on. A pass skipped because the period is already served reports itself with the
// same line as one skipped because the lease was held - both are this tick doing no work another
// pass has not already done. Nothing here is leadership: no process holds the loop, and a process
// that dies mid-pass costs that loop one period rather than a hand-over.
//
// It opens no transaction. A pass runs for as long as its work takes, and the pre-deploy census
// refuses a session holding a lock on a table a pending migration touches whose transaction has
// been open a minute (cmd/dispatch/README.md, "Pre-deploy census"), so a pass that held its lease
// inside a transaction would fail a deploy taken while it ran.
//
// Because the lock belongs to the session, Postgres drops it when that session's backend exits: a
// process killed in the middle of a pass - a task replaced by a deploy, an out-of-memory kill -
// leaves no lease behind, and the surviving process takes it on its next tick. A lease row in a
// table would instead have to be waited out.
func Exclusive(ctx context.Context, leases Leases, name string, period time.Duration, pass func(context.Context)) bool {
	conn, err := leases.Acquire(ctx)
	if err != nil {
		reportFailure(name, "dispatch duty: take the lease", err)
		return false
	}
	var held bool
	if err := conn.QueryRow(ctx, takeLease, leaseNamespace, name).Scan(&held); err != nil {
		conn.Release()
		reportFailure(name, "dispatch duty: take the lease", err)
		return false
	}
	if !held {
		conn.Release()
		slog.Info("dispatch duty: leased pass skipped", "lease", name)
		return false
	}
	// Deferred, so a pass that panics gives its lease back on the way out rather than holding it
	// for as long as the connection lives.
	defer freeLeaseOn(conn, name)
	boundary, served, err := periodServed(ctx, conn, name, period)
	if err != nil {
		// The record could not be read, so whether this period is served is unknown. The pass
		// does not run: a tick that runs on no answer is the duplicate work the record is here
		// to stop, and the next tick is one period away at most.
		reportFailure(name, "dispatch duty: read the loop's last pass", err)
		return false
	}
	if served {
		// The same line a lease held elsewhere writes, and for the same reason: this tick would
		// be the pass some process has already made of this period, and the loops' tests count
		// one skip either way.
		slog.Info("dispatch duty: leased pass skipped", "lease", name)
		return false
	}
	pass(ctx)
	if ctx.Err() != nil {
		// Shutdown cut this pass short, so what it did is a part of a pass and not one: the
		// period is left unserved deliberately, and whichever process is up at the next tick
		// makes the pass. A record here would be a period served by work that did not finish -
		// for the webhook sweep, a window of failed deliveries nobody lists; for the daily
		// compaction, a day - and a rolling update, which sends SIGTERM, is how that would
		// ordinarily happen rather than rarely. The line says so, so the tick that looks like a
		// skip is findable afterwards.
		//
		// A pass that finished microseconds before the cancellation arrived is read as cut short
		// too, and costs one early pass next period. That is the safe direction of the two and
		// is deliberately not tightened: the reverse error is the silent gap above.
		slog.Info("dispatch duty: leased pass cut short by shutdown; its period is left unserved", "lease", name)
		return false
	}
	recordPassOn(conn, name, boundary, period)
	slog.Info("dispatch duty: leased pass ran", "lease", name)
	return true
}

// periodServed answers the aligned start of the period this tick falls in - the boundary
// to_timestamp(floor(epoch / period) * period) on the database's clock - and whether a pass has
// already served that period, whichever process ran it. A period of zero or less has no period to
// serve and reads nothing.
//
// Both sides of the comparison are boundaries, never an instant: a boundary compared against
// now() would let the acquire-delay jitter back in through a different door. And a stored
// boundary counts only against a tick of the same period, since a boundary carries no record of
// the scale that produced it (readPeriod).
func periodServed(ctx context.Context, conn *pgxpool.Conn, name string, period time.Duration) (boundary time.Time, served bool, err error) {
	if period <= 0 {
		return time.Time{}, false, nil
	}
	err = conn.QueryRow(ctx, readPeriod, name, period.Nanoseconds()).Scan(&boundary, &served)
	return boundary, served, err
}

// recordPassOn records that this pass served its period, as the boundary the gate read and the
// period that produced it. It writes that boundary rather than one computed again: a second read
// could land in the next period and leave this one looking unserved.
//
// It runs on the lease's own connection and before the lease is given back, on a context of its
// own because the pass's own may already be cancelled - a context that is cancelled is also the
// one case this is never called in, since a pass shutdown cut short serves no period (Exclusive).
// This comment said the opposite until INFRA-66's second round: that a pass cut short by shutdown
// still ran and still served its period, which was the behaviour and was wrong both times it was
// written. A record that never lands only costs one early pass, which is why its failure is
// logged and not returned - and why teardownTimeout bounds that context as well as detaching it,
// since an upsert that never answers costs this loop its goroutine and the lease behind it rather
// than one record. A pass with no period records nothing.
func recordPassOn(conn *pgxpool.Conn, name string, boundary time.Time, period time.Duration) {
	if period <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
	defer cancel()
	if _, err := conn.Exec(ctx, recordPass, name, boundary, period.Nanoseconds()); err != nil {
		slog.Error("dispatch duty: record the pass", "lease", name, "error", err)
	}
}

// reportFailure logs a lease this process could not take, or a record it could not read - no
// connection, or a statement that failed. A cancelled context is the process shutting down rather
// than a fault, and says nothing: every loop ends that way.
//
// context.Canceled is the only error silenced here, and context.DeadlineExceeded is deliberately
// not kept beside it: a call that got no answer inside a bound is the fault worth seeing, not a
// shutdown. errors.Is already keeps the two apart - a context that reaches its deadline wraps
// DeadlineExceeded and never Canceled - so the check below logs a deadline rather than swallowing
// it, and the two are not to be tidied into one condition. The three calls teardownTimeout bounds
// log their own failures directly rather than through here; what reaches this function is the
// pass's own context, which carries a deadline whenever its caller gave it one.
func reportFailure(name, message string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	slog.Error(message, "lease", name, "error", err)
}

// freeLeaseOn unlocks name's lease on the connection that took it and gives that connection back.
// The unlock runs on a context of its own, because the pass's ctx is cancelled at shutdown and a
// lease must be given back then too, bounded by teardownTimeout because an unbounded unlock on a
// half-open connection would hold the loop's goroutine here for the life of the process and never
// reach the close below.
//
// A connection whose unlock fails - including by reaching that deadline - or reports that the
// session held nothing, may still hold the lease, and the pool would hand it to the next pass,
// which would take a lease it already holds and unlock it twice over. So it is destroyed instead
// of returned: hijacked out of the pool and closed, which ends the session and drops whatever it
// holds. The timed-out unlock is the case that needs this most, since nothing at all is known of
// that session, and it is the path by which the lease is released by closing the session rather
// than by the unlock. The close is given a teardownTimeout of its own, since the unlock's is
// spent by the time its failure is what brings us here.
func freeLeaseOn(conn *pgxpool.Conn, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
	defer cancel()
	freed := false
	err := conn.QueryRow(ctx, freeLease, leaseNamespace, name).Scan(&freed)
	if err == nil && !freed {
		err = errLeaseNotHeld
	}
	if err == nil {
		conn.Release()
		return
	}
	slog.Error("dispatch duty: free the lease", "lease", name, "error", err)
	dead := conn.Hijack()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), teardownTimeout)
	defer closeCancel()
	if err := dead.Close(closeCtx); err != nil {
		slog.Error("dispatch duty: close the lease's connection", "lease", name, "error", err)
	}
}
