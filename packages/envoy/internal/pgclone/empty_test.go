package pgclone

// The empty pool's own tests (Config.Empty, empty.go). Its promise is the one a migration test
// rests on: a database it migrated comes back the way `create database` made it, or it does not
// come back at all.
//
// Each test reads the database it was handed by name, which the free list only answers while
// nothing else is taking from it, so none of them calls t.Parallel - for the reason pgclone_test.go
// gives. Each also reads what a fresh database of this server holds rather than a list written
// here: the server's template1 may carry an extension of its own (this repository's lanes run one
// that carries pgvector, which migration 0072 then finds already installed), so what "empty"
// means is whatever `create database` makes, which only the server can say.

import (
	"context"
	"testing"
)

// emptyTestPool hands these tests their databases, as empties does in internal/dispatch/store.
// Its template carries no migration, so Migrate and SeedSchema are unset.
var emptyTestPool = New(Config{
	Name:           "pgclone's own empty pool",
	DatabaseURLEnv: "DISPATCH_TEST_DATABASE_URL",
	SkipMessage:    "DISPATCH_TEST_DATABASE_URL must be set to run the pgclone tests",
	DatabasePrefix: "pgclone_empty_test_",
	TemplatePrefix: "pgclone_empty_template_",
	Empty:          true,
})

// migrationLeavings is what a migration test leaves in a database it was handed empty: a schema of
// its own, and one object of every kind emptyDrops names that a role without superuser may create.
// It is deliberately wider than Dispatch's migrations - which leave tables, indexes, views, a
// materialised view, sequences, functions, triggers, constraints and an extension - because a
// reset that only removed those would be a reset that a migration adding an enum or a collation
// silently defeated, and a defeated reset is a dropped database and no saying so.
//
// The three kinds a test cannot leave here are each noted where they are handled: an extension and
// an event trigger need a right this server need not grant, so they have a test of their own
// below, and an operator class needs a superuser, so emptyDrops does not name it and the
// fingerprint refuses the database instead.
const migrationLeavings = `
create table widgets (id bigint generated always as identity primary key, label text not null);
create index widgets_by_label on widgets (label);
create view live_widgets as select * from widgets;
create materialized view counted_widgets as select count(*) as held from widgets;
create sequence batches;
create type mood as enum ('ok', 'bad');
create domain positive as int check (value > 0);
create type pair as (left_of int, right_of int);
create function note_widget() returns trigger language plpgsql as $$ begin return new; end $$;
create trigger widgets_noted before insert on widgets for each row execute function note_widget();
alter table widgets add constraint widgets_label_short check (length(label) < 64);
create procedure note_nothing() language sql as $$ select 1 $$;
create aggregate total (int) (sfunc = int4pl, stype = int);
create operator === (leftarg = int, rightarg = int, function = int4eq);
create collation strictly from "C";
create conversion latin1_to_utf8 for 'LATIN1' to 'UTF8' from iso8859_1_to_utf8;
create text search configuration words (copy = simple);
create statistics widgets_shape on id, label from widgets;
create schema staging;
create table staging.inbox (id int);
insert into widgets (label) values ('written by the migration');
select lo_create(0)`

// inventoryQuery is the whole of a database as the test after it would meet it: every object
// schema public holds that no extension owns, the schemas, the extensions, the event triggers,
// the publications, how many large objects it carries, how many settings it carries of its own,
// and how many default privileges. It is one line so that a failure names what the next test
// would be handed rather than a column of it, and it is read from the same catalogs the reset
// reads, since those are where "empty" is written down.
const inventoryQuery = `select format(
	'objects of public=%s / schemas=%s / extensions=%s / event triggers=%s / publications=%s / large objects=%s / settings=%s / default privileges=%s',
	(select coalesce(string_agg(format('%s %s', object.type, object.identity), ', '
			order by object.type, object.identity), 'none')
		from pg_depend dependency,
			lateral pg_identify_object(dependency.classid, dependency.objid, 0) object
		where dependency.refclassid = 'pg_namespace'::regclass
			and dependency.refobjid = 'public'::regnamespace and dependency.deptype = 'n'
			and not exists (
				select 1 from pg_depend member
				where member.classid = dependency.classid and member.objid = dependency.objid
					and member.deptype = 'e')),
	(select string_agg(nspname, ', ' order by nspname) from pg_namespace
		where nspname not like 'pg\_%' and nspname <> 'information_schema'),
	(select string_agg(extname, ', ' order by extname) from pg_extension),
	(select coalesce(string_agg(evtname, ', ' order by evtname), 'none') from pg_event_trigger),
	(select coalesce(string_agg(pubname, ', ' order by pubname), 'none') from pg_publication),
	(select count(*) from pg_largeobject_metadata),
	(select count(*) from pg_db_role_setting setting
		join pg_database database on database.oid = setting.setdatabase
		where database.datname = current_database()),
	(select count(*) from pg_default_acl))`

// freshInventory is what a database this pool has just made holds, read from one and then dropped.
// It is the answer every reset is judged against: a list written here would say what this file
// believes a fresh database holds, and the point of the reset is what the server makes.
func freshInventory(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	fresh := emptyTestPool.Take(t, false)
	conn := connect(t, emptyTestPool, fresh)
	inventory := probe(t, conn, inventoryQuery)
	conn.Close(ctx)
	emptyTestPool.Drop(fresh)
	return inventory
}

// The whole promise: a database a migration test changed - a schema of its own, an object of every
// kind the reset knows, rows, a large object, a setting on the database and a setting on every role
// in it - comes back holding exactly what a fresh database holds, and comes back rather than being
// dropped. Broken, it reads either "was dropped", which is the whole saving gone and silently,
// since a dropped database is silent by design, or a reused database still holding the migration's
// tables, types, collation, schema, large object or statement_timeout - which the test that takes
// it next migrates into.
func TestADatabaseAMigrationTestChangedComesBackEmpty(t *testing.T) {
	ctx := context.Background()
	want := freshInventory(t)

	name := emptyTestPool.Take(t, true)
	conn := connect(t, emptyTestPool, name)
	exec(t, conn, migrationLeavings)
	// Both shapes a setting takes: on the database for every role (pg_db_role_setting's setrole
	// 0) and on every role in the database. internal/dispatch/store's ask-reply backfill tests
	// leave the first, and either is carried by every session opened after it, this reset's own
	// among them.
	exec(t, conn, "alter database "+name+" set statement_timeout = '20s'")
	exec(t, conn, "alter role all in database "+name+" set search_path = staging, public")
	left := probe(t, conn, inventoryQuery)
	if left == want {
		t.Fatalf("what the migration left reads the same as a fresh database (%s), so this test holds the reset to nothing", want)
	}

	if outcome := release(t, emptyTestPool, name, func() { conn.Close(ctx) }); outcome != reused {
		t.Fatalf("the database of a test that migrated was %s; every migration test would pay a create database and a drop database of its own", outcome)
	}
	again := emptyTestPool.Take(t, true)
	if again != name {
		t.Fatalf("the next test was handed %s, not the emptied %s the free list held", again, name)
	}
	back := connect(t, emptyTestPool, again)
	defer release(t, emptyTestPool, again, func() { back.Close(ctx) })
	if got := probe(t, back, inventoryQuery); got != want {
		t.Fatalf("the reused database holds\n\t%s\nwhere a fresh one holds\n\t%s\nand the migration had left\n\t%s", got, want, left)
	}
}

// The two leavings behind a privilege the server a run is pointed at need not grant, each read on a
// database of its own so that a server refusing one still holds the reset to the other. An
// extension is what Dispatch's migration 0072 installs, and an event trigger is what
// internal/dispatch/store's census tests leave; a reused database carrying either runs code the
// next test never wrote, which is the failure the fingerprint was widened for.
func TestTheExtensionAndTheEventTriggerAMigrationTestLeftAreDropped(t *testing.T) {
	want := freshInventory(t)
	for _, leaving := range []struct{ name, statements string }{
		{"an extension", "create extension pgcrypto"},
		{"an event trigger", `
			create function note_ddl() returns event_trigger language plpgsql as $$ begin end $$;
			create event trigger ddl_noted on ddl_command_end execute function note_ddl()`},
	} {
		t.Run(leaving.name, func(t *testing.T) {
			ctx := context.Background()
			name := emptyTestPool.Take(t, true)
			conn := connect(t, emptyTestPool, name)
			if _, err := conn.Exec(ctx, leaving.statements); err != nil {
				conn.Close(ctx)
				emptyTestPool.Release(name, nil)
				t.Skipf("this server refuses %s to the role the tests run as: %v", leaving.name, err)
			}
			left := probe(t, conn, inventoryQuery)
			if left == want {
				t.Fatalf("%s left the database reading the same as a fresh one (%s)", leaving.name, want)
			}

			if outcome := release(t, emptyTestPool, name, func() { conn.Close(ctx) }); outcome != reused {
				t.Fatalf("the database of a test that left %s was %s", leaving.name, outcome)
			}
			again := emptyTestPool.Take(t, true)
			if again != name {
				t.Fatalf("the next test was handed %s, not the emptied %s the free list held", again, name)
			}
			back := connect(t, emptyTestPool, again)
			defer release(t, emptyTestPool, again, func() { back.Close(ctx) })
			if got := probe(t, back, inventoryQuery); got != want {
				t.Fatalf("the reused database holds\n\t%s\nwhere a fresh one holds\n\t%s", got, want)
			}
		})
	}
}

// The other half of the promise, and the half that makes the first one safe to rely on: a leaving
// the reset does not remove is refused the pool rather than handed on. A default privilege granted
// with no schema is one - reversing it needs the grant's own shape, which nothing here has - and it
// is in the fingerprint for exactly that reason. Broken - the term cut from fingerprintQuery - this
// reads "was put back on the free list", and every table the next test creates is granted to public
// by a rule it never wrote.
func TestADatabaseHoldingALeavingTheResetCannotRemoveIsRefused(t *testing.T) {
	ctx := context.Background()
	name := emptyTestPool.Take(t, true)
	conn := connect(t, emptyTestPool, name)
	exec(t, conn, "alter default privileges grant select on tables to public")
	if outcome := release(t, emptyTestPool, name, func() { conn.Close(ctx) }); outcome != refused {
		t.Fatalf("a database holding a default privilege the reset cannot remove was %s", outcome)
	}
}
