package pgmigrate

import "testing"

// migration0122 is the text of Dispatch's 0122_artifact_versions_block_index_trigger.up.sql, the
// migration the exemption was written for; the store's census test reads the shipped file itself.
const migration0122 = `-- 0122_artifact_versions_block_index_trigger.up.sql
-- Every document version any build writes marks its document's blocks owed.
create or replace function document_block_versions_owe() returns trigger language plpgsql as $$
begin
  insert into document_block_versions (artifact_id, version) values (new.artifact_id, null)
  on conflict (artifact_id) do update set version = null;
  return null;
end;
$$;
create trigger artifact_versions_block_index_owed after insert on artifact_versions
  for each row when (new.markdown is not null) execute function document_block_versions_owe();
`

// A migration is exempt from the table limit only when Postgres's parser reads every one of its
// statements as a catalog-only kind. What a regular expression over the text would be fooled by -
// a trigger in a comment, in a string literal, in a dollar-quoted function body, or beside another
// statement on one line - is read as the server reads it, and a migration that adds an index or a
// column default, or holds any statement outside the allowlist, is not exempt.
func TestTableSizeExemptionReadsStatementsAsPostgresParsesThem(t *testing.T) {
	for name, tc := range map[string]struct{ sql, want string }{
		"0122, a function and a trigger":  {migration0122, "create function and create trigger"},
		"a trigger alone":                 {"create trigger t_owed after insert on things for each row execute function owe();", "create trigger"},
		"a constraint trigger":            {"create constraint trigger t_check after insert on things deferrable initially deferred for each row execute function check_things();", "create trigger"},
		"or replace trigger":              {"create or replace trigger t_owed after update on things for each row execute function owe()", "create trigger"},
		"a procedure":                     {"create procedure p() language sql as $$ select 1 $$", "create function"},
		"a function in sql":               {"create function f() returns int language sql as $$ select 1 $$", "create function"},
		"a function with a standard body": {"create function f() returns int begin atomic select 1; end", "create function"},
		"a function in uppercase PLPGSQL": {"create function f() returns void language PLPGSQL as $$ begin end $$", "create function"},
		// Creating a C function loads its library, which can run any code; another language's
		// validator is not known to stay in the catalog.
		"a function in c":      {"create function f() returns int language c as 'mylib', 'f'", ""},
		"a function in plperl": {"create function f() returns int language plperl as $$ return 1 $$", ""},
		"a function and a trigger on one line": {
			"create function owe() returns trigger language plpgsql as $$ begin return null; end $$; create trigger t after insert on things for each row execute function owe();",
			"create function and create trigger",
		},
		// A function body is one string to the parser, whatever statements it spells: defining the
		// function runs none of them.
		"an index inside a dollar-quoted body": {"create function f() returns void language plpgsql as $body$ begin create index i on things (kind); end $body$;", "create function"},

		"an index":                                  {"create index things_kind on things (kind);", ""},
		"an index concurrently":                     {"create index concurrently things_kind on things (kind)", ""},
		"a column with a default":                   {"alter table things add column note text default 'x';", ""},
		"a default set on a column":                 {"alter table things alter column kind set default 'x';", ""},
		"a trigger and an index":                    {"create trigger t after insert on things for each row execute function owe(); create index i on things (kind);", ""},
		"two statements on one line":                {"create trigger t after insert on things for each row execute function owe(); alter table things add column n int default 0", ""},
		"a trigger only in a comment":               {"-- create trigger t after insert on things for each row execute function owe();\ncreate index i on things (kind);", ""},
		"a trigger only in a block comment":         {"/* create trigger t after insert on things\n for each row execute function owe(); */ create index i on things (kind)", ""},
		"a trigger only in a string literal":        {"update things set kind = 'create trigger t after insert on things for each row execute function owe()'", ""},
		"a trigger only in a dollar-quoted literal": {"select $$; create trigger t after insert on things for each row execute function owe(); $$", ""},
		"a trigger inside a DO block":               {"do $$ begin create trigger t after insert on things for each row execute function owe(); end $$;", ""},
		// A set is a parameter of the transaction: it takes no lock and reads no row, so it
		// cannot make a lock window grow with a table, and a migration that bounds its own lock
		// wait keeps the exemption its other statements earn.
		"a set before a trigger":       {"set local lock_timeout = '1s'; create trigger t after insert on things for each row execute function owe();", "set and create trigger"},
		"a set and nothing else":       {"set local lock_timeout = '1s';", "set"},
		"a set before an index":        {"set local lock_timeout = '1s'; create index i on things (kind);", ""},
		"a comment and nothing else":   {"-- create trigger t after insert on things for each row execute function owe();\n", ""},
		"no text":                      {"", ""},
		"a statement Postgres refuses": {"create trigger t after insert on things for each row", ""},
	} {
		if got := tableSizeExemption(tc.sql); got != tc.want {
			t.Errorf("%s: tableSizeExemption = %q, want %q", name, got, tc.want)
		}
	}
}

// A refusing holder's query text keeps the statement's shape and names and loses every value a
// row could have put there: literals of every quoting, numbers with what follows them unspaced,
// and comments. A parameter's number stays, and a text cut off inside a literal loses the rest.
func TestRedactedQueryKeepsTheShapeAndDropsEveryValue(t *testing.T) {
	for in, want := range map[string]string{
		"select id from asks where id = $1 for no key update":                   "select id from asks where id = $1 for no key update",
		"update asks set body = 'ghp_SECRET' where id = 42":                     "update asks set body = ? where id = ?",
		"select E'it\\'s', $tag$ x $tag$, U&'d\\0061t', \"Quoted Name\" from t": "select ?, ?, ?, \"Quoted Name\" from t",
		"select 1.5e3, 0x1F, -7 -- ghp_COMMENT\n/* ghp_BLOCK */ from t":         "select ?, ?, -? from t",
		"insert into asks (body) values ('cut off by track_activity_quer":       "insert into asks (body) values (?",
		"  select\n\t  1  ": "select ?",
	} {
		if got := redactedQuery(in); got != want {
			t.Errorf("redactedQuery(%q) = %q, want %q", in, got, want)
		}
	}
}
