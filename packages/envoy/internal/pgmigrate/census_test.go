package pgmigrate

import (
	"slices"
	"strings"
	"testing"
)

// Every statement form the repository's migrations take a lock with names its table, once, with
// a public. qualifier, quotes and case removed; a statement inside a comment names nothing, and
// one inside a DO block is read like any other. A foreign key locks the table it references
// (SHARE ROW EXCLUSIVE), so a table that is only referenced is touched too.
func TestTouchedTablesFindsEveryStatementFormTheMigrationsUse(t *testing.T) {
	for sql, want := range map[string][]string{
		"alter table messages add column broadcast_position integer;":                                                             {"messages"},
		"ALTER TABLE IF EXISTS ONLY public.asks DROP CONSTRAINT x;":                                                               {"asks"},
		"create unique index message_deliveries_one_acceptance on message_deliveries (message_id) where accepted_at is not null;": {"message_deliveries"},
		"create index concurrently if not exists i on only \"events\" (id);":                                                      {"events"},
		"drop table if exists old_a, old_b;":                                                                                      {"old_a", "old_b"},
		"truncate table only t1, t2;":                                                                                             {"t1", "t2"},
		"create trigger trg before insert on comments for each row execute function f();":                                         {"comments"},
		"update events set payload = '{}' where type = 'x';":                                                                      {"events"},
		"delete from refs where kind is null;":                                                                                    {"refs"},
		"insert into user_agent_read (login) select login from users;":                                                            {"user_agent_read"},
		"lock table asks in access exclusive mode;":                                                                               {"asks"},
		"-- alter table commented_out add column x integer;\ncreate table fresh (id integer);":                                    nil,
		"do $$ begin delete from artifacts where id is null; end $$;":                                                             {"artifacts"},
		"create table keys (id uuid not null references broadcasts(id), login text);":                                             {"broadcasts"},
		"alter table comments add constraint comments_ask foreign key (ask_id) references public.asks (id);":                      {"asks", "comments"},
		// An update may name its table under an alias; 0043 and 0044 rewrite comments that way.
		"update comments c set turn = 'human' from asks a where a.id = c.ask_id;":       {"comments"},
		"UPDATE artifact_versions AS v SET markdown = '' WHERE false;":                  {"artifact_versions"},
		"insert into keys (k) values (1) on conflict (k) do update set k = excluded.k;": {"keys"},
	} {
		if got := TouchedTables(sql); !slices.Equal(got, want) {
			t.Errorf("TouchedTables(%q) = %v, want %v", sql, got, want)
		}
	}
}

// An index a migration drops or alters is named, so the census can resolve it to its table.
func TestTouchedIndexesFindsDropAndAlterIndex(t *testing.T) {
	got := TouchedIndexes("drop index asks_search;\nalter index messages_reply_to rename to messages_in_reply_to;")
	if want := []string{"asks_search", "messages_reply_to"}; !slices.Equal(got, want) {
		t.Errorf("TouchedIndexes = %v, want %v", got, want)
	}
}

// The report is one census: line per fact, each refusal under its migration, ending with the
// verdict; a lock holder is named by its session metadata and never its query.
func TestReportWritesCountsAndEndsWithTheVerdict(t *testing.T) {
	count := int64(3)
	age := int64(8040)
	report := &Report{VersionTable: "schema_migrations", SchemaVersion: 52, TableLimit: CensusTableLimit, LongTransaction: CensusLongTransaction, Pending: []MigrationCensus{{
		Migration: Migration{Version: 53, Name: "0053_x.up.sql", CensusName: "0053_x.census.sql", Census: "select 1"},
		Tables: []TableCensus{
			{Name: "asks", Exists: true, Rows: 2113, Bytes: 8626176, Holders: []Session{{PID: 4242, User: "dispatch", Application: "psql", State: "idle in transaction", XactSeconds: &age}}},
			{Name: "later", Exists: false},
			{Name: "single", Exists: true, Rows: 1, Bytes: 8192},
		},
		Count:    &count,
		Refusals: []string{"its census counts 3 (0053_x.census.sql); the migration would refuse or rewrite what those rows hold"},
	}}}
	var out strings.Builder
	report.Write(&out)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	for _, want := range []string{
		"census: schema version 52 (schema_migrations); pending: 0053_x.up.sql",
		"census: 0053_x.up.sql touches asks: 2,113 rows, 8.2 MiB (limit 1.0 GiB); locks held by other sessions: pid 4242 (user dispatch, application \"psql\", idle in transaction, transaction open 2h14m0s)",
		"census: 0053_x.up.sql touches later: not in the database yet",
		"census: 0053_x.up.sql touches single: 1 row, 8.0 KiB (limit 1.0 GiB); locks held by other sessions: none",
		"census: REFUSED 0053_x.up.sql: its census counts 3 (0053_x.census.sql); the migration would refuse or rewrite what those rows hold",
		"census: transactions open longer than 1m0s: none",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("report lacks %q; got:\n%s", want, out.String())
		}
	}
	if last := lines[len(lines)-1]; last != "census: REFUSED (1 reason)" {
		t.Errorf("last line = %q, want the verdict", last)
	}
}

// Every refusal the verdict counts is printed, whatever its text: the report promises every
// reason is in it.
func TestReportPrintsEveryRefusalItCounts(t *testing.T) {
	report := &Report{VersionTable: "schema_migrations", SchemaVersion: 1, TableLimit: CensusTableLimit, LongTransaction: CensusLongTransaction, Pending: []MigrationCensus{{
		Migration: Migration{Version: 2, Name: "0002_x.up.sql"},
		Refusals:  []string{"things is held by an old transaction"},
	}}}
	var out strings.Builder
	report.Write(&out)
	if !strings.Contains(out.String(), "census: REFUSED 0002_x.up.sql: things is held by an old transaction\n") || !strings.HasSuffix(out.String(), "census: REFUSED (1 reason)\n") {
		t.Errorf("report:\n%s", out.String())
	}
}

// A session of another role shows its pid, role and application, and Postgres hides its state
// and transaction age from a role without pg_read_all_stats; the report says each is hidden
// rather than printing an empty field. An autovacuum worker shows no role either.
func TestSessionStringSaysWhatPostgresHides(t *testing.T) {
	for session, want := range map[Session]string{
		{PID: 79, User: "other"}: `pid 79 (user other, application "", state not visible, transaction age not visible)`,
		{PID: 464}:               `pid 464 (user not visible, application "", state not visible, transaction age not visible)`,
	} {
		if got := session.String(); got != want {
			t.Errorf("Session.String() = %q, want %q", got, want)
		}
	}
}
