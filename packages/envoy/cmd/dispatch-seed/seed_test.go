package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// testScale is the scale the suite seeds at: about 200 issues, which is small enough that two
// whole runs fit in one test and large enough that every arm of the graph, every size band and
// every anchor shape holds rows.
const testScale = 0.01

// TestSeedAtASmallScale seeds two fresh databases and holds the pair, and then one of them, to
// everything the seed promises. Both runs are in one test because each one takes most of a
// minute: a test of its own for each check would seed six databases to read one data set.
func TestSeedAtASmallScale(t *testing.T) {
	first, firstRun := seedTestDatabase(t)
	second, _ := seedTestDatabase(t)

	t.Run("two runs at one scale write the same rows", func(t *testing.T) {
		assertSameRowCounts(t, first, second)
		assertSameChecksums(t, first, second)
	})
	t.Run("the seeded database owes no background work", func(t *testing.T) {
		assertNoOwedWork(t, first)
	})
	t.Run("the event counters match the seeded events", func(t *testing.T) {
		assertEventCountersMatch(t, first)
	})
	t.Run("every ask has the event its reader resolves", func(t *testing.T) {
		assertEveryAskHasItsOpeningEvent(t, first)
	})
	t.Run("every arm of graph_edges holds rows", func(t *testing.T) {
		assertEveryGraphArmHoldsRows(t, first)
	})
	t.Run("the rare word finds exactly the documents holding it", func(t *testing.T) {
		assertRareWordFindsItsDocuments(t, first, firstRun)
	})
	t.Run("a second seed of a filled database is refused", func(t *testing.T) {
		err := refuseFilledDatabase(context.Background(), first)
		if err == nil {
			t.Fatal("seeding a filled database again was not refused")
		}
		if !strings.Contains(err.Error(), "projects rows") || !strings.Contains(err.Error(), "issues rows") {
			t.Fatalf("the refusal does not name what it found: %v", err)
		}
	})
}

// seedTestDatabase seeds a database of its own and answers it with the run that filled it.
func seedTestDatabase(t *testing.T) (*store.Store, *seedRun) {
	t.Helper()
	database := storetest.Open(t)
	started := time.Now()
	run, err := seed(context.Background(), database, testScale, io.Discard)
	if err != nil {
		t.Fatalf("seed at scale %v: %v", testScale, err)
	}
	t.Logf("seeded at scale %v in %s", testScale, time.Since(started).Round(time.Millisecond))
	return database, run
}

// TestSeedBuildsTermStatisticsOnMainsSchema seeds the shape the measurement protocol's "before"
// side seeds: a database migrated from a checkout of main, filled by this tool with
// --no-migrate. Its search_term_totals carries no snapshot column (migration 0166) and the build
// of this release reads one, so without the step choosing its build by the schema every project
// fails and the seed reports a data set whose term statistics were never built.
//
// The two columns are dropped from a database migrated to the head rather than the schema being
// migrated from main, because a test process has no checkout of main to migrate from. Dropping
// them leaves search_term_totals the shape migration 0139 gave it, which is the shape the step
// reads the catalogue for; the rest of main's schema is older still, and the step's build reads
// nothing of it that this head does not also have.
func TestSeedBuildsTermStatisticsOnMainsSchema(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx,
		`alter table search_term_totals drop column snapshot, drop column snapshot_at`); err != nil {
		t.Fatalf("drop the snapshot columns 0166 added: %v", err)
	}

	started := time.Now()
	run, err := seed(ctx, database, testScale, io.Discard)
	if err != nil {
		t.Fatalf("seed at scale %v on main's search_term_totals: %v", testScale, err)
	}
	t.Logf("seeded at scale %v in %s", testScale, time.Since(started).Round(time.Millisecond))

	var built int
	if err := database.Pool.QueryRow(ctx, `select count(*) from search_term_totals`).Scan(&built); err != nil {
		t.Fatalf("count the totals rows: %v", err)
	}
	if built != len(run.plan.projects) {
		t.Fatalf("the term statistics were built for %d projects, not %d", built, len(run.plan.projects))
	}
	// Every project's row holds a counted item total and the moment it was counted, which is
	// every column main's search_term_totals has, and lexemes to go with it: a row claimed and
	// never rebuilt would carry 0 items and no statistics.
	var unbuilt int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from projects p
		where not exists (select 1 from search_term_totals t
		                   where t.project_key = p.key and t.items > 0 and t.refreshed_at is not null)
		   or not exists (select 1 from search_term_stats s where s.project_key = p.key)
	`).Scan(&unbuilt); err != nil {
		t.Fatalf("count the projects whose statistics were not built: %v", err)
	}
	if unbuilt != 0 {
		t.Fatalf("%d of %d projects hold no counted items or no lexemes", unbuilt, len(run.plan.projects))
	}
}

// TestSeedRefusesAFilledDatabaseWithoutMigratingIt holds the seed to refusing a database that
// already holds rows before it has written anything to it, its migrations included. A
// --database-url that names a database in use by mistake is the case the order decides: the
// migrations this release adds (0158 to 0167) have been run by no release, and applying them to
// somebody else's database is not something the refusal that follows takes back.
//
// Each case reads schema_migrations before and after, whole and in order. A database with no
// Dispatch schema carries no such table, so "it did not change" is "it is still not there": a
// migrate-first run creates schema_migrations before it applies anything (store.createVersionTable),
// so it shows there whichever migration it then fails on.
func TestSeedRefusesAFilledDatabaseWithoutMigratingIt(t *testing.T) {
	t.Run("a database that is not Dispatch's", func(t *testing.T) {
		ctx := context.Background()
		database := storetest.Open(t)
		// Somebody else's database: no Dispatch schema, no schema_migrations, and a projects
		// table of its own carrying rows.
		for _, statement := range []string{
			`drop schema public cascade`,
			`create schema public`,
			`create table projects (key text primary key, owner text not null)`,
			`insert into projects (key, owner) values ('ledger', 'somebody else')`,
		} {
			if _, err := database.Pool.Exec(ctx, statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
		if before := migrationVersionsOf(t, database); before != nil {
			t.Fatalf("the database carries schema_migrations %v before the seed was pointed at it", before)
		}

		err := Seed(ctx, Options{DatabaseURL: databaseURLOf(t, database), Scale: testScale,
			Migrate: true, Out: io.Discard})
		assertFilledDatabaseRefusal(t, err, "1 projects rows")

		if after := migrationVersionsOf(t, database); after != nil {
			t.Errorf("the seed migrated the database it refused: schema_migrations holds %v", after)
		}
		var dispatchTable bool
		if err := database.Pool.QueryRow(ctx, `select to_regclass('issues') is not null`).Scan(&dispatchTable); err != nil {
			t.Fatalf("look for the issues table: %v", err)
		}
		if dispatchTable {
			t.Error("the seed created Dispatch's issues table in the database it refused")
		}
	})

	t.Run("a Dispatch database that already holds rows", func(t *testing.T) {
		ctx := context.Background()
		database := storetest.Open(t)
		if _, err := database.Pool.Exec(ctx, `
			insert into projects (key, name, next_number, created_at, last_seq)
			values ('INUSE', 'a database in use', 1, now(), 0)
		`); err != nil {
			t.Fatalf("fill the database: %v", err)
		}
		before := migrationVersionsOf(t, database)
		if len(before) == 0 {
			t.Fatal("the test database records no migration, so its before and after compare nothing")
		}

		err := Seed(ctx, Options{DatabaseURL: databaseURLOf(t, database), Scale: testScale,
			Migrate: true, Out: io.Discard})
		assertFilledDatabaseRefusal(t, err, "1 projects rows")

		if after := migrationVersionsOf(t, database); !slices.Equal(after, before) {
			t.Errorf("schema_migrations holds %v after the refusal and held %v before it", after, before)
		}
	})

	t.Run("an empty database and an empty schema both pass", func(t *testing.T) {
		ctx := context.Background()
		database := storetest.Open(t)
		if err := refuseFilledDatabase(ctx, database); err != nil {
			t.Errorf("a migrated database holding no row was refused: %v", err)
		}
		// And the same database with nothing in it at all, which is what --database-url names
		// before the seed migrates it.
		for _, statement := range []string{`drop schema public cascade`, `create schema public`} {
			if _, err := database.Pool.Exec(ctx, statement); err != nil {
				t.Fatalf("%s: %v", statement, err)
			}
		}
		if err := refuseFilledDatabase(ctx, database); err != nil {
			t.Errorf("a database carrying no table at all was refused: %v", err)
		}
	})
}

// databaseURLOf is the URL the store was opened with. Seed takes a URL and opens a pool of its
// own from it, and what it does between opening one and seeding it is this test's subject, so
// the test calls Seed rather than seed; storetest hands out the database and not its URL.
func databaseURLOf(t *testing.T, database *store.Store) string {
	t.Helper()
	return database.Pool.Config().ConnString()
}

// migrationVersionsOf is every version schema_migrations records, in order, and nil when the
// database carries no such table.
func migrationVersionsOf(t *testing.T, database *store.Store) []int {
	t.Helper()
	ctx := context.Background()
	var present bool
	if err := database.Pool.QueryRow(ctx, `select to_regclass('schema_migrations') is not null`).Scan(&present); err != nil {
		t.Fatalf("look for schema_migrations: %v", err)
	}
	if !present {
		return nil
	}
	rows, err := database.Pool.Query(ctx, `select version from schema_migrations order by version`)
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	return versions
}

// assertFilledDatabaseRefusal holds err to the refusal refuseFilledDatabase writes, naming what
// it counted: a run that failed for another reason - a migration it could not apply, say - is not
// this test passing. It reports rather than stops, so the comparison of schema_migrations that
// follows it runs and says whether the database was written to as well.
func assertFilledDatabaseRefusal(t *testing.T, err error, counted string) {
	t.Helper()
	if err == nil {
		t.Error("the seed accepted a database that already holds rows")
		return
	}
	for _, want := range []string{"the database already holds data", counted} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// assertSameRowCounts holds every table of the two databases to the same count. The tables are
// read from the catalogue rather than listed here, so a table a later migration adds is covered
// without this test being edited.
func assertSameRowCounts(t *testing.T, first, second *store.Store) {
	t.Helper()
	firstCounts := rowCountsOf(t, first)
	secondCounts := rowCountsOf(t, second)
	for table, count := range firstCounts {
		if other := secondCounts[table]; other != count {
			t.Errorf("%s holds %d rows in the first run and %d in the second", table, count, other)
		}
	}
	for table := range secondCounts {
		if _, held := firstCounts[table]; !held {
			t.Errorf("%s is in the second run's database and not the first's", table)
		}
	}
}

func rowCountsOf(t *testing.T, database *store.Store) map[string]int {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `
		select table_name from information_schema.tables
		where table_schema = 'public' and table_type = 'BASE TABLE' order by table_name
	`)
	if err != nil {
		t.Fatalf("list the tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("list the tables: %v", err)
	}
	counts := make(map[string]int, len(tables))
	for _, table := range tables {
		var count int
		if err := database.Pool.QueryRow(ctx, `select count(*) from `+pgx.Identifier{table}.Sanitize()).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		counts[table] = count
	}
	return counts
}

// checksums are the sorted samples the repeatability check compares, by table. Each names its
// columns: a column a trigger or a default fills with now() is left out, because two runs are not
// at one instant, and so is every transaction-id column, which the queue of a later seam carries.
// A long text column is compared by its md5, so one checksum is not the whole database again.
var checksums = []struct {
	table   string
	columns string
}{
	{"projects", `key, name, next_number, created_at, last_seq`},
	{"repo_projects", `repo, project, created_at, is_default`},
	{"issues", `key, project_key, number, title, status, parent_key, created_at, updated_at,
		closed_at, rank, repo, priority, last_seq, tasks_done, tasks_total, tasks_version,
		md5(search::text), title_lexemes::text`},
	{"issue_links", `issue_key, kind, target_key, created_at`},
	{"issue_components", `issue_key, mode, reason, created_at, updated_at`},
	{"issue_component_members", `issue_key, project_key, component_id`},
	{"components", `project_key, id, title, prose, parent, snapshot_id, source_id`},
	{"component_depends", `project_key, from_id, to_id`},
	{"artifacts", `id::text, issue_key, project_key, slug, name, kind, is_primary, created_at, ref_key, last_seq`},
	{"artifact_versions", `artifact_id::text, number, md5(markdown), named, created_at, md5(search::text)`},
	{"document_blocks", `artifact_id::text, block_id, node_id, type, text_sha256, md5(excerpt), changed_version, changed_at`},
	{"document_block_texts", `artifact_id::text, block_id, version, md5(text)`},
	{"document_block_versions", `artifact_id::text, version`},
	{"document_indexed_versions", `artifact_id::text, version`},
	{"asks", `id::text, issue_key, artifact_id::text, md5(question), state, answer::text, resolution::text,
		anchor::text, created_at, block_id, block_artifact_id::text, written_version, md5(search::text)`},
	{"ask_followers", `ask_id::text, session_id, since`},
	{"comments", `id::text, issue_key, artifact_id::text, md5(body), anchor::text, reply_to::text, resolved,
		created_at, ask_id::text, replies_to_block, edited_at, written_version, md5(search::text)`},
	{"comment_blocks", `comment_id::text, block_id, node_id, position, type, text_sha256, md5(excerpt),
		made_at, changed_revision, changed_at, removed_at`},
	{"comment_block_texts", `comment_id::text, block_id, revision, md5(text)`},
	{"comment_block_state", `comment_id::text, revision, next_block, body_sha256, owed_at`},
	{"messages", `id::text, issue_key, md5(body), created_at, in_reply_to::text, replies_to_block, md5(search::text)`},
	{"message_blocks", `message_id::text, block_id, node_id, position, type, text_sha256, md5(excerpt),
		made_at, changed_at, removed_at`},
	{"message_block_state", `message_id::text, body_sha256, owed_at`},
	{"refs", `from_kind, from_id, to_kind, to_id, kind, created_at, excerpt_block_id, md5(excerpt_text), excerpt_version`},
	{"block_refs", `artifact_id::text, block_id, from_node, to_kind, to_id, kind, created_at, created_version`},
	{"comment_block_refs", `comment_id::text, block_id, from_node, to_kind, to_id, kind, created_at`},
	{"message_block_refs", `message_id::text, block_id, from_node, to_kind, to_id, kind, created_at`},
	{"events", `id, issue_key, artifact_id::text, project_key, seq, type, actor::text, payload::text,
		notify, published_at, created_at`},
	{"embeddings", `kind, id, content_hash, embedded_hash, model, embedded_at, md5(embedding::text)`},
	{"search_term_stats", `project_key, lexeme, ndoc`},
	{"search_term_totals", `project_key, items`},
}

func assertSameChecksums(t *testing.T, first, second *store.Store) {
	t.Helper()
	for _, item := range checksums {
		query := fmt.Sprintf(
			`select coalesce(md5(string_agg(row_text, '|' order by row_text)), 'empty')
			 from (select concat_ws(':', %s) as row_text from %s) sample`,
			item.columns, pgx.Identifier{item.table}.Sanitize())
		firstSum := checksumOf(t, first, item.table, query)
		secondSum := checksumOf(t, second, item.table, query)
		if firstSum != secondSum {
			t.Errorf("%s: the two runs checksum differently (%s and %s)", item.table, firstSum, secondSum)
		}
		if firstSum == "empty" {
			t.Errorf("%s: both runs left the table empty, so its checksum proves nothing", item.table)
		}
	}
}

func checksumOf(t *testing.T, database *store.Store, table, query string) string {
	t.Helper()
	var sum string
	if err := database.Pool.QueryRow(context.Background(), query).Scan(&sum); err != nil {
		t.Fatalf("checksum %s: %v", table, err)
	}
	return sum
}

// assertNoOwedWork holds the seeded database to the five counts settle.go's owedWorkCounts
// leaves at zero, the stale-excerpt count included. A Dispatch server started on a database that
// answered anything else would reindex the documents, rebuild the item blocks, count the tasks
// again and rewrite the excerpts, and a measurement taken across that churn would read two
// different data sets.
func assertNoOwedWork(t *testing.T, database *store.Store) {
	t.Helper()
	var blocks, grammar, items, excerpts, stale int
	if err := database.Pool.QueryRow(context.Background(), owedWorkCounts).
		Scan(&blocks, &grammar, &items, &excerpts, &stale); err != nil {
		t.Fatalf("count the owed work: %v", err)
	}
	for _, count := range []struct {
		label string
		value int
	}{
		{"documents owed a block index", blocks},
		{"bodies owed a grammar pass", grammar},
		{"items owed a block index", items},
		{"mentions owed an excerpt", excerpts},
		{"mentions whose excerpt is older than their document's latest version", stale},
	} {
		if count.value != 0 {
			t.Errorf("%s: %d, want 0", count.label, count.value)
		}
	}
}

// assertEventCountersMatch holds every event owner's last_seq to the highest seq it carries, and
// every project's next_number above its highest issue number. events/broker.go's Append inserts
// seq = last_seq + 1 under the unique (issue_key, seq), so an owner whose counter is behind fails
// the first write to it.
func assertEventCountersMatch(t *testing.T, database *store.Store) {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select 'issue ' || i.key || ' last_seq ' || i.last_seq || ' events ' || coalesce(e.top, 0)
		  from issues i left join (select issue_key, max(seq) as top from events where issue_key is not null group by issue_key) e
		    on e.issue_key = i.key
		 where i.last_seq is distinct from coalesce(e.top, 0)
		union all
		select 'artifact ' || a.id || ' last_seq ' || a.last_seq || ' events ' || coalesce(e.top, 0)
		  from artifacts a left join (select artifact_id, max(seq) as top from events where artifact_id is not null group by artifact_id) e
		    on e.artifact_id = a.id
		 where a.last_seq is distinct from coalesce(e.top, 0)
		union all
		select 'project ' || p.key || ' last_seq ' || p.last_seq || ' events ' || coalesce(e.top, 0)
		  from projects p left join (select project_key, max(seq) as top from events where project_key is not null group by project_key) e
		    on e.project_key = p.key
		 where p.last_seq is distinct from coalesce(e.top, 0)
		union all
		select 'project ' || p.key || ' next_number ' || p.next_number || ' highest issue ' || i.top
		  from projects p join (select project_key, max(number) as top from issues group by project_key) i
		    on i.project_key = p.key
		 where p.next_number <= i.top
	`)
	if err != nil {
		t.Fatalf("read the event counters: %v", err)
	}
	mismatched, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the event counters: %v", err)
	}
	if len(mismatched) > 0 {
		t.Errorf("%d owners whose counter does not match their events, the first few: %v",
			len(mismatched), mismatched[:min(5, len(mismatched))])
	}
}

// assertEveryAskHasItsOpeningEvent holds every seeded ask to the identity event a read of it
// resolves. api.attachOpenedEventIDs runs events.OpenedEventIDs over every ask it is about to
// answer with and fails the whole request when one has no event, so an ask without one answers
// 500 on GET /api/v1/issues/{key}/asks, GET /api/v1/artifacts/{id}/asks, GET /api/v1/asks/{id},
// the Inbox and the issue read's open_asks. The check calls that same function rather than
// restating its query, so a change to which types establish an ask's identity is followed here.
func assertEveryAskHasItsOpeningEvent(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `select id::text from asks order by id`)
	if err != nil {
		t.Fatalf("list the asks: %v", err)
	}
	askIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("list the asks: %v", err)
	}
	if len(askIDs) == 0 {
		t.Fatal("the seeded database holds no ask, so this check proves nothing")
	}
	openedEventIDs, err := events.OpenedEventIDs(ctx, database.Pool, askIDs)
	if err != nil {
		t.Fatalf("resolve the opening events: %v", err)
	}
	missing := []string{}
	for _, askID := range askIDs {
		if _, held := openedEventIDs[askID]; !held {
			missing = append(missing, askID)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d of %d asks have no opening event, the first few: %v",
			len(missing), len(askIDs), missing[:min(5, len(missing))])
	}
}

// graphArmCount is how many arms the graph_edges view of migration 0142 unions. The test reads
// the arms from the view's own text, so it follows a migration that restates the view; this
// number is the guard that it still read a whole view.
const graphArmCount = 23

var (
	armKindLiteral   = regexp.MustCompile(`'([a-z_]+)'::text AS kind`)
	armToKindLiteral = regexp.MustCompile(`'([a-z_]+)'::text AS to_kind`)
)

// assertEveryGraphArmHoldsRows runs each arm of graph_edges as its own query and holds it to at
// least one row, and holds each (kind, to_kind) pair the view's literals define to a non-zero
// count in the view itself. The arms come from pg_get_viewdef, the view's own text, not from a
// list in this test and not from the seed's fixture: an arm a later migration adds is covered the
// day it lands.
func assertEveryGraphArmHoldsRows(t *testing.T, database *store.Store) {
	t.Helper()
	ctx := context.Background()
	var definition string
	if err := database.Pool.QueryRow(ctx, `select pg_get_viewdef('graph_edges', true)`).Scan(&definition); err != nil {
		t.Fatalf("read the graph_edges definition: %v", err)
	}
	arms := strings.Split(definition, "UNION ALL")
	if len(arms) != graphArmCount {
		t.Fatalf("graph_edges has %d arms, not the %d migration 0142 defines; read the view and update the test",
			len(arms), graphArmCount)
	}
	pairs := map[[2]string]bool{}
	for index, arm := range arms {
		arm = strings.TrimSuffix(strings.TrimSpace(arm), ";")
		var rows int
		if err := database.Pool.QueryRow(ctx, `select count(*) from (`+arm+`) arm`).Scan(&rows); err != nil {
			t.Fatalf("count arm %d of graph_edges: %v\n%s", index+1, err, arm)
		}
		if rows == 0 {
			t.Errorf("arm %d of graph_edges holds no row:\n%s", index+1, arm)
		}
		kind := armKindLiteral.FindStringSubmatch(arm)
		toKind := armToKindLiteral.FindStringSubmatch(arm)
		if kind != nil && toKind != nil {
			pairs[[2]string{kind[1], toKind[1]}] = true
		}
	}
	if len(pairs) == 0 {
		t.Fatal("no arm of graph_edges names its kind and its target kind as literals")
	}
	rows, err := database.Pool.Query(ctx, `
		select kind, to_kind, count(*) from graph_edges group by 1, 2
	`)
	if err != nil {
		t.Fatalf("group the graph edges: %v", err)
	}
	counted := map[[2]string]int{}
	for rows.Next() {
		var kind, toKind string
		var count int
		if err := rows.Scan(&kind, &toKind, &count); err != nil {
			t.Fatalf("group the graph edges: %v", err)
		}
		counted[[2]string{kind, toKind}] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("group the graph edges: %v", err)
	}
	for pair := range pairs {
		if counted[pair] == 0 {
			t.Errorf("graph_edges holds no %s edge to a %s", pair[0], pair[1])
		}
	}
	// Every node kind the backlink count resolves must be the target of a mention as well, which
	// the pairs above do not cover: the mention arms take their kind from a column.
	for _, kind := range []string{"issue", "artifact", "ask", "comment", "message", "component",
		"session", "block", "comment_block", "message_block"} {
		if counted[[2]string{"mentions", kind}] == 0 {
			t.Errorf("no mention names a %s, so the backlink count of that kind reads nothing", kind)
		}
	}
}

// assertRareWordFindsItsDocuments is the draft's A3, run as SQL rather than through a server: a
// search for the printed rare word finds exactly the documents whose latest version matches
// plainto_tsquery for it, and no older version brings one in.
func assertRareWordFindsItsDocuments(t *testing.T, database *store.Store, run *seedRun) {
	t.Helper()
	word := run.subject.RareWord
	if word == "" {
		t.Fatal("the run printed no rare word")
	}
	if run.subject.RareHits == 0 || run.subject.RareHits >= 100 {
		t.Fatalf("the rare word %q is in %d documents, which is not fewer than search's per-arm limit and more than none",
			word, run.subject.RareHits)
	}
	found := documentsMatching(t, database, `
		select a.id::text from artifacts a
		  join lateral (select v.search from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1) v on true
		 where a.kind = 'doc' and v.search @@ plainto_tsquery('english', $1)
		 order by 1`, word)
	expected := documentsMatching(t, database, `
		select artifact_id::text from (
			select distinct on (artifact_id) artifact_id, search
			  from artifact_versions order by artifact_id, number desc) latest
		 where search @@ plainto_tsquery('english', $1)
		 order by 1`, word)
	if !slices.Equal(found, expected) {
		t.Errorf("the rare word %q finds %d documents through the search leg and %d through the latest versions",
			word, len(found), len(expected))
	}
	if len(found) != run.subject.RareHits {
		t.Errorf("the command printed %d documents for %q and the database holds %d",
			run.subject.RareHits, word, len(found))
	}
}

func documentsMatching(t *testing.T, database *store.Store, query, word string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), query, word)
	if err != nil {
		t.Fatalf("find the documents holding %q: %v", word, err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("find the documents holding %q: %v", word, err)
	}
	return found
}
