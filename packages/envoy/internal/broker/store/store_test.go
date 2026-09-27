package store

import (
	"context"
	"os"
	"strings"
	"testing"
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
		 'credential_requests','credential_request_events','approver_keys','approver_key_seeds',
		 'webauthn_ceremonies','machine_login_polls')`).Scan(&n)
	if err != nil || n != 13 {
		t.Fatalf("expected 13 tables, got %d (%v)", n, err)
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
