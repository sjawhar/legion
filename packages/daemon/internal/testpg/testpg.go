// Package testpg gives a test a Postgres database of its own on the devbox and CI Postgres that
// LEGION_TEST_PG_DSN names, dropped when the test ends, so no test sees another's rows, and a way
// to reach it across a network's delay (Delayed). A test is skipped without it: there is nothing
// to run against.
//
// Database is an empty database. A test that needs the daemon's schema takes Clone or CloneStore
// instead, which copy a template this process migrated once (template.go); the package whose tests
// call either returns Main from its TestMain, which drops the template.
package testpg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
// URL and the function that drops it, which also closes the admin connection create opened.
func create(dsn, prefix string) (string, func() error, error) {
	base, err := url.Parse(dsn)
	if err != nil {
		return "", nil, fmt.Errorf("parse LEGION_TEST_PG_DSN: %w", err)
	}
	admin := *base
	admin.Path = "/postgres"
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		return "", nil, fmt.Errorf("connect to the admin database: %w", err)
	}
	name, err := randomName(prefix)
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
	return databaseURL(*base, name), drop, nil
}

// randomName is prefix with a random suffix, so two databases of one prefix never collide.
func randomName(prefix string) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("random database name: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(suffix[:]), nil
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
