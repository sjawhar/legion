// Package testpg gives a test a Postgres database of its own on the devbox and CI Postgres that
// LEGION_TEST_PG_DSN names, dropped when the test ends, so no test sees another's rows, and a way
// to reach it across a network's delay (Delayed). A test is skipped without it: there is nothing
// to run against.
package testpg

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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Image is the Postgres server image a developer starts for the daemon's own tests: PostgreSQL 18
// (tag 18, server 18.6-1.pgdg13+2), pinned by digest through the mirror. 18 is also the major
// version of the Postgres service CI's daemon test job runs (pr-and-main.yaml's `test` job), and
// the version envoy-infra gives a new Cloud SQL instance.
const Image = "mirror.gcr.io/library/postgres@sha256:979bccfde35445db618c65107d27d08c6c21339d8dc3ad4c830c6633947be0e4"

// Database is the URL of a new, empty database named <prefix>_<random>, dropped at the test's end.
func Database(t testing.TB, prefix string) string {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LEGION_TEST_PG_DSN is unset, so there is no Postgres to test against. In a Legion pod, read skill://legion-worker/references/tools.md, \"The database your tests use\"; never the sessions database. Elsewhere, start one and set it:\n" +
			"  docker run -d --name legion-pg --cpus 2 --memory 2g -e POSTGRES_USER=legion -e POSTGRES_PASSWORD=legion -e POSTGRES_DB=legion -p 127.0.0.1::5432 " + Image + "\n" +
			"  port=$(docker port legion-pg 5432/tcp | head -1 | sed 's/.*://')\n" +
			"  LEGION_TEST_PG_DSN=postgres://legion:legion@127.0.0.1:$port/legion go test ./...")
	}
	base, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse LEGION_TEST_PG_DSN: %v", err)
	}
	admin := *base
	admin.Path = "/postgres"
	conn, err := pgx.Connect(context.Background(), admin.String())
	if err != nil {
		t.Fatalf("connect to the admin database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	name := prefix + "_" + hex.EncodeToString(suffix[:])
	if _, err := conn.Exec(context.Background(), "create database "+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		if err := Drop(context.Background(), conn, name); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})
	database := *base
	database.Path = "/" + name
	return database.String()
}

// insufficientPrivilege is PostgreSQL's SQLSTATE 42501, which a forced drop answers when the
// login may not terminate a session of another role.
const insufficientPrivilege = "42501"

// Execer is the one method Drop needs, which pgx.Conn and pgxpool.Pool each have.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Drop drops a test's own database, waiting out a backend it may not terminate.
//
// `with (force)` terminates every other session on the database, and PostgreSQL refuses that to a
// login that is neither a superuser nor a member of pg_signal_backend as soon as one of those
// sessions belongs to another role (SQLSTATE 42501, "permission denied to terminate process").
// The test's own sessions are its own role's and are no trouble; what arrives unbidden is an
// autovacuum worker, which runs as the server's superuser and attaches to a database the test
// just filled. It leaves of its own accord within moments, so the drop is tried again until it
// does, rather than asking the deployment for a privilege a test does not need: every Legion pod
// shares one PostgreSQL whose `legion_test` login holds neither (skill://legion-worker's
// references/tools.md, "The database your tests use"), where CI's container runs the tests as the
// superuser and never sees this. A refusal that outlasts the wait is returned as it stands.
func Drop(ctx context.Context, admin Execer, name string) error {
	deadline := time.Now().Add(30 * time.Second)
	for attempt := 0; ; attempt++ {
		_, err := admin.Exec(ctx, "drop database "+name+" with (force)")
		if err == nil {
			return nil
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != insufficientPrivilege {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w (still refused after %d attempts over 30s: a session of another role, an autovacuum worker, holds the database and this login may not terminate it)", err, attempt+1)
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
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
