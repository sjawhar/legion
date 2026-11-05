package storetest

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMain(m *testing.M) { os.Exit(Main(m)) }

// Open clones the template by copying its files (the package comment says why). The test reads
// the write-ahead log of the one backend that runs a clone (pg_stat_get_backend_wal, Postgres 18),
// so what other sessions on the server write does not count.
func TestACloneWritesTheLogOfAFileCopyNotOfTheTemplate(t *testing.T) {
	Open(t) // prepares the template, or skips the test
	ctx := context.Background()
	conn, err := shared.admin.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire an admin connection: %v", err)
	}
	defer conn.Release()
	var templateBytes int64
	if err := conn.QueryRow(ctx, "select pg_database_size($1)", shared.template).Scan(&templateBytes); err != nil {
		t.Fatalf("read the template's size: %v", err)
	}

	name := randomName(t, "dispatch_test_")
	before := backendWALBytes(t, conn)
	if _, err := conn.Exec(ctx, cloneStatement(name)); err != nil {
		t.Fatalf("clone the template: %v", err)
	}
	dropDatabase(name)
	written := backendWALBytes(t, conn) - before

	if written > templateBytes/100 {
		t.Fatalf("one clone wrote %d bytes of write-ahead log, more than a hundredth of the %d-byte template: it went through the log", written, templateBytes)
	}
	t.Logf("one clone wrote %d bytes of write-ahead log; the template holds %d bytes", written, templateBytes)
}

// Every test is handed a database of its own, carrying the migrations and nothing another test
// wrote, and that database leaves the server when the test ends. Broken, a test reads another
// test's rows, or a run leaves its databases behind on a server the next run has to share.
func TestEachTestIsHandedADatabaseOfItsOwnWhichIsDroppedAfterwards(t *testing.T) {
	ctx := context.Background()
	first := Open(t)
	second := Open(t)
	var firstName, secondName string
	if err := first.Pool.QueryRow(ctx, "select current_database()").Scan(&firstName); err != nil {
		t.Fatalf("read the first database's name: %v", err)
	}
	if err := second.Pool.QueryRow(ctx, "select current_database()").Scan(&secondName); err != nil {
		t.Fatalf("read the second database's name: %v", err)
	}
	if firstName == secondName {
		t.Fatalf("two tests were handed the same database %s", firstName)
	}

	if _, err := first.Pool.Exec(ctx, `insert into projects (key, name) values ('TEST', 'Test')`); err != nil {
		t.Fatalf("write a row in the first database: %v", err)
	}
	var elsewhere int
	if err := second.Pool.QueryRow(ctx, `select count(*) from projects where key = 'TEST'`).Scan(&elsewhere); err != nil {
		t.Fatalf("read the second database: %v", err)
	}
	if elsewhere != 0 {
		t.Fatalf("what one test wrote is in another test's database: it holds %d such row(s)", elsewhere)
	}

	// The drop runs in the background when the inner test ends, and Main waits for it; what this
	// reads is that the cleanup is registered at all, on a database of a subtest of its own.
	var dropped string
	t.Run("a database of its own", func(t *testing.T) {
		inner := Open(t)
		if err := inner.Pool.QueryRow(ctx, "select current_database()").Scan(&dropped); err != nil {
			t.Fatalf("read the inner database's name: %v", err)
		}
	})
	conn, err := shared.admin.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire an admin connection: %v", err)
	}
	defer conn.Release()
	shared.drops.Wait()
	var held int
	if err := conn.QueryRow(ctx, "select count(*) from pg_database where datname = $1", dropped).Scan(&held); err != nil {
		t.Fatalf("read whether the server still holds %s: %v", dropped, err)
	}
	if held != 0 {
		t.Fatalf("%s is still on the server after the test that took it ended", dropped)
	}
}

// backendWALBytes is the write-ahead log conn's backend has written. pg_stat_get_backend_wal
// reads the counters the backend last flushed, and the forced flush runs as its own statement ends.
func backendWALBytes(t *testing.T, conn *pgxpool.Conn) int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.Exec(ctx, "select pg_stat_force_next_flush()"); err != nil {
		t.Fatalf("force the backend's statistics flush: %v", err)
	}
	var written int64
	if err := conn.QueryRow(ctx, "select wal_bytes::bigint from pg_stat_get_backend_wal(pg_backend_pid())").Scan(&written); err != nil {
		t.Fatalf("read the backend's write-ahead log: %v", err)
	}
	return written
}
