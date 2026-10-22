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

// readPeriod answers two things, both from the database's clock: which period the tick about to
// run falls in, and whether a pass has already served it. A period is a bucket of the clock -
// floor(epoch / period) - rather than a window measured back from now, and the gate asks only
// whether a row holds that bucket number. recordPass stores the bucket once the pass has run
// (migration 0115).
//
// Buckets rather than a sliding window, because a loop whose period is its ticker's interval
// skipped about a third of its ticks under one: the ticker fires every interval, the row is
// stamped a few milliseconds later when the lease is taken, and "began_at > now() - period" then
// reduces to a comparison of this tick's lease-acquire delay against the previous tick's, which is
// a coin toss (measured on a lone process against real Postgres: ran 7 skipped 3, ran 6 skipped 4,
// ran 6 skipped 4 over ten one-second ticks). A bucket does not depend on when the tick fired: a
// tick one period after the last one is always in the next bucket, whatever the delay, so a lone
// process makes a pass on every tick and no constant has to be chosen to make that true.
//
// Both are read in the database because two processes' wall clocks differ by whatever their
// hosts' do, and the database's is the one clock every pass is judged by; the period reaches it as
// seconds, fraction included, since a Go duration is no SQL interval.
const (
	readPeriod = `with tick as (select floor(extract(epoch from now()) / $2)::bigint as bucket)
		select tick.bucket, exists (
			select 1 from duty_passes
			where lease = $1 and duty_passes.bucket >= tick.bucket) from tick`
	recordPass = `insert into duty_passes (lease, bucket) values ($1, $2)
		on conflict (lease) do update set bucket = excluded.bucket`
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
	bucket, served, err := periodServed(ctx, conn, name, period)
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
	recordPassOn(conn, name, bucket, period)
	slog.Info("dispatch duty: leased pass ran", "lease", name)
	return true
}

// periodServed answers the period this tick falls in - the bucket of the database's clock,
// floor(epoch / period) - and whether a pass has already served it, whichever process ran that
// pass. A period of zero or less has no period to serve and reads nothing.
//
// The comparison is on the bucket number alone, never on a time derived from it: a bucket turned
// back into an instant and compared against now() would let the acquire-delay jitter back in
// through a different door.
func periodServed(ctx context.Context, conn *pgxpool.Conn, name string, period time.Duration) (bucket int64, served bool, err error) {
	if period <= 0 {
		return 0, false, nil
	}
	err = conn.QueryRow(ctx, readPeriod, name, period.Seconds()).Scan(&bucket, &served)
	return bucket, served, err
}

// recordPassOn records that this pass served its period's bucket, so every process's next tick
// inside that bucket skips. It writes the bucket the gate read, not one computed again: a second
// read could land in the next bucket and leave this one looking unserved.
//
// It runs on the lease's own connection and before the lease is given back, on a context of its
// own because the pass's own may already be cancelled - a context that is cancelled is also the
// one case this is never called in, since a pass shutdown cut short serves no bucket (Exclusive).
// This comment said the opposite until INFRA-66's second round: that a pass cut short by shutdown
// still ran and still served its period, which was the behaviour and was wrong both times it was
// written. A record that never lands only costs one early pass, which is why its failure is
// logged and not returned. A pass with no period records nothing.
func recordPassOn(conn *pgxpool.Conn, name string, bucket int64, period time.Duration) {
	if period <= 0 {
		return
	}
	if _, err := conn.Exec(context.Background(), recordPass, name, bucket); err != nil {
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
