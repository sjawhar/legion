package pgmigrate

import (
	"strings"
	"testing"
)

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
// column default, or holds any statement outside the allowlist, is not exempt. A drop is exempt
// only with RESTRICT, the default: what a cascade drops besides the object the statement names is
// not in the statement, so the census cannot read it.
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

		// Dropping a trigger writes pg_trigger and dropping a function writes pg_proc, reading no
		// row of the table, so a drop with RESTRICT - the default - is exempt as a create is.
		"20261102233626, a dropped trigger and its function": {"drop trigger artifact_versions_embeddings_enqueue on artifact_versions;\ndrop function embeddings_enqueue_document();", "drop trigger and drop function"},
		"a dropped trigger alone":                            {"drop trigger t_owed on things;", "drop trigger"},
		"a dropped trigger if exists":                        {"drop trigger if exists t_owed on things;", "drop trigger"},
		"a dropped function alone":                           {"drop function owe();", "drop function"},
		"a drop beside a created trigger":                    {"drop trigger t_old on things; create trigger t_owed after insert on things for each row execute function owe();", "drop trigger and create trigger"},

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
		// A drop of anything else reads or removes rows, and a drop with cascade reaches objects
		// the statement does not name, so neither is exempt.
		"a dropped table":                 {"drop table things;", ""},
		"a dropped index":                 {"drop index things_kind;", ""},
		"a dropped view":                  {"drop view v;", ""},
		"a dropped schema":                {"drop schema s;", ""},
		"a dropped procedure":             {"drop procedure p();", ""},
		"a dropped function with cascade": {"drop function owe() cascade;", ""},
		"a dropped trigger with cascade":  {"drop trigger t_owed on things cascade;", ""},
		"a drop beside an index":          {"drop trigger t_owed on things; create index things_kind on things (kind);", ""},
		"a drop only in a comment":        {"-- drop trigger t_owed on things;\ncreate index things_kind on things (kind);", ""},
		"a drop only in a string literal": {"update things set kind = 'drop trigger t_owed on things'", ""},
		// A set is a parameter of the transaction: it takes no lock and reads no row, so it
		// cannot make a lock window grow with a table, and a migration that bounds its own lock
		// wait keeps the exemption its other statements earn.
		"a set before a trigger":       {"set local lock_timeout = '1s'; create trigger t after insert on things for each row execute function owe();", "set and create trigger"},
		"a set before a drop":          {"set local lock_timeout = '1s'; drop trigger t_owed on things;", "set and drop trigger"},
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
// row could have put there: literals of every quoting, numbers whole as scan.l reads them, and
// comments. A parameter's number stays, and a text cut off inside a literal loses the rest.
// The holder session owns standard_conforming_strings and its query text does not carry it, so
// each row names the setting it answers for. A text whose literals hold no backslash reads the
// same under both, and redactedQuery answers it with the shape intact. A backslash before a
// closing quote is read as that quote's escape with the setting off and as a plain byte with it
// on, which moves where every later literal starts - and, with them, where a comment opened in
// the bytes between them starts - so the two readings disagree over which bytes are a value:
// the answer covers the literals and the comments of both, which can swallow the shape between
// them - a comma, or a whole literal - and leaves no part of any value behind under either. A
// comment both readings read as a comment is the one space it has always been. Only a plain
// literal and the '…' an N precedes take the setting: E'…' escapes under both, U&'…' is
// refused outright with the setting off, and B'…' and X'…' hold no escape at all, so the two
// readings end those in the same place and a value next to one of them is covered rather than
// widened over.
//
// Where scan.l refuses the text - trailing junk after a number, U&'…' with the setting off, an
// unterminated literal - Postgres read no further, so from that byte on the row's answer is one
// ? rather than the shape: a session refused inside a transaction keeps every lock it took,
// sits idle in transaction (aborted), and pg_stat_activity holds the refused text, which is the
// session a migration queues behind. A refusal the parser raises and the lexer does not, as
// 1..10 outside a PL/pgSQL for loop is, moves nothing: where the literals of a text are is the
// lexer's and does not depend on the parser reaching them.
func TestRedactedQueryKeepsTheShapeAndDropsEveryValue(t *testing.T) {
	for name, tc := range map[string]struct {
		// setting is the holder's standard_conforming_strings the row answers for: "on", the
		// default; "off"; or "on and off" for a text both settings read the same way.
		setting, in, want string
	}{
		"a parameter's number stays": {"on and off", "select id from asks where id = $1 for no key update", "select id from asks where id = $1 for no key update"},
		"a literal and a number go":  {"on and off", "update asks set body = 'ghp_SECRET' where id = 42", "update asks set body = ? where id = ?"},
		// U&'…' is refused outright with the setting off ({xusstart} raises "unsafe use of
		// string constant with Unicode escapes" before reading it), so under that reading
		// Postgres read nothing after it and everything from it is covered.
		"every other quoting goes":        {"on and off", "select E'it\\'s', $tag$ x $tag$, U&'d\\0061t', \"Quoted Name\" from t", "select ?, ?, ?"},
		"numbers and comments go":         {"on and off", "select 1.5e3, 0x1F, -7 -- ghp_COMMENT\n/* ghp_BLOCK */ from t", "select ?, ?, -? from t"},
		"a text cut off inside a literal": {"on and off", "insert into asks (body) values ('cut off by track_activity_quer", "insert into asks (body) values (?"},
		"a run of space becomes one":      {"on and off", "  select\n\t  1  ", "select ?"},
		// Two backslashes escape each other with the setting off, so that literal ends where
		// it ends with the setting on and the literal after it is a value of its own: both
		// readings agree, and the statement keeps its shape.
		"two backslashes escape each other":       {"on and off", `update asks set body = 'a\\', note = 'ghp_SECOND' where id = 7`, "update asks set body = ?, note = ? where id = ?"},
		"a backslash away from the closing quote": {"on and off", `select 'C:\path' , 'ghp_SECRET'`, "select ? , ?"},
		// With the setting off the first literal runs on to the next quote, which swallows
		// ghp_SECRET; with it on the first literal ends at the backslash's quote and the one
		// opened at the next quote is unterminated, which swallows the rest of the text. Every
		// byte after body = is inside a literal of one reading or the other, so one ? is all
		// that may be printed.
		"off: a literal carried on to the next quote": {"off", `update asks set body = 'a\' , ghp_SECRET , '  where id = 1`, "update asks set body = ?"},
		// scan.l's {xnstart} eats only the n ("yyless(1); eat only 'n' this time") and returns
		// it as NCHAR or as the identifier n; the quote after it opens a literal of its own,
		// which is why the n is a word here and the literal that follows takes the setting.
		// That n is a token PostgreSQL itself returns, not a fragment of a literal: printing it
		// keeps a name, as the report keeps every name, and hides no byte of any value.
		"off: a national-character literal carried on": {"off", `select n'a\' , ghp_NCHAR , ' from t`, "select n?"},
		// The default setting, where the backslash is a plain byte: the holder's own reading
		// ends the first literal at its quote and opens the next at the next quote, so the
		// value between them must go, and with it whatever shape the two readings disagree on.
		"on: a path before another literal":          {"on", `select 'C:\' , 'ghp_SECRET' , 'b'`, "select ??"},
		"on: a path in an insert's values":           {"on", `insert into t values ('C:\', 'ghp_SECRET')`, "insert into t values (??"},
		"on: a path in a set, with a where after it": {"on", `update t set a = 'x\', b = 'ghp_SECRET' where c = 'y'`, "update t set a = ??"},
		// A comment one reading opens inside what the other reads as a literal hides a value
		// as surely as a literal does: Postgres ran none of its text, and whoever wrote the
		// statement put in it whatever they liked. With the setting off the literal here ends
		// at the quote after Brien and a block comment follows it; with it on, the comment's
		// user='alice' is read as a literal, a word and a literal. The answer covers the other
		// reading's comments as well as its literals, so neither printing of alice survives.
		"off: a block comment the carried literal opens": {"off", `select 'O\'Brien' /*user='alice'*/`, "select ??"},
		"off: a line comment the carried literal opens":  {"off", `select 'x\', '-- ', ghp_TOKEN`, "select ??"},
		// The same disagreement where nothing closes what it opens: with the setting on the
		// literal the backslash's quote opens is unterminated and takes the rest of the text,
		// and with it off the comment does, so one ? covers the tail under either reading.
		"off: a line comment to the end of the text":  {"off", `select 'a\' -- ghp_LINE`, "select ?"},
		"off: a block comment over the rest of a set": {"off", `update t set a = 'b\' /* ghp_BLOCK */ where c = 1`, "update t set a = ?"},
		// The space before that ? is this reading's own and lies outside every span of the
		// other, so it is printed as the one space it is: a comment of the other reading over
		// bytes this reading prints nothing for hides nothing and adds no ? of its own, which
		// is why a comment both readings read as a comment still collapses to a space.
		"off: a line comment opened between two literals": {"off", `select 'a\' , '-- x' , ghp_S`, "select ? ?"},
		// Postgres reads B'…' and X'…' in scan.l's xb and xh states, whose inside is [^']*:
		// neither a backslash nor a doubled quote escapes anything there, under either setting,
		// so such a literal ends at its first quote and only a quote continuation carries it on.
		// PostgreSQL 18.6 answers select B'1''1' with syntax error at or near "'1'" under both
		// settings, and select X'41\' , ghp_SECRET , 'x' from from with syntax error at the
		// second from under both: the backslash is a byte of the literal, which ended at the
		// quote after it, and what follows is read as structure. So both readings end a bit or
		// hex string in the same place, and the literal after one of them is a value of its own.
		"a hex string and the literal after it":      {"on and off", `select x'ff' , 'ghp_PLAIN'`, "select ? , ?"},
		"a continuation carries a hex string on":     {"on and off", "select X'41'\n'42' , 'ghp_PLAIN'", "select ? , ?"},
		"a doubled quote ends a bit string":          {"on and off", `select B'1''1' , 'ghp_PLAIN'`, "select ?? , ?"},
		"a backslash is a byte in a bit string":      {"on and off", `select b'0\' /* ghp_BLOCK */ , 'x'`, "select ? , ?"},
		"a name after a bit or hex string is a name": {"on and off", `select X'\' , ghp_SECRET , '`, "select ? , ghp_SECRET , ?"},
		"a name after a bit string is a name":        {"on and off", `select B'\' , ghp_SECRET , '`, "select ? , ghp_SECRET , ?"},
		// A name after a U&'…' stood here until the refused class was read as what it is: with
		// the setting off, scan.l's {xusstart} raises "unsafe use of string constant with
		// Unicode escapes" before reading the literal at all, so Postgres read nothing after
		// the U& and one ? covers the rest of the text under that reading.
		"a Unicode-escaped string covers what follows it": {"on and off", `select U&'a\' , ghp_SECRET , 'x'`, "select ?"},
		// The backslash that matters sits in the plain literal the hex string is followed by,
		// not in the hex string: with the setting off that literal takes it as its quote's
		// escape and runs on through ghp_SECRET to the next quote, which PostgreSQL 18.6 shows
		// for select 'a\' , ghp_SECRET , 'x' from from - unterminated hexadecimal string
		// literal at or near "x' from from", the marker having been swallowed - while with the
		// setting on the same text answers syntax error at the second from. So the marker is a
		// value under the off reading and a name under the on reading, and one ? covers it.
		"off: a hex string before a literal carried on":         {"off", "select X'\\''--\\'\n ghp_SECRET \\'' ;", "select ?? ;"},
		"off: a hex string before a literal carried to the end": {"off", "select X'\\''--\\'\n ghp_SECRET '' ;", "select ??"},
		// A number ends where scan.l ends it and nowhere else. {numericfail} throws back the
		// two dots of 1..e'…' and returns the 1, so what follows is read from the dots: an
		// E'…' literal after them is a literal, not the plain '…' a reading that ran the
		// number over the dots and the e would find there. PostgreSQL 18.6 answers
		// select 1..e'\'' 'a\' , ' ghp_SECRET ' from from with syntax error at or near ".."
		// and select 1.5.e'\'' … with syntax error at or near "." under both settings, the
		// numbers being 1 and 1.5 and the dots tokens of their own; the parser stops there and
		// the lexer does not, so the literals after the dots are read as they are written. The
		// values are covered either way: under the setting on by this reading's own literals,
		// under it off by the one the backslash carries on.
		"a number before two dots and an E literal": {"on and off", `select 1..e'\'' 'a\' , ' ghp_SECRET '`, "select ?..? ?"},
		"a number before a dot and an E literal":    {"on and off", `select 1.5.e'\'' 'a\' , ' ghp_SECRET '`, "select ?.? ?"},
		"a number before two dots and a literal":    {"on and off", `select 1..'a\' , ' ghp_SECRET '`, "select ?..?"},
		"a real before a literal carried on":        {"on and off", `select 1.5e3, 'a\' , ' ghp_SECRET '`, "select ?, ?"},
		"a hexadecimal before a literal carried on": {"on and off", `select 0x1F 'a\' , ' ghp_SECRET '`, "select ? ?"},
		// {integer_junk} is where scan.l raises "trailing junk after numeric literal": Postgres
		// read no further, so the rest of the text is covered rather than printed.
		"trailing junk after a number covers the rest": {"on and off", `select 1abc , ghp_SECRET , 'x'`, "select ?"},
		// A refusal of the lexer's covers the rest of the text; a refusal of the parser's, or of
		// anything later, moves nothing, a literal's extent being the lexer's. The rows below
		// are the two kinds side by side, each measured against a PostgreSQL 18.6.
		//
		// {integer_junk} is {decinteger}{identifier}, and flex takes the longest match of the
		// whole rule: 1_0$$ is the number 1 and the identifier _0$$, not the number 1_0 and a
		// dollar quote, so this refusal moves a literal's boundary as well as the failure - the
		// dollar-quoted literal a reading of the number alone would find opens at the second $$,
		// which Postgres never reaches. 18.6 answers trailing junk after numeric literal at or
		// near "1_0$$".
		"the junk of a number before an underscore": {"on and off", "select 1_0$$ x $$ ghp_SECRET", "select ?"},
		// The same number with no junk after it is a number Postgres reads: select 1_0 answers
		// 10, so the name after it is a name and this row keeps it.
		"an underscore inside a number is no refusal": {"on and off", "select 1_0 ghp_SECRET", "select ? ghp_SECRET"},
		// {param} reads the digits after the $ with pg_strtoint32_safe and raises "parameter
		// number too large" where no int32 holds them. The $1 of the first row of this table is
		// a parameter Postgres reads, and stays.
		"a parameter number no int32 holds": {"on and off", "select $999999999999 ghp_SECRET", "select ?"},
		// The <xqs> action verifies a literal whose escapes made a byte that is not 7-bit ASCII
		// (pg_verifymbstr): \400 is a zero byte and \xff is no UTF-8, and 18.6 answers both with
		// invalid byte sequence for encoding "UTF8". Escapes that spell a character the encoding
		// holds make a literal it reads, and the name after that is a name.
		"an escape that makes a zero byte":       {"on and off", `select e'\400' ghp_SECRET`, "select ?"},
		"an escape that makes a high byte":       {"on and off", `select e'\xff' , ghp_SECRET`, "select ?"},
		"escapes that make a character it holds": {"on and off", `select e'\xc3\xa9' , ghp_SECRET`, "select ? , ghp_SECRET"},
		// addunicode refuses a codepoint outside 0 < c <= 0x10FFFF with "invalid Unicode escape
		// value", which 18.6 answers for both of the first two. A surrogate pair that spells a
		// character is read; <xeu>. refuses every other byte after a first half, a letter and a
		// carriage return alike, since flex's "." is [^\n] and a carriage return is a byte it
		// takes - 18.6 answers invalid Unicode surrogate pair at or near that byte for each.
		//
		// The last four rows are two pairs, each pair one byte apart in the same position, and
		// the second of each pair is the one that matters: where the refusing byte is the last
		// before the closing quote the tail is covered because the literal never ends, which
		// hides a reading that does not refuse at all, and where the escape recovers and the
		// literal terminates the tail survives to be printed unless the refusal is made. A
		// reading whose <xeu>. did not take a carriage return printed that tail.
		"a codepoint no escape value holds":                     {"on and off", `select e'\U0011FFFF' , ghp_SECRET`, "select ?"},
		"the codepoint zero":                                    {"on and off", `select e'\u0000' , ghp_SECRET`, "select ?"},
		"a surrogate pair that spells a character":              {"on and off", `select e'\ud83d\ude00' , ghp_SECRET`, "select ? , ghp_SECRET"},
		"a letter where a second half was wanted":               {"on and off", `select e'\ud83dX' , ghp_SECRET`, "select ?"},
		"a letter, and then the literal recovers":               {"on and off", `select e'\ud83dX\udc00' , ghp_SECRET`, "select ?"},
		"a carriage return where a second half was wanted":      {"on and off", "select e'\\ud83d\r' , ghp_SECRET", "select ?"},
		"a carriage return, and then the literal recovers":      {"on and off", "select e'\\ud83d\r\\udc00' , ghp_SECRET", "select ?"},
		"a carriage return inside a literal that is no refusal": {"on and off", "select e'a\rb' , ghp_SECRET", "select ? , ghp_SECRET"},
		// {operator} refuses a token of NAMEDATALEN bytes or more with "operator too long". One
		// byte short of it is a token the lexer hands over and the parser complains about, which
		// moves nothing.
		"an operator of NAMEDATALEN bytes": {"on and off", "select 1 " + strings.Repeat("@", 64) + " ghp_SECRET", "select ? ?"},
		"an operator one byte shorter":     {"on and off", "select 1 " + strings.Repeat("@", 63) + " ghp_SECRET", "select ? " + strings.Repeat("@", 63) + " ghp_SECRET"},
		// Between tokens a carriage return is {space}: select \r 1 answers 1 on 18.6, before the
		// number and after it alike, so both rows keep the name. That holds in INITIAL and says
		// nothing about the states a literal is read in, where the same byte can be the one a
		// rule refuses - the two rows above it are that case, and reading this one as general is
		// what left that refusal out for two rounds.
		"a carriage return before a number": {"on and off", "select \r 1 ghp_SECRET", "select ? ghp_SECRET"},
		"a carriage return after a number":  {"on and off", "select 1 \r ghp_SECRET", "select ? ghp_SECRET"},
		// Three refusals that are not the lexer's, which 18.6 answers with syntax error at or
		// near "..", syntax error at or near "$" and column "u" does not exist. {numericfail}
		// throws the two dots back and returns the number, {dolqfailed} returns the $ alone and
		// {xufailed} returns the u, so every literal of these texts is where it was and the tail
		// after the refusal is printed.
		"two dots after a number are the parser's":   {"on and off", "select 1.. ghp_SECRET", "select ?.. ghp_SECRET"},
		"a dollar that opens no tag is the parser's": {"on and off", "select $notatag ghp_SECRET", "select $notatag ghp_SECRET"},
		"a U& that opens no literal is a name":       {"on and off", "select u&x ghp_SECRET", "select u&x ghp_SECRET"},
	} {
		if got := redactedQuery(tc.in); got != tc.want {
			t.Errorf("%s (standard_conforming_strings %s): redactedQuery(%q) = %q, want %q", name, tc.setting, tc.in, got, tc.want)
		}
	}
}
