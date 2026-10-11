package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/pgmigrate"
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

// TestMigrationSevenKeysLiveEnrollmentsBySlot applies 0001-0006 by hand inside an isolated schema
// (dropped on rollback, as TestMigrationFiveHandlesPreexistingLauncherCredentialsRow does), writes
// a live box and a live pod enrollment the way a broker before slots did, then runs 0007: both
// rows keep their identity with the empty slot, a second slot of the same pod is a live row of its
// own, and a second live row in one slot is still refused, by the new index and not the old one.
func TestMigrationSevenKeysLiveEnrollmentsBySlot(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, testDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Pool.Close()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `create schema migration_seven_slot`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := tx.Exec(ctx, `set local search_path to migration_seven_slot`); err != nil {
		t.Fatalf("set search_path: %v", err)
	}
	apply := func(version string) {
		t.Helper()
		sql, err := migrationFiles.ReadFile("migrations/" + version)
		if err != nil {
			t.Fatalf("read %s: %v", version, err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", version, err)
		}
	}
	for _, version := range []string{
		"0001_init.up.sql",
		"0002_launcher_request_service.up.sql",
		"0003_enrollment_subject.up.sql",
		"0004_ask_retraction.up.sql",
		"0005_credential_requests.up.sql",
		"0006_approval_by_login.up.sql",
	} {
		apply(version)
	}
	const credential = "00000000-0000-0000-0000-0000000000c1"
	if _, err := tx.Exec(ctx, `insert into launcher_credentials (id, service, host, key_thumbprint, public_jwk, expires_at)
		values ($1, 'legion-daemon', 'cluster', 'tp-launcher', '{}'::jsonb, now() + interval '1 day')`, credential); err != nil {
		t.Fatalf("insert launcher credential: %v", err)
	}
	enrol := func(sp pgx.Tx, id, kind, runtimeID, thumbprint string, slot *string) error {
		columns, values, args := "", "", []any{id, kind, runtimeID, thumbprint, credential}
		if slot != nil {
			columns, values, args = ", slot", ", $6", append(args, *slot)
		}
		_, err := sp.Exec(ctx, `insert into enrollments (id, kind, runtime_id, thumbprint, launcher_credential_id, lease_expires_at`+columns+`)
			values ($1, $2, $3, $4, $5, now() + interval '1 hour'`+values+`)`, args...)
		return err
	}
	if err := enrol(tx, "00000000-0000-0000-0000-0000000000e1", "box", "box-1", "tp-box", nil); err != nil {
		t.Fatalf("insert pre-slot box enrollment: %v", err)
	}
	if err := enrol(tx, "00000000-0000-0000-0000-0000000000e2", "pod", "pod-uid-1", "tp-pod", nil); err != nil {
		t.Fatalf("insert pre-slot pod enrollment: %v", err)
	}

	apply("0007_enrollment_slot.up.sql")

	var empty int
	if err := tx.QueryRow(ctx, `select count(*) from enrollments where slot = ''`).Scan(&empty); err != nil || empty != 2 {
		t.Fatalf("pre-slot enrollments with the empty slot = %d (%v), want both", empty, err)
	}
	var indexes []string
	rows, err := tx.Query(ctx, `select indexname from pg_indexes where schemaname = 'migration_seven_slot' and tablename = 'enrollments' order by indexname`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		indexes = append(indexes, name)
	}
	rows.Close()
	if strings.Join(indexes, ",") != "enrollments_live_runtime_slot,enrollments_pkey" {
		t.Fatalf("enrollments indexes = %v, want the slot index and the primary key alone", indexes)
	}

	inSavepoint := func(insert func(sp pgx.Tx) error) error {
		t.Helper()
		sp, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer sp.Rollback(ctx)
		if err := insert(sp); err != nil {
			return err
		}
		return sp.Commit(ctx)
	}
	implementer, reviewer := "implementer-g1", "reviewer-g1"
	for id, slot := range map[string]*string{"00000000-0000-0000-0000-0000000000a1": &implementer, "00000000-0000-0000-0000-0000000000a2": &reviewer} {
		if err := inSavepoint(func(sp pgx.Tx) error { return enrol(sp, id, "pod", "pod-uid-1", "tp-"+*slot, slot) }); err != nil {
			t.Fatalf("second live enrollment of pod-uid-1 in slot %s: %v", *slot, err)
		}
	}
	for name, slot := range map[string]*string{"the empty slot": nil, "slot implementer-g1": &implementer} {
		err := inSavepoint(func(sp pgx.Tx) error {
			return enrol(sp, "00000000-0000-0000-0000-0000000000ff", "pod", "pod-uid-1", "tp-other", slot)
		})
		if err == nil || !strings.Contains(err.Error(), "enrollments_live_runtime_slot") {
			t.Fatalf("a second live row of pod-uid-1 in %s = %v, want a violation of enrollments_live_runtime_slot", name, err)
		}
	}
	if _, err := tx.Exec(ctx, `update enrollments set revoked_at = now() where slot = $1`, implementer); err != nil {
		t.Fatal(err)
	}
	if err := inSavepoint(func(sp pgx.Tx) error {
		return enrol(sp, "00000000-0000-0000-0000-0000000000fe", "pod", "pod-uid-1", "tp-implementer-again", &implementer)
	}); err != nil {
		t.Fatalf("re-enrolling a revoked slot: %v", err)
	}
}

// migrationSchema opens a transaction with search_path set to a fresh schema of its own, dropped
// on rollback, and applies to it every embedded migration before version below, as a broker of
// that time left the database; apply then applies one more by its file name.
func migrationSchema(t *testing.T, schema string, below int) (tx pgx.Tx, apply func(file string)) {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, testDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Pool.Close)
	tx, err = s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, `create schema `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := tx.Exec(ctx, `set local search_path to `+schema); err != nil {
		t.Fatalf("set search_path: %v", err)
	}
	apply = func(file string) {
		t.Helper()
		sql, err := migrationFiles.ReadFile("migrations/" + file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", file, err)
		}
	}
	migrations, err := pgmigrate.Load(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.Version < below {
			apply(m.Name)
		}
	}
	return tx, apply
}

// TestMigrationTwelveMakesEveryMachineSessionOneKind writes a live box, host and pod enrollment
// the way a broker before 0012 did, then runs 0012: the box and host rows read as kind session,
// the pod row stays pod, and box and host are refused from then on.
func TestMigrationTwelveMakesEveryMachineSessionOneKind(t *testing.T) {
	ctx := context.Background()
	tx, apply := migrationSchema(t, "migration_twelve_kind", 12)
	const credential = "00000000-0000-0000-0000-0000000000c1"
	if _, err := tx.Exec(ctx, `insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, expires_at)
		values ($1, 'ada@example.com', 'example-host-devbox', 'tp-launcher', '{}'::jsonb, now() + interval '1 day')`, credential); err != nil {
		t.Fatalf("insert launcher credential: %v", err)
	}
	enrol := func(sp pgx.Tx, id, kind string) error {
		_, err := sp.Exec(ctx, `insert into enrollments (id, kind, runtime_id, thumbprint, launcher_credential_id, lease_expires_at)
			values ($1, $2, $3, $4, $5, now() + interval '1 hour')`, id, kind, kind+"-1", "tp-"+id, credential)
		return err
	}
	rows := map[string]string{
		"00000000-0000-0000-0000-0000000000e1": "box",
		"00000000-0000-0000-0000-0000000000e2": "host",
		"00000000-0000-0000-0000-0000000000e3": "pod",
	}
	for id, kind := range rows {
		if err := enrol(tx, id, kind); err != nil {
			t.Fatalf("insert %s enrollment before 0012: %v", kind, err)
		}
	}

	apply("0012_one_session_kind.up.sql")

	want := map[string]string{
		"00000000-0000-0000-0000-0000000000e1": "session",
		"00000000-0000-0000-0000-0000000000e2": "session",
		"00000000-0000-0000-0000-0000000000e3": "pod",
	}
	for id, kind := range want {
		var got string
		if err := tx.QueryRow(ctx, `select kind from enrollments where id = $1`, id).Scan(&got); err != nil || got != kind {
			t.Fatalf("enrollment %s (was %s) after 0012 = %q (%v), want %q", id, rows[id], got, err, kind)
		}
	}
	for _, kind := range []string{"box", "host"} {
		sp, err := tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = enrol(sp, "00000000-0000-0000-0000-0000000000f"+fmt.Sprint(len(kind)), kind)
		_ = sp.Rollback(ctx)
		if err == nil || !strings.Contains(err.Error(), "enrollments_kind_check") {
			t.Fatalf("a %s enrollment after 0012 = %v, want a violation of enrollments_kind_check", kind, err)
		}
	}
	if err := enrol(tx, "00000000-0000-0000-0000-0000000000e4", "session"); err != nil {
		t.Fatalf("a session enrollment after 0012: %v", err)
	}
}

// TestMigrationThirteenLetsAGrantLastUntilItsSessionEnds runs 0013 over a database 0012 left: a
// request may name no lifetime and its grant no expiry, the shape of a grant that lasts until its
// session ends, while a grant written with an expiry keeps it.
func TestMigrationThirteenLetsAGrantLastUntilItsSessionEnds(t *testing.T) {
	ctx := context.Background()
	tx, apply := migrationSchema(t, "migration_thirteen_lifetime", 13)
	const credential, enrollment = "00000000-0000-0000-0000-0000000000c1", "00000000-0000-0000-0000-0000000000e1"
	if _, err := tx.Exec(ctx, `insert into launcher_credentials (id, operator, host, key_thumbprint, public_jwk, expires_at)
		values ($1, 'ada@example.com', 'example-host-devbox', 'tp-launcher', '{}'::jsonb, now() + interval '1 day')`, credential); err != nil {
		t.Fatalf("insert launcher credential: %v", err)
	}
	if _, err := tx.Exec(ctx, `insert into enrollments (id, kind, runtime_id, thumbprint, launcher_credential_id, lease_expires_at)
		values ($1, 'session', 'session-1', 'tp-session', $2, now() + interval '1 hour')`, enrollment, credential); err != nil {
		t.Fatalf("insert enrollment: %v", err)
	}
	insert := func(request string, lifetime *int) error {
		if _, err := tx.Exec(ctx, `insert into requests (id, enrollment_id, reason, state, rules_version, lifetime_seconds)
			values ($1, $2, 'why', 'granted', 'v1', $3)`, request, enrollment, lifetime); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `insert into grants (id, request_id, enrollment_id, expires_at)
			values (gen_random_uuid(), $1, $2, now() + make_interval(secs => $3))`, request, enrollment, lifetime)
		return err
	}
	hour := 3600
	if err := insert("00000000-0000-0000-0000-0000000000a1", &hour); err != nil {
		t.Fatalf("a request with a lifetime before 0013: %v", err)
	}

	apply("0013_grant_until_session_ends.up.sql")

	if err := insert("00000000-0000-0000-0000-0000000000a2", nil); err != nil {
		t.Fatalf("a request with no lifetime and a grant with no expiry after 0013: %v", err)
	}
	var withExpiry, withoutExpiry int
	if err := tx.QueryRow(ctx, `select count(*) filter (where expires_at is not null), count(*) filter (where expires_at is null) from grants`).
		Scan(&withExpiry, &withoutExpiry); err != nil || withExpiry != 1 || withoutExpiry != 1 {
		t.Fatalf("grants after 0013: %d with an expiry, %d without (%v), want one of each", withExpiry, withoutExpiry, err)
	}
}
