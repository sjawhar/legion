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

// readPeriod answers two things, both from the database's clock: when the pass about to run
// begins, and whether this loop's period is already served - a pass of the same lease began less
// than one period ago, on this process or on another. recordPass stores that instant once the pass
// has run (migration 0115). The comparison is made in the database because two processes' wall
// clocks differ by whatever their hosts' do, and the database's is the one clock both passes are
// judged by; the period reaches it as seconds, fraction included, since a Go duration is no SQL
// interval.
const (
	readPeriod = `select now(), exists (
		select 1 from duty_passes where lease = $1 and began_at > now() - make_interval(secs => $2))`
	recordPass = `insert into duty_passes (lease, began_at) values ($1, $2)
		on conflict (lease) do update set began_at = excluded.began_at`
)

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
	begins, served, err := periodServed(ctx, conn, name, period)
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
	recordPassOn(conn, name, begins)
	slog.Info("dispatch duty: leased pass ran", "lease", name)
	return true
}

// periodServed reports whether name's period is already served - a pass of this loop began less
// than period ago, whichever process ran it - and answers the instant, on the database's clock, at
// which the pass this tick would run begins. A period of zero or less has no period to serve and
// reads nothing.
func periodServed(ctx context.Context, conn *pgxpool.Conn, name string, period time.Duration) (begins time.Time, served bool, err error) {
	if period <= 0 {
		return time.Time{}, false, nil
	}
	err = conn.QueryRow(ctx, readPeriod, name, period.Seconds()).Scan(&begins, &served)
	return begins, served, err
}

// recordPassOn records the pass that has just run, so every process's next tick inside the period
// skips. It writes the instant the pass began, which is what the period is measured from: a loop's
// ticker measures its interval from the tick that began the pass, and a period measured from the
// end would be short by the pass's own duration at every tick.
//
// It runs on the lease's own connection, before the lease is given back, and on a context of its
// own, because a pass cut short by shutdown still ran and still served its period - and because a
// record that never lands only costs one early pass, which is why its failure is logged and not
// returned. A pass with no period records nothing: begins is the zero time there.
func recordPassOn(conn *pgxpool.Conn, name string, begins time.Time) {
	if begins.IsZero() {
		return
	}
	if _, err := conn.Exec(context.Background(), recordPass, name, begins); err != nil {
		slog.Error("dispatch duty: record the pass", "lease", name, "error", err)
	}
}

// reportFailure logs a lease this process could not take, or a record it could not read - no
// connection, or a statement that failed. A cancelled context is the process shutting down rather
// than a fault, and says nothing: every loop ends that way.
func reportFailure(name, message string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	slog.Error(message, "lease", name, "error", err)
}

// freeLeaseOn unlocks name's lease on the connection that took it and gives that connection back.
// The unlock runs on a context of its own, because the pass's ctx is cancelled at shutdown and a
// lease must be given back then too.
//
// A connection whose unlock fails, or reports that the session held nothing, may still hold the
// lease, and the pool would hand it to the next pass, which would take a lease it already holds and
// unlock it twice over. So it is destroyed instead of returned: hijacked out of the pool and
// closed, which ends the session and drops whatever it holds.
func freeLeaseOn(conn *pgxpool.Conn, name string) {
	ctx := context.Background()
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
	if err := dead.Close(ctx); err != nil {
		slog.Error("dispatch duty: close the lease's connection", "lease", name, "error", err)
	}
}
