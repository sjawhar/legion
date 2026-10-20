package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
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

// Two branches can each take the next migration number, and they land in either order: a database
// migrated by a daemon with the higher one first has to take the lower one when it arrives. Migrate
// applies every embedded migration the database has not recorded, not only those above its highest,
// so a database at 0001-0040 and 0042 and later takes 0041 — here child_release_retries, a table no
// later migration alters — and ends with every version recorded.
func TestMigrateAppliesALowerMigrationADatabaseAlreadyPastItLacks(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	const skipped = 41
	for _, migration := range all {
		if migration.Version == skipped {
			continue
		}
		if _, err := store.apply(ctx, migration); err != nil {
			t.Fatalf("apply %s: %v", migration.Name, err)
		}
	}
	childReleaseRetries := func() bool {
		t.Helper()
		var exists bool
		if err := store.pool.QueryRow(ctx, "select to_regclass('public.child_release_retries') is not null").Scan(&exists); err != nil {
			t.Fatalf("look for the child_release_retries table: %v", err)
		}
		return exists
	}
	if version, err := store.SchemaVersion(ctx); err != nil || version != all[len(all)-1].Version || childReleaseRetries() {
		t.Fatalf("before Migrate: schema version %d (%v), child_release_retries table %t; want the latest, and no table", version, err, childReleaseRetries())
	}

	applied, err := store.Migrate(ctx)

	if err != nil || applied != 1 {
		t.Fatalf("migrate = %d, %v; want the one missing migration applied", applied, err)
	}
	if !childReleaseRetries() {
		t.Error("migration 0041 was recorded as applied, but the child_release_retries table does not exist")
	}
	var recorded []int
	rows, err := store.pool.Query(ctx, "select version from schema_version order by version")
	if err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			t.Fatalf("scan schema_version: %v", err)
		}
		recorded = append(recorded, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read schema_version: %v", err)
	}
	want := make([]int, len(all))
	for i, migration := range all {
		want[i] = migration.Version
	}
	if !slices.Equal(recorded, want) {
		t.Errorf("schema_version holds %v, want every embedded version %v", recorded, want)
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

// migrateThrough applies every embedded migration up to and including version, as an older
// daemon's database stands.
func migrateThrough(t *testing.T, store *Store, version int) {
	t.Helper()
	ctx := context.Background()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	for _, migration := range all {
		if migration.Version > version {
			return
		}
		if _, err := store.apply(ctx, migration); err != nil {
			t.Fatalf("apply %s: %v", migration.Name, err)
		}
	}
}

// A daemon upgraded in place has workers holding tasks delivered before the delivery carried its
// run. A completion attributed to no run is refused, so those phases would stall until every
// worker happened to be handed a new task: 0011 gives each such delivery the run its issue is on,
// and leaves a claim on an issue no workflow records — an operator's own (LEGION-272) — at zero.
func TestTheDeliveryGenerationBackfillsFromTheIssuesRun(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, 10)

	for _, row := range []struct{ token, role, issue string }{
		{token: "tok-tester", role: "tester", issue: "LEGION-208"},
		{token: "tok-operator", role: "implementer", issue: "LEGION-UNRECORDED"},
	} {
		if _, err := store.pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session,
			session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'LEGION', 'LEGION-208', $2, $3, 1, '', '', 'working', 0, 0, 0, 0)`,
			row.token, row.issue, row.role); err != nil {
			t.Fatalf("seed the %s claim: %v", row.role, err)
		}
		if _, err := store.pool.Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at, confirmed_at)
			values ($1, 'delivery-'||$1, 'the task', now(), now())`, row.token); err != nil {
			t.Fatalf("seed the %s delivery: %v", row.role, err)
		}
	}
	if _, err := store.pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'LEGION-208', 'testing', 3, 'in_progress', 'V', 0)`); err != nil {
		t.Fatalf("seed the issue: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate the rest: %v", err)
	}

	for _, want := range []struct {
		token      string
		generation int64
	}{
		{token: "tok-tester", generation: 3},
		{token: "tok-operator", generation: 0},
	} {
		var generation int64
		if err := store.pool.QueryRow(ctx, "select generation from pending_task_deliveries where claim_token = $1",
			want.token).Scan(&generation); err != nil {
			t.Fatalf("read %s's delivery: %v", want.token, err)
		}
		if generation != want.generation {
			t.Fatalf("%s's delivery carries run %d, want %d", want.token, generation, want.generation)
		}
	}
}

// A worker between turns at an in-place upgrade holds no delivery at all, so nothing tells the
// daemon which run it is working and its next completion is refused: 0012 gives each live claim
// the run its issue is on. A claim whose process is gone is left at zero — it is relaunched, and
// the task its new process is given carries its own run — as is one on an issue no workflow
// records.
func TestTheServingRunBackfillsForClaimsBetweenTurns(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, 10)

	for _, row := range []struct{ token, issue, state string }{
		{token: "tok-idle", issue: "LEGION-208", state: "idle"},
		{token: "tok-retired", issue: "LEGION-208", state: "retired"},
		{token: "tok-unrecorded", issue: "LEGION-UNRECORDED", state: "idle"},
	} {
		if _, err := store.pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session,
			session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'LEGION', 'LEGION-208', $2, 'tester', 1, '', '', $3, 0, 0, 0, 0)`,
			row.token, row.issue, row.state); err != nil {
			t.Fatalf("seed the %s claim: %v", row.token, err)
		}
	}
	if _, err := store.pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-208', 'LEGION-208', 'LEGION', 'LEGION-208', 'testing', 5, 'in_progress', 'V', 0)`); err != nil {
		t.Fatalf("seed the issue: %v", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate the rest: %v", err)
	}

	for _, want := range []struct {
		token   string
		serving int64
	}{
		{token: "tok-idle", serving: 5},
		{token: "tok-retired", serving: 0},
		{token: "tok-unrecorded", serving: 0},
	} {
		var serving int64
		if err := store.pool.QueryRow(ctx, "select serving_generation from claims where token = $1",
			want.token).Scan(&serving); err != nil {
			t.Fatalf("read %s: %v", want.token, err)
		}
		if serving != want.serving {
			t.Fatalf("%s serves run %d, want %d", want.token, serving, want.serving)
		}
	}
}

// A tree close and a workspace removal name the root generation of the linger they expire, and
// the outbox decodes rows strictly. 0016 gives each one queued in the older shape (a close naming
// no linger, a removal naming its issue's generation) the generation of its tree's root when that
// root still lingers, so the row goes on to expire that linger, and deletes the rest: their linger
// has ended, and a row that never decodes would fail every tick and every promotion of its tree.
func TestQueuedLingerRowsOfTheOlderShapeNameTheirLingerOrGo(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, 13)

	for _, issue := range []struct {
		key, tree  string
		generation int
		lingers    bool
	}{
		{key: "LEGION-208", tree: "LEGION-208", generation: 4, lingers: true},
		{key: "LEGION-209", tree: "LEGION-208", generation: 1},
		{key: "LEGION-300", tree: "LEGION-300", generation: 2},
		{key: "LEGION-301", tree: "LEGION-300", generation: 1},
	} {
		var lingerUntil *time.Time
		if issue.lingers {
			until := time.Now().Add(time.Hour)
			lingerUntil = &until
		}
		if _, err := store.pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, linger_until, last_dispatch_seq)
			values ($1, $2, 'LEGION', $1, 'testing', $3, 'in_progress', 'V', $4, 0)`, issue.key, issue.tree, issue.generation, lingerUntil); err != nil {
			t.Fatalf("seed %s: %v", issue.key, err)
		}
	}
	type row struct{ kind, issue, payload string }
	for _, seeded := range []row{
		{kind: "supervise", issue: "LEGION-209", payload: `{"op": "tree_close", "tree": "LEGION-208", "role": "tester", "generation": 1}`},
		{kind: "workspace_remove", issue: "LEGION-209", payload: `{"generation": 1}`},
		{kind: "supervise", issue: "LEGION-301", payload: `{"op": "tree_close", "tree": "LEGION-300", "role": "tester", "generation": 1}`},
		{kind: "workspace_remove", issue: "LEGION-301", payload: `{"generation": 1}`},
		{kind: "supervise", issue: "LEGION-301", payload: `{"op": "start", "tree": "LEGION-300", "role": "tester", "generation": 1, "phase": "testing", "task": "Test it."}`},
	} {
		if _, err := store.pool.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error) values ($1, $2, $3, 0, now(), '')`,
			seeded.kind, seeded.issue, seeded.payload); err != nil {
			t.Fatalf("seed the %s row of %s: %v", seeded.kind, seeded.issue, err)
		}
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate the rest: %v", err)
	}

	rows, err := store.pool.Query(ctx, "select id, kind, issue, payload from outbox order by id")
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var queued record.OutboxRow
		var kind string
		if err := rows.Scan(&queued.ID, &kind, &queued.Issue, &queued.Payload); err != nil {
			t.Fatalf("scan an outbox row: %v", err)
		}
		queued.Kind = record.OutboxKind(kind)
		payload, err := record.DecodeOutboxPayload(queued)
		if err != nil {
			t.Fatalf("the migrated %s row of %s does not decode: %v", kind, queued.Issue, err)
		}
		switch value := payload.(type) {
		case record.SuperviseRequest:
			got = append(got, fmt.Sprintf("%s %s linger=%d", queued.Issue, value.Op, value.Linger))
		case record.WorkspaceRemove:
			got = append(got, fmt.Sprintf("%s workspace_remove linger=%d", queued.Issue, value.Linger))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	want := []string{"LEGION-209 tree_close linger=4", "LEGION-209 workspace_remove linger=4", "LEGION-301 start linger=0"}
	if !slices.Equal(got, want) {
		t.Fatalf("outbox after 0016 = %q, want %q", got, want)
	}
}

// A worker whose process died mid-task leaves a claim launching, shim_connected or
// launch_uncertain: the relaunch resumes that session and is given no task, because the claim
// already holds one. 0014 gives such a claim the issue's run. A first launch has no session and
// keeps zero — the task it is given attributes it. Started from 0010 the two backfills run in one
// boot and together cover every live claim; started from 0013, a claim already past relaunching
// with a zero run is out of 0014's reach, which is the limit its header states.
func TestTheServingRunBackfillsForARelaunchingClaim(t *testing.T) {
	for _, tc := range []struct {
		from      int
		wantReady int64
	}{
		{from: 10, wantReady: 5},
		{from: 13, wantReady: 0},
	} {
		t.Run(fmt.Sprintf("from %04d", tc.from), func(t *testing.T) {
			ctx := context.Background()
			store := emptyStore(t)
			migrateThrough(t, store, tc.from)
			for _, row := range []struct{ token, state, session string }{
				{token: "tok-launching", state: "launching", session: "ses-resumed"},
				{token: "tok-shim", state: "shim_connected", session: "ses-resumed"},
				{token: "tok-uncertain", state: "launch_uncertain", session: "ses-resumed"},
				{token: "tok-first", state: "launching", session: ""},
				{token: "tok-retired", state: "retired", session: "ses-resumed"},
				{token: "tok-ready", state: "ready", session: "ses-resumed"},
			} {
				if _, err := store.pool.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session,
					session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
					values ($1, 'LEGION', 'LEGION-208', 'LEGION-208', 'tester', 1, $3, '', $2, 0, 0, 0, 0)`,
					row.token, row.state, row.session); err != nil {
					t.Fatalf("seed the %s claim: %v", row.token, err)
				}
			}
			if _, err := store.pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
				values ('LEGION-208', 'LEGION-208', 'LEGION', 'LEGION-208', 'testing', 5, 'in_progress', 'V', 0)`); err != nil {
				t.Fatalf("seed the issue: %v", err)
			}

			if _, err := store.Migrate(ctx); err != nil {
				t.Fatalf("migrate the rest: %v", err)
			}

			for _, want := range []struct {
				token   string
				serving int64
			}{
				{token: "tok-launching", serving: 5},
				{token: "tok-shim", serving: 5},
				{token: "tok-uncertain", serving: 5},
				{token: "tok-first", serving: 0},
				{token: "tok-retired", serving: 0},
				{token: "tok-ready", serving: tc.wantReady},
			} {
				var serving int64
				if err := store.pool.QueryRow(ctx, "select serving_generation from claims where token = $1",
					want.token).Scan(&serving); err != nil {
					t.Fatalf("read %s: %v", want.token, err)
				}
				if serving != want.serving {
					t.Fatalf("%s serves run %d, want %d", want.token, serving, want.serving)
				}
			}
		})
	}
}

// Every outbox kind the daemon writes is one the schema's check admits, on a fresh database and on
// databases that applied a lower migration late. 0016 (merge_queue_publish) and 0020
// (controller_notice) would each redefine the check with the other's kind missing, and a database
// past 0020 applies the lower 0016 late, where a check without controller_notice would refuse the
// queued row and the upgrade: neither sets the list, 0021 does, after both. In the same way
// `issue_branch` (0030) and `issue_suspend` (0034) each come with a migration that rewrites the
// check, and a database that ran 0031 through 0038 before 0029 and 0030 applies those two last, so
// a queued close suspension must survive them and both kinds must stand at the end in either
// order. Migrations from 0039 on ship only in binaries that carry 0030, and Migrate applies in
// ascending order, so none can run before it: 0043 sets the list after every kind it names.
func TestTheOutboxCheckAdmitsEveryKindWhicheverOrderTheMigrationsRan(t *testing.T) {
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	kinds := []record.OutboxKind{record.OutboxKindDispatchStatus, record.OutboxKindDispatchMessage, record.OutboxKindNotice, record.OutboxKindControllerNotice,
		record.OutboxKindSupervise, record.OutboxKindGateSeed, record.OutboxKindLingerClose, record.OutboxKindWorkspaceRemove, record.OutboxKindMergeQueuePublish,
		record.OutboxKindIssueSuspend, record.OutboxKindIssueBranch, record.OutboxKindAutoMergeDisable, record.OutboxKindReadyMerge}
	insert := "insert into outbox (kind, issue, payload, attempts, next_at, last_error) values ($1, 'LEGION-208', '{}', 0, now(), '')"
	for _, tc := range []struct {
		name  string
		first func(migrations.Migration) bool
	}{
		{"a fresh database", func(migrations.Migration) bool { return false }},
		{"a database that ran 0020 before 0016", func(m migrations.Migration) bool { return m.Version <= 20 && m.Version != 16 }},
		{"a database that ran 0031 through 0038 before 0029 and 0030", func(m migrations.Migration) bool { return m.Version <= 28 || (m.Version >= 31 && m.Version <= 38) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := emptyStore(t)
			for _, migration := range all {
				if !tc.first(migration) {
					continue
				}
				if _, err := store.apply(ctx, migration); err != nil {
					t.Fatalf("apply %s: %v", migration.Name, err)
				}
			}
			version, err := store.SchemaVersion(ctx)
			if err != nil {
				t.Fatalf("read the schema version: %v", err)
			}
			for _, queued := range []struct {
				since int
				kind  record.OutboxKind
			}{{20, record.OutboxKindControllerNotice}, {34, record.OutboxKindIssueSuspend}} {
				if version < queued.since {
					continue
				}
				if _, err := store.pool.Exec(ctx, insert, string(queued.kind)); err != nil {
					t.Fatalf("queue a %s row at schema %d: %v", queued.kind, version, err)
				}
			}
			if _, err := store.Migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			for _, kind := range kinds {
				if _, err := store.pool.Exec(ctx, insert, string(kind)); err != nil {
					t.Errorf("an outbox row of kind %s: %v", kind, err)
				}
			}
		})
	}
}

// A database can have ready_merge rows from 0043 when 0044 arrives. Its dedup index must migrate
// those rows instead of refusing the upgrade: it keeps the earliest unleased next_at, breaking a
// tie on id, and drops any leased duplicate when an unleased row exists.
func TestReadyMergeDedupMigrationCollapsesExistingRows(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, 43)
	now := time.Now().UTC()
	insert := `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token, lease_until)
		values ('ready_merge', 'LEGION-208', '{"number":42,"head":"ready-head"}', 0, $1, '', $2, $3) returning id`
	var later, earliest, leased int64
	if err := store.pool.QueryRow(ctx, insert, now.Add(2*time.Minute), "", nil).Scan(&later); err != nil {
		t.Fatalf("insert later row: %v", err)
	}
	if err := store.pool.QueryRow(ctx, insert, now.Add(time.Minute), "", nil).Scan(&earliest); err != nil {
		t.Fatalf("insert earliest row: %v", err)
	}
	if err := store.pool.QueryRow(ctx, insert, now, "mid-lease", now.Add(time.Hour)).Scan(&leased); err != nil {
		t.Fatalf("insert leased row: %v", err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through ready-merge dedup: %v", err)
	}
	var id int64
	var nextAt time.Time
	if err := store.pool.QueryRow(ctx, `select id, next_at from outbox where kind = 'ready_merge'`).Scan(&id, &nextAt); err != nil {
		t.Fatalf("read surviving READY merge: %v", err)
	}
	wantNext := now.Add(time.Minute).Truncate(time.Microsecond)
	if id != earliest || !nextAt.Equal(wantNext) {
		t.Fatalf("surviving READY merge = id %d at %s, want unleased earliest id %d at %s (later %d leased %d)", id, nextAt, earliest, wantNext, later, leased)
	}
}

// 0044 can run while a daemon still on 0043 works the outbox. A ready_merge row that daemon commits
// while the dedup waits would be one the dedup's snapshot never saw, and would fail the unique index
// and with it the upgrade. A worker claiming the duplicate the dedup deletes locks it and then
// writes its lease (record's ClaimDue), and a lock that let the dedup start between the two would
// leave the lease write waiting on the migration while the migration waits on the worker's row. The
// migration locks the outbox before its dedup so that the worker's claim finishes, the writer waits
// for the index and meets it (unique_violation), and the upgrade succeeds.
func TestReadyMergeDedupMigrationHoldsOffAConcurrentEnqueue(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, 43)
	insert := `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
		values ('ready_merge', 'LEGION-208', '{"number":42,"head":"ready-head"}', 0, $1, '', '') -- concurrent enqueue`
	now := time.Now().UTC()
	var duplicate int64
	if _, err := store.pool.Exec(ctx, insert, now); err != nil {
		t.Fatalf("insert the row the dedup keeps: %v", err)
	}
	if err := store.pool.QueryRow(ctx, insert+"\nreturning id", now.Add(time.Minute)).Scan(&duplicate); err != nil {
		t.Fatalf("insert the duplicate the dedup deletes: %v", err)
	}
	// A worker claims the duplicate as ClaimDue does: it locks the row, and writes its lease later.
	worker, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the worker: %v", err)
	}
	defer func() { _ = worker.Rollback(ctx) }()
	if _, err := worker.Exec(ctx, "select id from outbox where id = $1 for update", duplicate); err != nil {
		t.Fatalf("lock the duplicate: %v", err)
	}
	migrated := make(chan error, 1)
	go func() {
		_, err := store.Migrate(ctx)
		migrated <- err
	}()
	// pg_stat_activity keeps a statement's first kilobyte, so the migration is matched by its name.
	waitForALockWait(t, store, "0044_ready_merge_dedup", nil)
	enqueued := make(chan error, 1)
	go func() {
		_, err := store.pool.Exec(ctx, insert, now.Add(2*time.Minute))
		enqueued <- err
	}()
	enqueue := waitForALockWait(t, store, "-- concurrent enqueue", enqueued)
	if _, err := worker.Exec(ctx, "update outbox set lease_token = $2, lease_until = $3 where id = $1", duplicate, "worker", now.Add(30*time.Second)); err != nil {
		t.Fatalf("write the worker's lease while the migration waits: %v", err)
	}
	if err := worker.Commit(ctx); err != nil {
		t.Fatalf("finish the worker's claim: %v", err)
	}
	if err := <-migrated; err != nil {
		t.Fatalf("migrate while a writer enqueues: %v", err)
	}
	if enqueue == nil {
		enqueue = <-enqueued
	}
	var refused *pgconn.PgError
	if !errors.As(enqueue, &refused) || refused.Code != "23505" {
		t.Fatalf("the concurrent enqueue = %v, want it held off until the unique index refuses it", enqueue)
	}
	var rows int
	if err := store.pool.QueryRow(ctx, `select count(*) from outbox where kind = 'ready_merge'`).Scan(&rows); err != nil {
		t.Fatalf("count ready_merge rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("ready_merge rows %d, want one", rows)
	}
}

// An issue already awaiting its merge when the daemon is upgraded to the release that submits READY
// merges reached awaiting_merge before any ready_merge row existed, and the workflow queues one only
// as an issue reaches awaiting_merge or its pull request reopens. The upgrade queues it once: for
// each issue awaiting its merge at its merger's READY head, in the workflow, on an open pull
// request, in a tree that does not linger, and with no ready_merge row already. A READY a person
// merges (its by-hand note), a closed pull request, an issue that left the workflow, a lingering
// tree and an issue in another phase get none, and an existing row is kept as it is.
func TestTheUpgradeQueuesTheMergeOfAREADYAlreadyAwaitingIt(t *testing.T) {
	seed := func(t *testing.T, store *Store) {
		t.Helper()
		ctx, records := context.Background(), record.NewStore()
		until, root := time.Now().Add(time.Hour), "LEGION-212"
		awaiting := func(key, status string) record.Issue {
			return record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: key, Project: "LEGION", Title: key, Phase: phase.AwaitingMerge, Generation: 1, Status: status, Rank: "U"}
		}
		lingering := awaiting("LEGION-213", "retro")
		lingering.Tree, lingering.Parent = root, &root
		merging := awaiting("LEGION-214", "retro")
		merging.Phase = phase.Merging
		if err := pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error {
			for _, seeded := range []struct {
				issue     record.Issue
				mergeNote string
				pr        record.PullRequestState
			}{
				{awaiting("LEGION-208", "retro"), "", record.PullRequestOpen},
				{awaiting("LEGION-209", "retro"), "MANUAL_MERGE_REQUIRED: pull request #43 matches a by_hand merge rule; do not auto-arm it", record.PullRequestOpen},
				{awaiting("LEGION-210", "retro"), "", record.PullRequestClosed},
				{awaiting("LEGION-211", "done"), "", record.PullRequestOpen},
				{record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION", Title: root, Phase: phase.Done, Generation: 1, Status: "done", Rank: "T", LingerUntil: &until}, "", ""},
				{lingering, "", record.PullRequestOpen},
				{merging, "", record.PullRequestOpen},
			} {
				// The seed runs at a schema before 0047, which the record store's PutIssue writes
				// beyond, so it writes the columns that schema has.
				if _, err := tx.Exec(ctx, `insert into issues (key, tree, project, title, parent, phase, generation, status, rank, linger_until, last_dispatch_seq)
					values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 0)`,
					seeded.issue.Key, seeded.issue.Tree, seeded.issue.Project, seeded.issue.Title, seeded.issue.Parent, string(seeded.issue.Phase),
					int64(seeded.issue.Generation), seeded.issue.Status, seeded.issue.Rank, seeded.issue.LingerUntil); err != nil {
					return err
				}
				if seeded.pr == "" {
					continue
				}
				if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: seeded.issue.Key, Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "ready-head", MergeNote: seeded.mergeNote}); err != nil {
					return err
				}
				number, _ := strconv.Atoi(strings.TrimPrefix(seeded.issue.Key, "LEGION-"))
				if err := records.PutPullRequest(ctx, tx, record.PullRequest{State: seeded.pr, Issue: seeded.issue.Key, Repo: "acme/widgets", Number: number,
					Branch: "legion/" + seeded.issue.Key, HeadSHA: "ready-head", Failing: []string{}}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("seed the issues: %v", err)
		}
	}
	readyMerges := func(t *testing.T, store *Store) []string {
		t.Helper()
		rows, err := store.pool.Query(context.Background(), `select id, issue, payload->>'number', payload->>'head', attempts from outbox where kind = 'ready_merge' order by id`)
		if err != nil {
			t.Fatalf("read ready_merge rows: %v", err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var id int64
			var issue, number, head string
			var attempts int
			if err := rows.Scan(&id, &issue, &number, &head, &attempts); err != nil {
				t.Fatalf("scan a ready_merge row: %v", err)
			}
			got = append(got, fmt.Sprintf("row %d %s #%s at %s attempts %d", id, issue, number, head, attempts))
		}
		return got
	}
	t.Run("from schema 42", func(t *testing.T) {
		store := emptyStore(t)
		migrateThrough(t, store, 42)
		seed(t, store)
		if _, err := store.Migrate(context.Background()); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		got := readyMerges(t, store)
		if len(got) != 1 || !strings.HasSuffix(got[0], " LEGION-208 #208 at ready-head attempts 0") {
			t.Fatalf("READY merges after the upgrade %q, want LEGION-208's alone", got)
		}
	})
	t.Run("an existing row is kept", func(t *testing.T) {
		store := emptyStore(t)
		migrateThrough(t, store, 44)
		seed(t, store)
		if _, err := store.pool.Exec(context.Background(), `insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token)
			values ('ready_merge', 'LEGION-208', '{"number":208,"head":"ready-head"}', 3, now(), 'waiting', '')`); err != nil {
			t.Fatalf("queue the existing row: %v", err)
		}
		before := readyMerges(t, store)
		if _, err := store.Migrate(context.Background()); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		if got := readyMerges(t, store); !slices.Equal(got, before) {
			t.Fatalf("READY merges after the upgrade %q, want the existing row alone, unchanged: %q", got, before)
		}
	})
	// A daemon still on 0043 or 0044 can enqueue the same READY merge while 0045 runs. Its row, not
	// committed yet, is one the backfill's check for an existing row cannot see, so the backfill's
	// insert waits on it at the unique index; once the writer commits, the backfill keeps the
	// writer's row and inserts nothing, and the upgrade succeeds.
	t.Run("a live writer's uncommitted row", func(t *testing.T) {
		ctx := context.Background()
		store := emptyStore(t)
		migrateThrough(t, store, 44)
		seed(t, store)
		writer, err := store.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the writer: %v", err)
		}
		defer func() { _ = writer.Rollback(ctx) }()
		row, err := record.NewOutboxRow("LEGION-208", record.ReadyMerge{Number: 208, Head: "ready-head"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		row.Attempts = 2
		if err := record.NewStore().Enqueue(ctx, writer, row); err != nil {
			t.Fatalf("the writer's enqueue: %v", err)
		}
		migrated := make(chan error, 1)
		go func() {
			_, err := store.Migrate(ctx)
			migrated <- err
		}()
		// pg_stat_activity keeps a statement's first kilobyte, so the migration is matched by its name.
		if err := waitForALockWait(t, store, "0045_ready_merge_backfill", migrated); err != nil {
			t.Fatalf("the backfill did not wait on the writer's row: %v", err)
		}
		if err := writer.Commit(ctx); err != nil {
			t.Fatalf("commit the writer's row: %v", err)
		}
		if err := <-migrated; err != nil {
			t.Fatalf("migrate while a writer enqueues: %v", err)
		}
		if got := readyMerges(t, store); len(got) != 1 || !strings.HasSuffix(got[0], " LEGION-208 #208 at ready-head attempts 2") {
			t.Fatalf("READY merges after the upgrade %q, want the writer's row alone", got)
		}
	})
}

// waitForALockWait waits until another backend running a statement that contains marker waits on a
// lock, and returns nil; or, when done reports that statement's end first, returns its error.
func waitForALockWait(t *testing.T, store *Store, marker string, done <-chan error) error {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			if err == nil {
				return errors.New("finished without waiting on a lock")
			}
			return err
		default:
		}
		var waiting bool
		if err := store.pool.QueryRow(context.Background(), `select exists (select 1 from pg_stat_activity
			where pid <> pg_backend_pid() and wait_event_type = 'Lock' and strpos(query, $1) > 0)`, marker).Scan(&waiting); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if waiting {
			return nil
		}
		if time.Now().After(deadline) {
			t.Fatalf("no statement containing %q waited on a lock", marker)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A duplicate ready_merge enqueue (a reopen at READY's head) reuses the durable row. It keeps the
// row's next_at, so a reopen never pulls the row forward past GitHub's rate-limit deadline, and
// restarts its attempts. A row a worker holds is taken from that worker: its lease token no longer
// matches, so the worker, which read the record before the reopen, can neither finish nor retry it,
// and the row decides again once the lease expires. A row no worker holds stays unleased.
func TestReadyMergeEnqueueReusesTheRowAndTakesItFromItsWorker(t *testing.T) {
	for _, leased := range []bool{false, true} {
		t.Run(fmt.Sprintf("leased %t", leased), func(t *testing.T) {
			ctx := context.Background()
			store := emptyStore(t)
			if _, err := store.Migrate(ctx); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			now := time.Now().UTC()
			row, err := record.NewOutboxRow("LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now.Add(3*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			row.Attempts, row.LastError = 7, "GitHub GraphQL: API rate limit exceeded"
			if err := pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error { return record.NewStore().Enqueue(ctx, tx, row) }); err != nil {
				t.Fatalf("enqueue rate-limited row: %v", err)
			}
			if leased {
				if _, err := store.pool.Exec(ctx, `update outbox set lease_token = 'worker', lease_until = $1 where kind = 'ready_merge'`, now.Add(30*time.Second)); err != nil {
					t.Fatalf("lease the row: %v", err)
				}
			}
			reopened, err := record.NewOutboxRow("LEGION-208", record.ReadyMerge{Number: 42, Head: "ready-head"}, now)
			if err != nil {
				t.Fatal(err)
			}
			if err := pgx.BeginFunc(ctx, store.pool, func(tx pgx.Tx) error { return record.NewStore().Enqueue(ctx, tx, reopened) }); err != nil {
				t.Fatalf("enqueue reopen: %v", err)
			}
			var rows, attempts int
			var nextAt time.Time
			var token string
			if err := store.pool.QueryRow(ctx, `select count(*), max(attempts), max(next_at), max(lease_token) from outbox where kind = 'ready_merge'`).Scan(&rows, &attempts, &nextAt, &token); err != nil {
				t.Fatalf("read ready merge: %v", err)
			}
			if want := row.NextAt.Truncate(time.Microsecond); rows != 1 || attempts != 0 || !nextAt.Equal(want) {
				t.Fatalf("reopened READY merge rows %d, attempts %d, next_at %s; want one row, attempts 0, the rate-limit deadline %s kept", rows, attempts, nextAt, want)
			}
			if leased == (token == "") || token == "worker" {
				t.Fatalf("reopened READY merge lease token %q; leased %t wants it taken from the worker", token, leased)
			}
		})
	}
}
