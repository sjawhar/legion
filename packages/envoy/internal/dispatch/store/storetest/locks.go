package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// CountLockWaits counts the sessions of database waiting on a lock whose statement matches the LIKE
// pattern like. It reads through the pool, never through a transaction: a transaction's first read
// of pg_stat_activity freezes the view for the rest of it, and a poll inside one spins until it
// times out. It returns its error rather than failing the test, so a hook running off the test's
// goroutine, such as a settlement's, can report it and let the code under test go on.
func CountLockWaits(ctx context.Context, database *store.Store, like string) (int, error) {
	var waiting int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from pg_stat_activity
		where datname = current_database() and wait_event_type = 'Lock' and query like $1
	`, like).Scan(&waiting); err != nil {
		return 0, fmt.Errorf("inspect database locks: %w", err)
	}
	return waiting, nil
}

// OwnerLockWaiters counts the backends of database waiting on an owner lock (the statements
// store.LockOwner and LockOwnerShared run, which name their result owner_lock), and how many of
// them hold a transaction id while they wait: every writer takes its owner's lock in its read
// phase, before its first write, so a waiter holding a transaction id is holding one open behind
// the lock. The caller's own backend is left out. It reads through the pool, as CountLockWaits
// does and for the same reason.
func OwnerLockWaiters(t testing.TB, database *store.Store) (waiting, holdingXid int) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*), count(*) filter (where backend_xid is not null)
		from pg_stat_activity
		where datname = current_database() and wait_event_type = 'Lock' and wait_event = 'advisory'
		  and query like '%owner_lock%' and pid <> pg_backend_pid()
	`).Scan(&waiting, &holdingXid); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	return waiting, holdingXid
}

// AwaitOwnerLockWaiter waits up to ten seconds for a backend to wait on an owner lock
// (OwnerLockWaiters) and answers how many of the waiters hold a transaction id. It returns the
// call's answer and true when the call under test answers on answered before any backend waited,
// as WaitForQueuedBehind does: such a call never waited for the lock the test holds, and the
// caller fails with what it answered. what names that call, for the failure when the ten seconds
// pass first.
func AwaitOwnerLockWaiter[T any](t testing.TB, database *store.Store, what string, answered <-chan T) (T, bool, int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case early := <-answered:
			return early, true, 0
		default:
		}
		if waiting, holdingXid := OwnerLockWaiters(t, database); waiting > 0 {
			var none T
			return none, false, holdingXid
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never waited on an owner lock within 10s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitingBehindHolder is the recursive CTE naming, as waiting(pid), every backend queued behind a
// lock the current transaction owns, directly or behind an earlier waiter.
const waitingBehindHolder = `
	with recursive waiting(pid) as (
		select pid from pg_locks
		where not granted and pg_backend_pid() = any(pg_blocking_pids(pid))
		union
		select blocked.pid from pg_locks blocked, waiting
		where not blocked.granted and waiting.pid = any(pg_blocking_pids(blocked.pid))
	)
`

// WaitForLockWaiters waits up to within for want backends to queue behind a lock holder's
// transaction owns, directly or behind an earlier waiter (a second row-lock waiter is blocked by
// the first, which holds the tuple lock), and returns how many it last saw. It polls through
// holder's own connection: the code under test can drain the shared pool while it waits on that
// lock, and a poll that needed a pool connection of its own would deadlock with it once the pool is
// exhausted. It reads pg_locks, not pg_stat_activity, whose view is frozen for the rest of a
// transaction once read. It counts every backend behind the holder, a background document
// settlement's among them; a test that must know the call under test is the one waiting watches
// that call's backend instead (QueuedBehind).
func WaitForLockWaiters(t testing.TB, holder pgx.Tx, want int, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var count int
		if err := holder.QueryRow(context.Background(), waitingBehindHolder+`select count(*) from waiting`).Scan(&count); err != nil {
			t.Fatalf("inspect database locks: %v", err)
		}
		if count >= want || time.Now().After(deadline) {
			return count
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// QueuedBehind reports whether the backend pid waits on a lock holder's transaction owns,
// directly or behind an earlier waiter, as WaitForLockWaiters counts them, and reads through
// holder's own connection for the same reason. Watching one backend rather than counting them is
// what tells the call under test from any other session queued on the same lock: an issue's
// document settlement locks the issue's row too (docs/solutions/testing/a-lock-wait-test-watches-
// the-pid-of-the-call-under-test-and-its-mutant-fires-only-on-that-call-INFRA-260.md).
func QueuedBehind(ctx context.Context, holder pgx.Tx, pid uint32) (bool, error) {
	var queued bool
	if err := holder.QueryRow(ctx, waitingBehindHolder+`select exists (select 1 from waiting where pid = $1)`, pid).Scan(&queued); err != nil {
		return false, fmt.Errorf("inspect database locks: %w", err)
	}
	return queued, nil
}

// WaitForQueuedBehind waits up to within for the backend pid, the one connection a call under
// test runs on, to queue behind a lock holder's transaction owns (QueuedBehind), polling through
// holder's own connection. It returns the call's answer and true if the call answers on answered
// before its backend has queued: such a call never waited on the lock, and the caller fails with
// what it answered. It fails the test if within passes first.
func WaitForQueuedBehind[T any](t testing.TB, holder pgx.Tx, pid uint32, answered <-chan T, within time.Duration) (T, bool) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(within)
	for {
		select {
		case answer := <-answered:
			return answer, true
		default:
		}
		queued, err := QueuedBehind(ctx, holder, pid)
		if err != nil {
			t.Fatal(err)
		}
		if queued {
			var none T
			return none, false
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend %d never queued behind the lock within %s", pid, within)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
