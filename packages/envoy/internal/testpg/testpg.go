// Package testpg gives a test that brings its own servers, as a natslive test does with testnats,
// an empty Postgres database and the URL to hand a binary. The caller migrates it itself. A test
// of Dispatch's store against a server CI provides uses internal/dispatch/store/storetest instead,
// which hands back a migrated store rather than a URL.
package testpg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/testcaps"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image is the pgvector server image CI uses for Dispatch's Postgres service. The envoy-infra-go
// job pulls it before the natslive tests run, so a test does not fetch it in the middle of a lane.
const Image = "mirror.gcr.io/pgvector/pgvector@sha256:7b822b0aac60967beb1ea5e576b8602c94c300a157d187f385ae3e0da199b90a"

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
// — the Postgres service the envoy-go job already runs
// (.github/workflows/envoy-and-contracts.yaml) — and dropped from it afterwards. That variable is
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
// test ends. The name is random because every test of every package sharing that server takes one.
func databaseOn(t testing.TB, raw string) string {
	t.Helper()
	server, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", databaseURLEnv, err)
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random database name: %v", err)
	}
	name := "testpg_" + hex.EncodeToString(suffix[:])
	admin := databaseURL(*server, "postgres")
	adminExec(t, admin, "create database "+name)
	// `with (force)` ends any connection the test left open, so one leaked pool cannot keep a
	// database on a shared server for the rest of the run.
	t.Cleanup(func() { adminExec(t, admin, "drop database "+name+" with (force)") })
	return databaseURL(*server, name)
}

// adminExec runs one statement on the server's postgres database over a connection of its own:
// neither create database nor drop database may run inside a transaction or against the database
// it names.
func adminExec(t testing.TB, admin, statement string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect to the Postgres %s names: %v", databaseURLEnv, err)
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
// again, and returns its postgres database's loopback connection URL. The container ends with t,
// and is capped at two cores and 2 GiB (internal/testcaps).
func containerURL(t testing.TB) string {
	t.Helper()
	ctx := context.Background()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:              Image,
			ExposedPorts:       []string{"5432/tcp"},
			Env:                map[string]string{"POSTGRES_PASSWORD": "test"},
			WaitingFor:         wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
			HostConfigModifier: testcaps.Cap(2, 2<<30),
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
