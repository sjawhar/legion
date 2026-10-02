package pgmigrate

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Every statement form the repository's migrations take a lock with names its table, once, with
// a public. qualifier, quotes and case removed; a statement inside a comment or a string literal
// names nothing, and one inside a DO block is read like any other. A function's body is defined by
// the migration, not run, so a statement inside it names nothing. A foreign key locks the table it
// references (SHARE ROW EXCLUSIVE), so a table that is only referenced is touched too.
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
		"update comments c set turn = 'human' from asks a where a.id = c.ask_id;":                                                  {"comments"},
		"UPDATE artifact_versions AS v SET markdown = '' WHERE false;":                                                             {"artifact_versions"},
		"insert into keys (k) values (1) on conflict (k) do update set k = excluded.k;":                                            {"keys"},
		"create function f() returns trigger language plpgsql as $$ begin insert into things (id) values (1); return new; end $$;": nil,
		"create or replace function g() returns void language sql as $fn$ update things set kind = null $fn$;":                     nil,
		"create function h() returns void language sql as 'delete from things';":                                                   nil,
		"insert into notes (body) values ('update things set kind = null -- or /* */');":                                           {"notes"},
		"do language plpgsql $body$ begin update things set kind = 'x'; end $body$;":                                               {"things"},
	} {
		if got := TouchedTables(sql); !slices.Equal(got, want) {
			t.Errorf("TouchedTables(%q) = %v, want %v", sql, got, want)
		}
	}
}

// CensusTables does not need the foreign-key catalog for a migration that writes no row, so a
// pure DDL migration stays readable when its caller cannot query that catalog.
func TestCensusTablesSkipsForeignKeysForAMigrationThatDoesNotWriteRows(t *testing.T) {
	got, err := CensusTables(context.Background(), refusingCatalogQuerier{}, "alter table things add column note text")
	if err != nil {
		t.Fatalf("CensusTables: %v", err)
	}
	if want := []TouchedTable{{Name: "things"}}; !slices.Equal(got, want) {
		t.Errorf("CensusTables = %#v, want %#v", got, want)
	}
}

type refusingCatalogQuerier struct{}

func (refusingCatalogQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected catalog query")
}

func (refusingCatalogQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	return nil
}

// An index a migration drops or alters is named, so CensusTables can resolve it to its table.
func TestTouchedIndexesFindsDropAndAlterIndex(t *testing.T) {
	got := touchedIndexes("drop index asks_search;\nalter index messages_reply_to rename to messages_in_reply_to;")
	if want := []string{"asks_search", "messages_reply_to"}; !slices.Equal(got, want) {
		t.Errorf("touchedIndexes = %v, want %v", got, want)
	}
}

// The foreign keys a pending migration adds are read from its create table and alter table
// statements, from the statement's table, cascading when the statement says on delete cascade,
// wherever the statement stands: inside a DO block too, behind the usual guard on pg_constraint. A
// references inside a literal or another statement adds none.
func TestAddedForeignKeysReadsTheKeysAMigrationAdds(t *testing.T) {
	for sql, want := range map[string][]foreignKey{
		"create table keys (id uuid not null references broadcasts(id) on delete cascade, login text);":                          {{from: "keys", to: "broadcasts", onDelete: "c"}},
		"ALTER TABLE public.comments ADD CONSTRAINT c FOREIGN KEY (ask_id) REFERENCES public.asks (id);":                         {{from: "comments", to: "asks", onDelete: "a"}},
		"alter table things add column note text; create table notes (body text default 'references things');":                   nil,
		"create table a (id int primary key);\ncreate table b (a_id int references a, c_id int references c on delete cascade);": {{from: "b", to: "a", onDelete: "c"}, {from: "b", to: "c", onDelete: "c"}},

		// Inside a DO block, and behind the usual guard on pg_constraint.
		"do $$ begin alter table things add column parent_id integer references parent on delete cascade; end $$;": {{from: "things", to: "parent", onDelete: "c"}},
		"do $$ begin if not exists (select from pg_constraint where conname = 'k') then " +
			"alter table things add constraint k foreign key (parent_id) references parent; end if; end $$;": {{from: "things", to: "parent", onDelete: "a"}},
	} {
		if got := addedForeignKeys(readMigration(sql).code); !slices.Equal(got, want) {
			t.Errorf("addedForeignKeys(%q) = %v, want %v", sql, got, want)
		}
	}
}

// The report is one census: line per fact, each refusal under its migration, ending with the
// verdict; a lock holder is named by its session metadata and never its query. A table a foreign
// key reaches says from where, and a size above the limit never prints as the limit itself.
func TestReportWritesCountsAndEndsWithTheVerdict(t *testing.T) {
	count := int64(3)
	age := 2*time.Hour + 14*time.Minute
	report := &Report{VersionTable: "schema_migrations", SchemaVersion: 52, TableLimit: CensusTableLimit, LongTransaction: CensusLongTransaction, Pending: []MigrationCensus{{
		Migration: Migration{Version: 53, Name: "0053_x.up.sql", CensusName: "0053_x.census.sql", Census: "select 1"},
		Tables: []TableCensus{
			{Name: "asks", Exists: true, Rows: 2113, Bytes: 8626176, Holders: []Session{{PID: 4242, User: "dispatch", Application: "psql", State: "idle in transaction", XactAge: &age}}},
			{Name: "later", Exists: false},
			{Name: "single", Exists: true, Rows: 1, Bytes: 8192},
			{Name: "followers", Exists: true, Bytes: 16384, Through: "asks"},
			{Name: "broadcasts", Exists: true, Bytes: 1115660288},
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
		"census: 0053_x.up.sql touches followers through a foreign key with asks: 16.0 KiB; locks held by other sessions: none",
		"census: 0053_x.up.sql touches broadcasts: 1,115,660,288 bytes, above the 1,073,741,824 bytes limit, so its rows were not counted; locks held by other sessions: none",
		"census: REFUSED 0053_x.up.sql: its census counts 3 (0053_x.census.sql); the migration would refuse or rewrite what those rows hold",
		"census: transactions open longer than 1m0s, or of an age the census cannot see: none",
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

// A fresh database's report is one line and the verdict.
func TestReportOfAFreshDatabaseIsOneLine(t *testing.T) {
	var out strings.Builder
	(&Report{VersionTable: "schema_migrations", Fresh: true, TableLimit: CensusTableLimit, LongTransaction: CensusLongTransaction}).Write(&out)
	if got, want := out.String(), "census: fresh database, nothing to check\ncensus: ok\n"; got != want {
		t.Errorf("report = %q, want %q", got, want)
	}
}

// A session of another role shows its pid, role and application, and Postgres hides its state
// and transaction age from a role without pg_read_all_stats; the report says each is hidden
// rather than printing an empty field. An autovacuum worker shows no role, and says what it is. A
// transaction's age is printed as Postgres measured it, to the microsecond, never rounded.
func TestSessionStringSaysWhatPostgresHides(t *testing.T) {
	age := 59*time.Second + 500123*time.Microsecond
	for session, want := range map[Session]string{
		{PID: 79, User: "other"}:     `pid 79 (user other, application "", state not visible, transaction age not visible)`,
		{PID: 464}:                   `pid 464 (user not visible, application "", state not visible, transaction age not visible)`,
		{PID: 798, Autovacuum: true}: `pid 798 (autovacuum worker, application "", state not visible, transaction age not visible)`,
		{PID: 80, User: "dispatch", State: "idle in transaction", XactAge: &age}: `pid 80 (user dispatch, application "", idle in transaction, transaction open 59.500123s)`,
	} {
		if got := session.String(); got != want {
			t.Errorf("Session.String() = %q, want %q", got, want)
		}
	}
}

// A lock holder refuses the migration when it would hold up the migration's lock past
// LockTimeout: a transaction open at least the bound, measured unrounded, or one whose age the
// census cannot see. An autovacuum worker does not, however long it has run, because Postgres
// cancels it once the migration has waited deadlock_timeout for its lock - unless it is an
// anti-wraparound vacuum, which Postgres does not cancel, or deadlock_timeout is not shorter than
// LockTimeout, so the migration gives up first. Whether a vacuum is anti-wraparound is its
// activity's to say: Postgres marks it so when it launches it, so a vacuum launched before its
// table passed its freeze age is not, and only where the activity says nothing - hidden from the
// census's role, or not tracked - does the census take the table's age for it.
func TestAHolderRefusesOnlyWhatWouldHoldTheMigrationPastItsLockTimeout(t *testing.T) {
	young, old := 5*time.Second, 2*time.Hour
	short, past := 59500*time.Millisecond, 60500*time.Millisecond
	yes, no := true, false
	hidden := lockHolder{Session: Session{PID: 798, Autovacuum: true}}
	regular := lockHolder{Session: Session{PID: 799, Autovacuum: true, XactAge: &old}, antiWraparound: &no}
	wraparound := lockHolder{Session: Session{PID: 800, Autovacuum: true, XactAge: &young}, antiWraparound: &yes}
	untracked := lockHolder{Session: Session{PID: 801, Autovacuum: true, State: "disabled"}, antiWraparound: &no}
	for name, tc := range map[string]struct {
		holder                      lockHolder
		pastFreezeAge, cancellation bool
		want                        string
	}{
		"an autovacuum Postgres cancels":                  {holder: hidden, cancellation: true},
		"an old autovacuum Postgres cancels":              {holder: regular, cancellation: true},
		"a regular vacuum of a table past its freeze age": {holder: regular, pastFreezeAge: true, cancellation: true},
		"an anti-wraparound vacuum":                       {holder: wraparound, cancellation: true, want: "pid 800 (autovacuum worker, application \"\", state not visible, transaction open 5s) holds a lock on things, and it is an anti-wraparound autovacuum"},
		"a hidden vacuum of a table past its freeze age":  {holder: hidden, pastFreezeAge: true, cancellation: true, want: "holds a lock on things, which is past its freeze age, so it may be an anti-wraparound autovacuum, which Postgres does not cancel for the migration's lock; granting the census's role pg_read_all_stats lets the census read which"},
		// With track_activities off Postgres shows the worker as disabled with an empty activity,
		// which says nothing about why it was launched, even to a role that may read it.
		"an untracked vacuum of a table past its freeze age": {holder: untracked, pastFreezeAge: true, cancellation: true, want: "holds a lock on things, which is past its freeze age, so it may be an anti-wraparound autovacuum, which Postgres does not cancel for the migration's lock; Postgres records no activity for it with track_activities off, so the census cannot read which"},
		"an untracked vacuum of a table short of it":         {holder: untracked, cancellation: true},
		"an autovacuum outlasting the lock timeout":          {holder: hidden, want: "deadlock_timeout is not shorter than"},
		"a session whose age Postgres hides":                 {holder: lockHolder{Session: Session{PID: 79, User: "other"}}, cancellation: true, want: "Postgres hides its transaction's age from the census's role, so the census cannot rule out a transaction older than 1m0s; granting that role pg_read_all_stats lets the census read the age"},
		"a session Postgres does not track":                  {holder: lockHolder{Session: Session{PID: 82, User: "dispatch", State: "disabled"}}, cancellation: true, want: "Postgres records no transaction start for it with track_activities off, so the census cannot rule out a transaction older than 1m0s"},
		"an old transaction":                                 {holder: lockHolder{Session: Session{PID: 80, User: "dispatch", XactAge: &old}}, cancellation: true, want: "has held a lock on things for 2h0m0s, longer than 1m0s"},
		"a young transaction":                                {holder: lockHolder{Session: Session{PID: 81, User: "dispatch", XactAge: &young}}, cancellation: true},
		"a transaction half a second short of the bound":     {holder: lockHolder{Session: Session{PID: 83, User: "dispatch", XactAge: &short}}, cancellation: true},
		"a transaction half a second past the bound":         {holder: lockHolder{Session: Session{PID: 84, User: "dispatch", XactAge: &past}}, cancellation: true, want: "transaction open 1m0.5s) has held a lock on things for 1m0.5s, longer than 1m0s"},
	} {
		got := holderRefusal(tc.holder, "things", tc.pastFreezeAge, tc.cancellation, time.Minute)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: holderRefusal = %q, want %q", name, got, tc.want)
		}
	}
}
