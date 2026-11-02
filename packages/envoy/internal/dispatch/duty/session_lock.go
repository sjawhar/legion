package duty

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSessionClosed is FreeSessionLock's answer for a connection that was closed before its unlock
// was sent. The statement that closed it has reported its own failure to its caller, and Postgres
// drops the session's locks as the session ends, so there is nothing left to unlock.
var ErrSessionClosed = errors.New("the connection was closed before the unlock; its locks go with its session")

// errLockNotHeld is pg_advisory_unlock answering false: the session did not hold the lock it was
// about to give back, so what that connection holds is unknown.
var errLockNotHeld = errors.New("the unlock reports this session did not hold the lock")

// FreeSessionLock gives back a session-level advisory lock taken on conn, and then conn itself.
// unlock is a statement that answers pg_advisory_unlock's boolean, run with args. conn goes back
// to its pool once Postgres answers true. Every session-level advisory lock Dispatch takes on a
// pooled connection is given back this way: a loop's lease (freeLeaseOn), a document room's lock
// (internal/dispatch/docs, lockRoom) and the files backfill's lock (internal/dispatch/files).
//
// The unlock runs under a leaseCall of its own, never the holder's context, for two reasons. A
// lock must be given back after its holder's context has ended too. And an unbounded unlock on a
// half-open connection would hold its caller here, and everything queued behind that caller,
// until TCP gives the connection up.
//
// An unlock that fails, reaches that bound or answers false leaves what the session holds
// unknown. The pool would hand such a connection to its next caller, which could take a lock the
// session already holds and unlock it twice over. So the connection is destroyed instead of
// returned: it is hijacked out of the pool and closed under a leaseCall of its own. Closing ends
// the session, and Postgres drops every lock the session held.
//
// The answer is nil once the unlock answered true, ErrSessionClosed when conn was closed before
// the unlock, and otherwise what failed, for the caller to log beside what the lock was for. conn
// is released or closed whatever the answer, so the caller does not use it again.
func FreeSessionLock(conn *pgxpool.Conn, unlock string, args ...any) error {
	if conn.Conn().IsClosed() {
		conn.Release()
		return ErrSessionClosed
	}
	ctx, cancel := leaseCall()
	defer cancel()
	freed := false
	err := conn.QueryRow(ctx, unlock, args...).Scan(&freed)
	if err == nil && !freed {
		err = errLockNotHeld
	}
	if err == nil {
		conn.Release()
		return nil
	}
	dead := conn.Hijack()
	closeCtx, closeCancel := leaseCall()
	defer closeCancel()
	if closeErr := dead.Close(closeCtx); closeErr != nil {
		return fmt.Errorf("%w; close its connection: %v", err, closeErr)
	}
	return err
}
