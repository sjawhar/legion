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
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
// so a database at 0001-0006 and 0008 and later takes 0007 — here the controllers table — and ends
// with every version recorded.
func TestMigrateAppliesALowerMigrationADatabaseAlreadyPastItLacks(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	const skipped = 7
	for _, migration := range all {
		if migration.Version == skipped {
			continue
		}
		if _, err := store.apply(ctx, migration); err != nil {
			t.Fatalf("apply %s: %v", migration.Name, err)
		}
	}
	controllers := func() bool {
		t.Helper()
		var exists bool
		if err := store.pool.QueryRow(ctx, "select to_regclass('public.controllers') is not null").Scan(&exists); err != nil {
			t.Fatalf("look for the controllers table: %v", err)
		}
		return exists
	}
	if version, err := store.SchemaVersion(ctx); err != nil || version != all[len(all)-1].Version || controllers() {
		t.Fatalf("before Migrate: schema version %d (%v), controllers table %t; want the latest, and no table", version, err, controllers())
	}

	applied, err := store.Migrate(ctx)

	if err != nil || applied != 1 {
		t.Fatalf("migrate = %d, %v; want the one missing migration applied", applied, err)
	}
	if !controllers() {
		t.Error("migration 0007 was recorded as applied, but the controllers table does not exist")
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
