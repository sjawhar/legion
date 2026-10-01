package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/record"
)

func testDatabaseURL(t *testing.T) string {
	url := os.Getenv("BROKER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("BROKER_TEST_DATABASE_URL must be set to run Postgres store tests")
	}
	return url
}

func TestMigrateIsIdempotentAndCreatesTables(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, testDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool.Close()
	for range 2 {
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	err = s.Pool.QueryRow(ctx, `select count(*) from information_schema.tables where table_schema='public' and table_name in
		('launcher_credentials','enrollments','requests','request_secrets','grants','proof_jtis','audit',
		 'credential_requests','credential_request_events','machine_login_polls')`).Scan(&n)
	if err != nil || n != 10 {
		t.Fatalf("expected 10 tables, got %d (%v)", n, err)
	}
	// Migration 0006: approval by Dispatch login keeps no approver keys, seeds or ceremonies.
	err = s.Pool.QueryRow(ctx, `select count(*) from information_schema.tables where table_schema='public' and table_name in
		('approver_keys','approver_key_seeds','webauthn_ceremonies')`).Scan(&n)
	if err != nil || n != 0 {
		t.Fatalf("expected the approver key tables dropped, found %d (%v)", n, err)
	}
}

// TestTerminalEventNamesMatchTheDecisionIndex pins record.TerminalEventNames to the predicate of
// credential_request_decision, the partial unique index that holds a record to one terminal
// event, so the Go list and the index cannot drift apart. It compares the whole deparsed
// predicate Postgres reports, built fresh from the Go list, rather than extracting the quoted
// literals out of it: a predicate reshaped around the same literals (`event not in (...)`, or one
// naming an unrelated column such as `actor in (...)`) must fail this test on its own, not only
// the sweeper and lock tests that happen to depend on the real predicate.
func TestTerminalEventNamesMatchTheDecisionIndex(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, testDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var definition string
	if err := s.Pool.QueryRow(ctx, `select indexdef from pg_indexes where schemaname='public' and indexname='credential_request_decision'`).
		Scan(&definition); err != nil {
		t.Fatalf("read credential_request_decision: %v", err)
	}
	_, predicate, found := strings.Cut(definition, " WHERE ")
	if !found {
		t.Fatalf("credential_request_decision = %q, want a partial index", definition)
	}
	names := record.TerminalEventNames()
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("'%s'::text", name)
	}
	want := fmt.Sprintf("(event = ANY (ARRAY[%s]))", strings.Join(quoted, ", "))
	if predicate != want {
		t.Fatalf("credential_request_decision's predicate = %q, want %q (from record.TerminalEventNames() = %v)", predicate, want, names)
	}
}

func TestMigrationFiveShapesTheRecordTables(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, testDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// The whole test runs inside one transaction that is always rolled back, never committed, so
	// the fixed record id below can never collide with a leftover row from a prior run against the
	// same long-lived database. Each statement expected to fail runs inside its own savepoint
	// (tx.Begin on a pgx.Tx opens a SAVEPOINT), since a failed statement would otherwise abort the
	// whole outer transaction and every later statement.
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	const recordID = "deadbeef00000000000000000000000000000000000000000000000000000000"
	if _, err := tx.Exec(ctx, `insert into credential_requests (id, body, kind, approver, expires_at)
		values ($1, 'body', 'agent_secret', 'alice', now() + interval '1 hour')`, recordID); err != nil {
		t.Fatalf("insert credential_requests: %v", err)
	}

	func() {
		sp, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer sp.Rollback(ctx)
		if _, err := sp.Exec(ctx, `update credential_requests set code='x' where id=$1`, recordID); err == nil {
			t.Fatal("expected update on credential_requests to fail")
		} else if !strings.Contains(err.Error(), "credential requests are append-only") {
			t.Fatalf("expected append-only error, got %v", err)
		}
	}()

	if _, err := tx.Exec(ctx, `insert into credential_request_events (record_id, event, actor) values ($1, 'approved', 'alice')`, recordID); err != nil {
		t.Fatalf("insert first decision event: %v", err)
	}

	func() {
		sp, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer sp.Rollback(ctx)
		if _, err := sp.Exec(ctx, `insert into credential_request_events (record_id, event, actor) values ($1, 'denied', 'alice')`, recordID); err == nil {
			t.Fatal("expected second decision event to violate credential_request_decision")
		} else if !strings.Contains(err.Error(), "credential_request_decision") {
			t.Fatalf("expected unique-index violation naming credential_request_decision, got %v", err)
		}
	}()
}

func TestMigrationFiveHandlesPreexistingLauncherCredentialsRow(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, testDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool.Close()

	// Migration 0005 adds NOT NULL columns to launcher_credentials, a table migration 0001
	// created, not 0005 itself. Postgres refuses ALTER TABLE ... ADD COLUMN ... NOT NULL with no
	// DEFAULT the instant the table holds even one row - exactly the state every deployed v8
	// launcher credential leaves behind. This applies 0001-0004 by hand inside an isolated
	// schema (dropped on rollback, so it can never collide with the shared long-lived database's
	// own already-migrated public schema), inserts a v8-shaped row against that schema, then
	// runs 0005 and confirms it both succeeds and clears the table - the exact scenario every
	// other test misses, since Migrate always starts from an empty database.
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `create schema migration_five_fix`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := tx.Exec(ctx, `set local search_path to migration_five_fix`); err != nil {
		t.Fatalf("set search_path: %v", err)
	}

	for _, version := range []string{
		"0001_init.up.sql",
		"0002_launcher_request_service.up.sql",
		"0003_enrollment_subject.up.sql",
		"0004_ask_retraction.up.sql",
	} {
		sql, err := migrationFiles.ReadFile("migrations/" + version)
		if err != nil {
			t.Fatalf("read %s: %v", version, err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", version, err)
		}
	}

	if _, err := tx.Exec(ctx, `insert into launcher_credentials (id, host, token_hash) values (gen_random_uuid(), 'launcher.example.com', 'x')`); err != nil {
		t.Fatalf("insert v8-shaped launcher_credentials row: %v", err)
	}

	sql, err := migrationFiles.ReadFile("migrations/0005_credential_requests.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("migration 0005 should succeed against a pre-existing launcher_credentials row: %v", err)
	}

	var n int
	if err := tx.QueryRow(ctx, `select count(*) from launcher_credentials`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected migration 0005 to delete the pre-existing row, got %d remaining", n)
	}
}
