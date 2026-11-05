package testpg_test

import (
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Every test of this module that makes a database of its own drops it with `with (force)`, which
// terminates whatever is still attached to it — an autovacuum worker, most often. Terminating a
// backend of another role needs pg_signal_backend or the superuser, so a server whose login holds
// neither fails those drops with `permission denied to terminate process` (SQLSTATE 42501), at a
// different handful of tests each run, and only where that server is: CI's login is a superuser,
// so CI stays green while every Legion issue pod goes red. This holds the right the drop needs on
// whatever server LEGION_TEST_PG_DSN names, which is the one the tests will use.
func TestTheDSNRoleMayTerminateAnotherRolesBackend(t *testing.T) {
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LEGION_TEST_PG_DSN is unset, so there is no server whose login to read")
	}
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect to the test server: %v", err)
	}
	defer conn.Close(t.Context())

	var mayTerminate bool
	if err := conn.QueryRow(t.Context(),
		`select pg_has_role(current_user, 'pg_signal_backend', 'usage')
			or coalesce((select rolsuper from pg_roles where rolname = current_user), false)`,
	).Scan(&mayTerminate); err != nil {
		t.Fatalf("read whether the login may terminate a backend: %v", err)
	}
	if !mayTerminate {
		var login string
		if err := conn.QueryRow(t.Context(), "select current_user").Scan(&login); err != nil {
			login = "the DSN's login"
		}
		t.Fatalf("%s is no superuser and does not hold pg_signal_backend, so `drop database … with (force)` fails for it with SQLSTATE 42501; "+
			"in a Legion issue pod the server is the pod's own and packages/daemon/docker/legion-testpg is where the grant belongs", login)
	}
}
