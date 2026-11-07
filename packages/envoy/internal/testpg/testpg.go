// Package testpg gives a test that brings its own servers, as a natslive test does with testnats,
// an empty Postgres database and the URL to hand a binary. The caller migrates it itself. URL is
// the helper for a test that may have no server: it takes the one DISPATCH_TEST_DATABASE_URL
// names, or starts a container when that variable is unset. DatabaseOn is the helper for a test
// that already holds the URL of the server it runs on: it reads no variable and starts nothing.
// A test of Dispatch's store that wants a migrated store rather than a URL calls
// internal/dispatch/store/storetest instead.
package testpg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image is the pgvector server image CI uses for Dispatch's Postgres service: pgvector 0.8.7 on
// PostgreSQL 18 (tag 0.8.7-pg18), as ghcr.io/cybermonklabs/legion/ci holds it, the copy of
// pgvector/pgvector (.github/ci-images.yaml). The envoy-infra-go job pulls it before the natslive
// tests run, so a test does not fetch it in the middle of a lane.
const Image = "ghcr.io/cybermonklabs/legion/ci@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11"

// databaseURLEnv names the Postgres server a test may be given instead of starting one. It is
// spelled here rather than taken from internal/dispatch/store/storetest, which declares the same
// name as DatabaseURLEnv: importing that package would pull Dispatch's whole store, and with it
// its dependency tree, into every module that builds this one — among them packages/envoy/infra,
// whose go.work.sum would grow for a test helper's one string. The two must stay in step; they
// are one variable with one meaning, "the Postgres server this lane provides".
const databaseURLEnv = "DISPATCH_TEST_DATABASE_URL"

// URL returns the loopback connection URL of an empty Postgres database of this test's own,
// dropped when the test ends. It is reached two ways.
//
// With DISPATCH_TEST_DATABASE_URL set, the database is created on the server that variable names
// — the Postgres service the envoy-go job of .github/workflows/ci.yaml already runs — and
// dropped from it afterwards. That variable is
// storetest's own, reused rather than doubled: it is how every Postgres-backed Dispatch test is
// already told which server CI provides, and a second name would have to be set beside it on every
// job and in every developer's shell for the same answer. Its database path is ignored exactly as
// storetest ignores it: the admin connection uses the postgres database.
//
// With it unset, Image is started as a container and the whole server ends with the test. A
// developer with a container runtime and no server therefore needs no setup, and a lane that has
// a server needs no runtime.
func URL(t testing.TB) string {
	t.Helper()
	if server := os.Getenv(databaseURLEnv); server != "" {
		return databaseOn(t, server)
	}
	return containerURL(t)
}

// databaseOn creates a database of this test's own on the server raw names, and drops it when the
// test ends.
func databaseOn(t testing.TB, raw string) string {
	t.Helper()
	server, err := url.Parse(raw)
	if err != nil {
		// url.Parse's own error quotes the URL it was given, so only the error it wraps is printed.
		t.Fatalf("parse %s: %v", databaseURLEnv, errors.Unwrap(err))
	}
	return DatabaseOn(t, databaseURL(*server, "postgres"), "testpg")
}

// DatabaseOn creates a database of this test's own on the server adminURL names, drops it when the
// test ends, and returns adminURL with its path replaced by that database and its query kept. The
// name is prefix, an underscore and eight random bytes in hex: it is random because every test of
// every package sharing one server takes a name there.
//
// Both statements run on adminURL itself, which is exact for every caller because a caller passes
// the URL its own statements run on, and that URL and the result differ only in the path.
//
// A failure names the server's host and the database, never adminURL: a DSN carries a credential,
// and a test here prints none (docs/solutions/testing/a-proof-script-never-prints-a-dsn-a-userinfo-scrubber-leaks-the-query-and-keyword-password-forms-INFRA-229.md).
// This function reads no environment variable and starts no container, so its failures can name
// neither.
func DatabaseOn(t testing.TB, adminURL, prefix string) string {
	t.Helper()
	server, err := url.Parse(adminURL)
	if err != nil {
		// url.Parse's own error quotes the URL it was given, so only the error it wraps is printed.
		t.Fatalf("parse the admin URL of the Postgres server: %v", errors.Unwrap(err))
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random database name: %v", err)
	}
	name := prefix + "_" + hex.EncodeToString(suffix[:])
	// A copy of template1's files: created the default way, through the write-ahead log, an empty
	// database costs 4.4 MB of log (docs/testing.md, "A database a Go test creates").
	adminExec(t, adminURL, server.Host, "create database "+name+" strategy file_copy")
	// `with (force)` ends every backend attached to the database, which the drop needs because a
	// database a connection still holds cannot be dropped. That covers a connection the test left
	// open, so one leaked pool cannot keep a database on a shared server for the rest of the run.
	t.Cleanup(func() { adminExec(t, adminURL, server.Host, "drop database "+name+" with (force)") })
	return databaseURL(*server, name)
}

// adminExec runs one statement over a connection of its own to adminURL: neither create database
// nor drop database may run inside a transaction or against the database it names. host is that
// server's host, printed in place of adminURL, which carries a credential.
func adminExec(t testing.TB, adminURL, host, statement string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to the Postgres server at %s: %v", host, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, statement); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

func databaseURL(server url.URL, database string) string {
	server.Path = "/" + database
	return server.String()
}

// containerURL starts Image, waits for Postgres to finish its first initialization and be ready
// again, and returns its postgres database's loopback connection URL. The container ends with t.
func containerURL(t testing.TB) string {
	t.Helper()
	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        Image,
			ExposedPorts: []string{"5432/tcp"},
			Env:          map[string]string{"POSTGRES_PASSWORD": "test"},
			// The data directory's parent on tmpfs leaves no anonymous volume behind (the image
			// declares one at /var/lib/postgresql, and its PostgreSQL 18 entrypoint refuses a mount at
			// the old /var/lib/postgresql/data), and with it the server fits in two CPUs and 2 GiB, as
			// every container the repository's tests start is capped on a shared host; tmpfs counts
			// against that memory.
			Tmpfs: map[string]string{"/var/lib/postgresql": "rw"},
			HostConfigModifier: func(hostConfig *container.HostConfig) {
				hostConfig.NanoCPUs = 2_000_000_000
				hostConfig.Memory = 2 << 30
			},
			WaitingFor: wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, container)
	if err != nil {
		t.Fatalf("start Postgres: %v", err)
	}
	endpoint, err := container.PortEndpoint(ctx, "5432/tcp", "")
	if err != nil {
		t.Fatalf("Postgres endpoint: %v", err)
	}
	return "postgres://postgres:test@" + endpoint + "/postgres?sslmode=disable"
}
