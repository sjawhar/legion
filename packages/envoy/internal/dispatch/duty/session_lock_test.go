package duty

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// heldSessionLock takes a connection of leases and the session-level advisory lock key on it.
func heldSessionLock(t *testing.T, leases *store.RawPool, key int64) *pgxpool.Conn {
	t.Helper()
	conn, err := leases.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire a lease connection: %v", err)
	}
	var held bool
	if err := conn.QueryRow(context.Background(), `select pg_try_advisory_lock($1)`, key).Scan(&held); err != nil || !held {
		conn.Release()
		t.Fatalf("take lock %d: held %t, error %v", key, held, err)
	}
	return conn
}

// requireLockFree waits until a session of the shared pool can take key, which it can once the
// session that held it unlocked it or ended, and gives it back.
func requireLockFree(t *testing.T, pool *store.Pool, key int64) {
	t.Helper()
	ctx := context.Background()
	other, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection of the shared pool: %v", err)
	}
	defer other.Release()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var taken bool
		if err := other.QueryRow(ctx, `select pg_try_advisory_lock($1)`, key).Scan(&taken); err != nil {
			t.Fatalf("try lock %d from another session: %v", key, err)
		}
		if taken {
			if _, err := other.Exec(ctx, `select pg_advisory_unlock($1)`, key); err != nil {
				t.Fatalf("give back lock %d: %v", key, err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock %d is still held by another session 5s after FreeSessionLock returned", key)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// requireConnections waits up to 5s for the pool to hold total connections, idle of them idle: a
// connection released closed is destroyed by the pool on a goroutine of its own.
func requireConnections(t *testing.T, leases *store.RawPool, total, idle int32, why string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		stat := leases.Stat()
		if stat.TotalConns() == total && stat.IdleConns() == idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool after FreeSessionLock: total %d, idle %d; want total %d, idle %d: %s",
				stat.TotalConns(), stat.IdleConns(), total, idle, why)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An unlock Postgres confirms gives the lock back and the connection back to its pool.
func TestFreeSessionLockReturnsTheConnectionOnceItsUnlockAnsweredTrue(t *testing.T) {
	database := storetest.Open(t)
	leases := leasesOf(t, database)
	conn := heldSessionLock(t, leases, 7001)

	if err := FreeSessionLock(conn, `select pg_advisory_unlock($1)`, int64(7001)); err != nil {
		t.Fatalf("FreeSessionLock of a held lock: %v", err)
	}
	requireConnections(t, leases, 1, 1, "a confirmed unlock returns the connection")
	requireLockFree(t, database.Pool, 7001)
}

// An unlock that answers false leaves what the session holds unknown, so the connection is closed
// rather than returned, and the lock it did hold goes with the session.
func TestFreeSessionLockClosesTheConnectionWhenItsUnlockAnsweredFalse(t *testing.T) {
	database := storetest.Open(t)
	leases := leasesOf(t, database)
	conn := heldSessionLock(t, leases, 7002)

	err := FreeSessionLock(conn, `select pg_advisory_unlock($1)`, int64(7099))
	if !errors.Is(err, errLockNotHeld) {
		t.Fatalf("FreeSessionLock of a lock the session never took answered %v, want errLockNotHeld", err)
	}
	requireConnections(t, leases, 0, 0, "a connection whose unlock answered false is closed")
	requireLockFree(t, database.Pool, 7002)
}

// An unlock that does not answer within leaseCallTimeout fails at the bound, and the connection is
// closed: a half-open connection would otherwise hold the caller until TCP gave it up.
func TestFreeSessionLockBoundsAnUnlockThatDoesNotAnswer(t *testing.T) {
	database := storetest.Open(t)
	leases := leasesOf(t, database)
	conn := heldSessionLock(t, leases, 7003)

	started := time.Now()
	err := FreeSessionLock(conn, `select pg_advisory_unlock($1) from pg_sleep(60)`, int64(7003))
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FreeSessionLock of an unlock that never answers answered %v, want its deadline", err)
	}
	if elapsed < leaseCallTimeout || elapsed > leaseCallTimeout+10*time.Second {
		t.Fatalf("FreeSessionLock returned after %v, want about its %v bound", elapsed, leaseCallTimeout)
	}
	requireConnections(t, leases, 0, 0, "a connection whose unlock reached its bound is closed")
	requireLockFree(t, database.Pool, 7003)
}

// A connection closed before the unlock - by a statement whose context ended - has nothing to
// unlock: its locks go with its session. FreeSessionLock gives it back, which destroys it, and
// says so with ErrSessionClosed, so a caller whose own statement ended it can stay quiet.
func TestFreeSessionLockAnswersErrSessionClosedForAConnectionAlreadyClosed(t *testing.T) {
	database := storetest.Open(t)
	leases := leasesOf(t, database)
	conn := heldSessionLock(t, leases, 7004)
	if err := conn.Conn().Close(context.Background()); err != nil {
		t.Fatalf("close the lock's connection: %v", err)
	}

	if err := FreeSessionLock(conn, `select pg_advisory_unlock($1)`, int64(7004)); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("FreeSessionLock of a closed connection answered %v, want ErrSessionClosed", err)
	}
	requireConnections(t, leases, 0, 0, "a closed connection is destroyed, not returned")
	requireLockFree(t, database.Pool, 7004)
}
