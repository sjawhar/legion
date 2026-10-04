package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// WaitForLockWaiters waits up to within for want backends to queue behind a lock holder's
// transaction owns, directly or behind an earlier waiter (a second row-lock waiter is blocked by
// the first, which holds the tuple lock), and returns how many it last saw. It polls through
// holder's own connection: the code under test can drain the shared pool while it waits on that
// lock, and a poll that needed a pool connection of its own would deadlock with it once the pool is
// exhausted. It reads pg_locks, not pg_stat_activity, whose view is frozen for the rest of a
// transaction once read.
func WaitForLockWaiters(t testing.TB, holder pgx.Tx, want int, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var count int
		if err := holder.QueryRow(context.Background(), `
			with recursive waiting(pid) as (
				select pid from pg_locks
				where not granted and pg_backend_pid() = any(pg_blocking_pids(pid))
				union
				select blocked.pid from pg_locks blocked, waiting
				where not blocked.granted and waiting.pid = any(pg_blocking_pids(blocked.pid))
			)
			select count(*) from waiting
		`).Scan(&count); err != nil {
			t.Fatalf("inspect database locks: %v", err)
		}
		if count >= want || time.Now().After(deadline) {
			return count
		}
		time.Sleep(10 * time.Millisecond)
	}
}
