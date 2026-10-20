package duty

import (
	"context"
	"errors"
	"log/slog"

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

// errLeaseNotHeld is pg_advisory_unlock answering false: the session this pass ran on did not hold
// the lease it was about to give back, so what that connection holds is unknown.
var errLeaseNotHeld = errors.New("the unlock reports this session did not hold the lease")

// Exclusive runs pass exactly when this process takes the lease named name, and reports whether it
// ran. It is the once-overall guard a periodic maintenance loop needs once two envoy-dispatch
// processes serve one database, where each process's ticker fires a pass of its own and both would
// otherwise do the same work at the same moment.
//
// The lease is a session-level two-key advisory lock, leaseNamespace and the name's hashtext, taken
// on a connection of its own with pg_try_advisory_lock: a lease the database reports held elsewhere
// is a skip and not a queue, because the other process is running that very pass and waiting for it
// would only run the same work again, late. The pass is called on ctx, and the lease is given back
// when it returns, however it returns.
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
func Exclusive(ctx context.Context, leases Leases, name string, pass func(context.Context)) bool {
	conn, err := leases.Acquire(ctx)
	if err != nil {
		reportLeaseFailure(name, err)
		return false
	}
	var held bool
	if err := conn.QueryRow(ctx, takeLease, leaseNamespace, name).Scan(&held); err != nil {
		conn.Release()
		reportLeaseFailure(name, err)
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
	pass(ctx)
	slog.Info("dispatch duty: leased pass ran", "lease", name)
	return true
}

// reportLeaseFailure logs a lease this process could not even try for - no connection, or a lock
// statement that failed. A cancelled context is the process shutting down rather than a fault, and
// says nothing: every loop ends that way.
func reportLeaseFailure(name string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	slog.Error("dispatch duty: take the lease", "lease", name, "error", err)
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
