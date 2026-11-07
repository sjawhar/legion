// Package testdb gives a test a Postgres database of its own on the devbox and CI Postgres that
// LEGION_TEST_PG_DSN names, dropped when the test ends, so no test sees another's rows, and a way
// to reach it across a network's delay (Delayed). A test is skipped without it: there is nothing
// to run against.
//
// Database is an empty database. A test that needs the daemon's schema takes internal/testpg's
// Clone or CloneStore instead, which copy a template that package migrated once.
//
// A package whose tests migrate into the database their DSN names, rather than making one per
// test, takes one database for the whole run instead: RunDatabaseMain from its TestMain, and
// RunDatabaseFor wherever it reads the DSN. The two mechanisms compose, and a package that needs
// both nests them (internal/daemon's TestMain).
//
// This package imports nothing of this module, and nothing added here may: migrating the template
// needs internal/store, so internal/testpg imports it, and a test binary of internal/store that
// imported internal/testpg back would be a cycle. Keeping the schema-free half apart is what lets
// internal/store's own tests take the same fixture every other package's tests take.
//
// RandomName and DatabaseURL are exported for internal/testpg itself, which names its template
// and each clone of it with them; no other package needs either.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Image is the Postgres server image a developer starts for the daemon's own tests: PostgreSQL 18
// (tag 18, server 18.6-1.pgdg13+2), pinned by digest through the mirror. 18 is also the major
// version of the Postgres service CI's daemon test job runs (ci.yaml's `test` job), and
// the version envoy-infra gives a new Cloud SQL instance.
const Image = "mirror.gcr.io/library/postgres@sha256:979bccfde35445db618c65107d27d08c6c21339d8dc3ad4c830c6633947be0e4"

// SkipMessage is what a test is skipped with when there is no Postgres to run it against. It is
// exported for the packages whose own skip says the same thing (internal/daemon's testDSN).
const SkipMessage = "LEGION_TEST_PG_DSN is unset, so there is no Postgres to test against. In a Legion pod, read skill://legion-worker/references/tools.md, \"The database your tests use\"; never the sessions database. Elsewhere, start one and set it:\n" +
	"  docker run -d --name legion-pg --cpus 2 --memory 2g -e POSTGRES_USER=legion -e POSTGRES_PASSWORD=legion -e POSTGRES_DB=legion -p 127.0.0.1::5432 " + Image + "\n" +
	"  port=$(docker port legion-pg 5432/tcp | head -1 | sed 's/.*://')\n" +
	"  LEGION_TEST_PG_DSN=postgres://legion:legion@127.0.0.1:$port/legion go test ./..."

// Database is the URL of a new, empty database named <prefix>_<random>, dropped at the test's end.
func Database(t testing.TB, prefix string) string {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip(SkipMessage)
	}
	database, drop, err := create(dsn, prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := drop(); err != nil {
			t.Error(err)
		}
	})
	return database
}

// create makes an empty database named <prefix>_<random> on the server dsn names, and answers its
// URL and the function that drops it, which also closes the admin connection create opened. The
// URL is dsn with its path replaced, so it keeps dsn's query. Database and RunDatabaseMain are
// its two callers: one database per test, one per run.
func create(dsn, prefix string) (string, func() error, error) {
	base, err := url.Parse(dsn)
	if err != nil {
		// url.Parse's own error quotes the URL it was given, so only the error it wraps is printed.
		return "", nil, fmt.Errorf("parse LEGION_TEST_PG_DSN: %w", errors.Unwrap(err))
	}
	admin := *base
	admin.Path = "/postgres"
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		return "", nil, fmt.Errorf("connect to the admin database: %w", err)
	}
	name, err := RandomName(prefix)
	if err != nil {
		_ = conn.Close(ctx)
		return "", nil, err
	}
	// A copy of template1's files. Created the default way, from PostgreSQL 15, every page of
	// template1 goes into the write-ahead log: 4.4 MB of log for each empty database.
	if _, err := conn.Exec(ctx, "create database "+name+" strategy file_copy"); err != nil {
		_ = conn.Close(ctx)
		return "", nil, fmt.Errorf("create %s: %w", name, err)
	}
	drop := func() error {
		defer func() { _ = conn.Close(context.Background()) }()
		if _, err := conn.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			return fmt.Errorf("drop %s: %w", name, err)
		}
		return nil
	}
	return DatabaseURL(*base, name), drop, nil
}

// RandomName is prefix with a random suffix, so two databases of one prefix never collide. It is
// exported for internal/testpg, which names its template and each clone with it.
func RandomName(prefix string) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("random database name: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(suffix[:]), nil
}

// DatabaseURL is server with its path replaced by database, so the URL keeps server's query. It is
// exported for internal/testpg, which addresses its template and each clone with it.
func DatabaseURL(server url.URL, database string) string {
	server.Path = "/" + database
	return server.String()
}

// runServer is the server RunDatabaseMain made the run's database on, named by serverKey, and
// runDatabase the URL of that database; both are empty until a RunDatabaseMain with
// LEGION_TEST_PG_DSN set runs.
var (
	runServer   string
	runDatabase string
)

// serverKey is the part of a DSN that names the server rather than a database on it, so two DSNs
// that differ only in the database they name have one key. A DSN no URL parser reads is its own
// key, which no parsed one equals.
func serverKey(dsn string) string {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	parsed.Path, parsed.RawPath = "", ""
	return parsed.String()
}

// RunDatabaseMain makes one database named <prefix>_<random> for the whole run on the server
// LEGION_TEST_PG_DSN names, runs run, drops that database and returns run's code; a drop that
// fails prints why and turns a passing run into exit 1. A package whose tests migrate into the
// database their DSN names calls it from TestMain, where every test reads the run's database
// through RunDatabaseFor, so two runs of the package on one server never write each other's rows
// and neither writes the database that server serves. With LEGION_TEST_PG_DSN unset it makes
// nothing and returns run(), so those tests skip as they do without a server.
//
// It is not internal/testpg's Main, which prepares the template Clone copies: the two are
// different databases for different needs, and a package that wants both nests them, each under a
// prefix of its own so a census of either name says which it counted:
//
//	os.Exit(testpg.Main("legion_daemon_test", func() int {
//		return testdb.RunDatabaseMain("legion_daemon_run", m.Run)
//	}))
//
// It takes run rather than *testing.M so that it wraps another TestMain helper.
func RunDatabaseMain(prefix string, run func() int) int {
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		return run()
	}
	database, drop, err := create(dsn, prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "make the package's run database: %v\n", err)
		return 1
	}
	runServer, runDatabase = serverKey(dsn), database
	code := run()
	if err := drop(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

// RunDatabaseFor is the URL of the database RunDatabaseMain made for this run when dsn names the
// server that database is on, which is the same URL but for the path, and dsn itself otherwise: a
// test that points LEGION_TEST_PG_DSN at a server of its own keeps what it set. Without a run
// database — no RunDatabaseMain, or LEGION_TEST_PG_DSN unset — it is dsn.
func RunDatabaseFor(dsn string) string {
	if runDatabase == "" || serverKey(dsn) != runServer {
		return dsn
	}
	return runDatabase
}

// DSNFile is a 0600 file under the test's temporary directory holding Database's URL, as the
// providers Secret's key holds the session database's.
func DSNFile(t testing.TB, prefix string) (dsn, file string) {
	t.Helper()
	dsn = Database(t, prefix)
	file = filepath.Join(t.TempDir(), "OMP_SESSION_SQL_DSN")
	if err := os.WriteFile(file, []byte(dsn+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dsn, file
}
