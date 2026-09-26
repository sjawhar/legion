package store

import (
	"context"
	"os"
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
		('launcher_credentials','launcher_credential_requests','enrollments','requests','request_secrets','grants','proof_jtis','audit')`).Scan(&n)
	if err != nil || n != 8 {
		t.Fatalf("expected 8 tables, got %d (%v)", n, err)
	}
}
