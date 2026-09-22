package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// testDSN is the devbox and CI Postgres the store tests run against. Without it
// there is nothing to test: the store has no in-memory mode by design.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LEGION_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LEGION_TEST_PG_DSN is unset, so there is no Postgres to test against. Start one and set it:\n" +
			"  docker run -d --name legion-pg -e POSTGRES_USER=legion -e POSTGRES_PASSWORD=legion -e POSTGRES_DB=legion -p 127.0.0.1::5432 postgres:16\n" +
			"  port=$(docker port legion-pg 5432/tcp | head -1 | sed 's/.*://')\n" +
			"  LEGION_TEST_PG_DSN=postgres://legion:legion@127.0.0.1:$port/legion go test ./internal/store/")
	}
	return dsn
}

// emptyStore opens a store on a database of its own, so "an empty database" in a
// test means one, and tests never see each other's rows.
func emptyStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()

	base, err := url.Parse(testDSN(t))
	if err != nil {
		t.Fatalf("parse LEGION_TEST_PG_DSN: %v", err)
	}
	adminURL := *base
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(ctx, adminURL.String())
	if err != nil {
		t.Fatalf("connect to the admin database: %v", err)
	}
	t.Cleanup(admin.Close)

	name := "legion_store_test_" + randomSuffix(t)
	if _, err := admin.Exec(ctx, "create database "+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+name+" with (force)"); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})

	testURL := *base
	testURL.Path = "/" + name
	store, err := Open(ctx, testURL.String())
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(store.Close)
	return store
}

// migratedStore is an empty database with the schema applied.
func migratedStore(t *testing.T) *Store {
	t.Helper()
	store := emptyStore(t)
	if _, err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	return hex.EncodeToString(b[:])
}

func TestOpenNamesTheUnreachableAddressAndNotThePassword(t *testing.T) {
	_, err := Open(context.Background(), "postgres://legion:hunter2@127.0.0.1:1/legion")
	if err == nil {
		t.Fatal("expected an error connecting to 127.0.0.1:1, got none")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("error does not name the host:port it failed to reach: %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks the DSN password: %v", err)
	}
}

// The driver's own text is not ours to trust: whatever it says on a future
// version, the password the daemon was configured with does not reach a log.
func TestConnectErrorRedactsThePasswordOutOfWhatItWraps(t *testing.T) {
	for _, detail := range []string{
		"failed to connect to `host=db.internal password=hunter2`",
		"cannot parse postgres://legion:hunter2@db.internal:5432/legion",
	} {
		err := &connectError{address: "db.internal:5432", password: "hunter2", err: errors.New(detail)}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("error leaks the password its driver printed: %v", err)
		}
		if !strings.Contains(err.Error(), "db.internal:5432") {
			t.Errorf("error does not name the host:port it failed to reach: %v", err)
		}
	}
}

// postgres://legion:legion@… is the ordinary devbox DSN: the password equals
// the user and the database name. Redaction that blanked every occurrence
// would hide the two fields an operator reads to diagnose the refusal.
func TestConnectErrorKeepsTheFieldsThatMerelyMatchThePassword(t *testing.T) {
	err := &connectError{
		address:  "127.0.0.1:5432",
		password: "legion",
		err:      errors.New("failed to connect to `user=legion database=legion`: connection refused"),
	}
	if !strings.Contains(err.Error(), "user=legion database=legion") {
		t.Errorf("error hides the user and database the daemon was connecting as: %v", err)
	}
}

func TestSchemaVersionIsZeroBeforeAnyMigration(t *testing.T) {
	version, err := emptyStore(t).SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("schema version on an empty database: %v", err)
	}
	if version != 0 {
		t.Errorf("schema version on an empty database = %d, want 0", version)
	}
}

// latestMigration is the highest embedded migration and how many there are: what a fresh
// database is brought to, whatever stage added the newest file.
func latestMigration(t *testing.T) (count, version int) {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	return len(all), all[len(all)-1].Version
}

func TestMigrateAppliesEveryEmbeddedMigration(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	count, latest := latestMigration(t)

	applied, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if applied != count {
		t.Errorf("migrate applied %d migrations, want all %d", applied, count)
	}
	version, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version != latest {
		t.Errorf("schema version = %d, want %d", version, latest)
	}
}

func TestMigrateAppliesNothingASecondTime(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	_, latest := latestMigration(t)

	applied, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if applied != 0 {
		t.Errorf("second migrate applied %d migrations, want 0", applied)
	}
	version, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version != latest {
		t.Errorf("schema version after a second migrate = %d, want %d", version, latest)
	}
}

func TestBootsCountsTheProjectsBootsFromItsFirst(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	first := time.Now().UTC().Truncate(time.Microsecond)

	if _, err := store.RecordBoot(ctx, "WIDGETS", first.Add(-time.Hour)); err != nil {
		t.Fatalf("record another project's boot: %v", err)
	}
	firstID, err := store.RecordBoot(ctx, "LEGION", first)
	if err != nil {
		t.Fatalf("record first boot: %v", err)
	}
	secondID, err := store.RecordBoot(ctx, "LEGION", first.Add(time.Minute))
	if err != nil {
		t.Fatalf("record second boot: %v", err)
	}
	if firstID == secondID {
		t.Errorf("both boots got id %d, want one row each", firstID)
	}

	count, firstBootAt, err := store.Boots(ctx, "LEGION")
	if err != nil {
		t.Fatalf("boots: %v", err)
	}
	if count != 2 {
		t.Errorf("boots count = %d, want 2", count)
	}
	if !firstBootAt.Equal(first) {
		t.Errorf("first boot at = %s, want %s", firstBootAt, first)
	}
}

func TestBootsIsZeroForAProjectThatNeverBooted(t *testing.T) {
	count, firstBootAt, err := migratedStore(t).Boots(context.Background(), "LEGION")
	if err != nil {
		t.Fatalf("boots: %v", err)
	}
	if count != 0 {
		t.Errorf("boots count = %d, want 0", count)
	}
	if !firstBootAt.IsZero() {
		t.Errorf("first boot at = %s, want the zero time", firstBootAt)
	}
}

func TestStopBootStampsTheBootItNames(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	started := time.Now().UTC().Truncate(time.Microsecond)

	id, err := store.RecordBoot(ctx, "LEGION", started)
	if err != nil {
		t.Fatalf("record boot: %v", err)
	}
	running, err := store.RecordBoot(ctx, "LEGION", started)
	if err != nil {
		t.Fatalf("record second boot: %v", err)
	}
	stopped := started.Add(90 * time.Second)
	if err := store.StopBoot(ctx, id, stopped); err != nil {
		t.Fatalf("stop boot: %v", err)
	}

	var stoppedAt *time.Time
	if err := store.pool.QueryRow(ctx, "select stopped_at from daemon_boot where id = $1", id).Scan(&stoppedAt); err != nil {
		t.Fatalf("read stopped boot: %v", err)
	}
	if stoppedAt == nil || !stoppedAt.Equal(stopped) {
		t.Errorf("stopped_at = %v, want %s", stoppedAt, stopped)
	}

	var otherStoppedAt *time.Time
	if err := store.pool.QueryRow(ctx, "select stopped_at from daemon_boot where id = $1", running).Scan(&otherStoppedAt); err != nil {
		t.Fatalf("read running boot: %v", err)
	}
	if otherStoppedAt != nil {
		t.Errorf("stopped_at of the still-running boot = %s, want null", otherStoppedAt)
	}
}

func TestStopBootRefusesAnUnknownBoot(t *testing.T) {
	err := migratedStore(t).StopBoot(context.Background(), 4242, time.Now().UTC())
	if err == nil {
		t.Fatal("expected an error stopping boot 4242, got none")
	}
	if !strings.Contains(err.Error(), "4242") {
		t.Errorf("error does not name the boot it could not stop: %v", err)
	}
}

func TestTxCommitsWhatTheFunctionWrote(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)

	if err := store.Tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "insert into daemon_boot (project, started_at) values ($1, now())", "LEGION")
		return err
	}); err != nil {
		t.Fatalf("tx: %v", err)
	}

	count, _, err := store.Boots(ctx, "LEGION")
	if err != nil {
		t.Fatalf("boots: %v", err)
	}
	if count != 1 {
		t.Errorf("boots count = %d, want the committed row", count)
	}
}

func TestTxRollsBackWhatTheFunctionWroteBeforeFailing(t *testing.T) {
	ctx := context.Background()
	store := migratedStore(t)
	failure := errors.New("the phase gave up")

	err := store.Tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "insert into daemon_boot (project, started_at) values ($1, now())", "LEGION"); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("tx error = %v, want the function's own error", err)
	}

	count, _, err := store.Boots(ctx, "LEGION")
	if err != nil {
		t.Fatalf("boots: %v", err)
	}
	if count != 0 {
		t.Errorf("boots count = %d, want 0 after a rolled-back transaction", count)
	}
}
