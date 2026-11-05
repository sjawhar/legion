package pgclone

// An empty pool's reset (Config.Empty). A migrated pool's database comes back by having its rows
// put back, which a test that migrates defeats: it leaves a different schema behind, and the
// fingerprint drops such a database by design. The databases those tests want carry no migration
// at all, so their reset has an easier question to answer - make it empty again - and this file is
// that answer.
//
// The question it asks a database is one query (leftoversQuery), and the fingerprint asks the
// same one again afterwards: which objects does schema public hold that are not members of an
// extension, and which schemas, extensions, event triggers and publications does the database
// hold that a fresh one does not. The objects of a schema are read from pg_depend rather than
// from a list of catalogs, so no kind is missed - a collation, an operator, an extended
// statistics object and a text search configuration are each in the answer, and none of them is
// in pg_class - and pg_identify_object names each one in the form its own DROP takes.
//
// Why not one `drop schema public cascade`, which would need no enumeration at all: the server's
// template1 can hold an extension in schema public (the one this repository's lanes run holds
// pgvector, which migration 0072 then finds already installed), the drop takes it with the
// schema, and putting it back needs a privilege this harness's role has not got - `create
// extension vector` is answered "permission denied to create extension" for an ordinary role. So
// the schema stays and its objects go, which also leaves its owner, its grants and its comment
// exactly as `create database` made them.
//
// What a test can leave that this reset does NOT remove, and what happens to it:
//
//   - An object of a kind emptyDrops does not name - an operator class or an operator family,
//     each of which needs a superuser to create. It is left where it is and the fingerprint,
//     asking the same question, refuses the database to the pool.
//   - A role. CREATE ROLE is cluster-wide: it is in no database, so no database's reset can
//     remove it, and it outlives the run either way, exactly as it does today when the database is
//     dropped (internal/dispatch/store's census tests make several).
//   - A subscription. Dropping one needs its replication slot handled and rights this harness's
//     role need not have. The fingerprint carries this database's subscriptions, so a test that
//     leaves one is refused the pool.
//   - A default privilege granted with no schema (alter default privileges without `in schema`).
//     Reversing one needs the grant's own shape. The fingerprint carries pg_default_acl, so it is
//     refused.
//   - A grant on the database itself, or a change of its owner. The fingerprint carries the
//     pg_database row, so it is refused.
//
// Everything else a test leaves is removed, and the fingerprint is read after the reset rather
// than before it so that a reset which did not manage it hands the database to the drops instead
// of to the next test.

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The template's own reading, which prepareEmpty takes: what a fresh database of this server
// holds. Each is what a reset has to leave behind.
const (
	templateSchemas = `
		select nspname from pg_namespace
		where nspname not like 'pg\_%' and nspname <> 'information_schema'
		order by nspname`
	templateExtensions    = `select extname from pg_extension order by extname`
	templateEventTriggers = `select evtname from pg_event_trigger order by evtname`
	templatePublications  = `select pubname from pg_publication order by pubname`
)

// publicObjects is the branch leftoversQuery and the fingerprint share: every object schema public
// holds that no extension owns, as the type and the identity pg_identify_object gives it - the
// identity being the form that object's own DROP takes, schema-qualified and quoted.
//
// It reads pg_depend because that is where an object's schema is recorded for every kind at once:
// a table, a view, a sequence, a type, a routine, an operator, a collation, a conversion, an
// extended statistics object and a text search configuration each carry one normal dependency on
// the namespace they were created in, and nothing else does - an index and a trigger depend on
// their table, a constraint on its table, a column default on its column, so none of them is
// listed twice over here.
//
// An extension's own members are left out, by the 'e' dependency every one of them carries: a
// fresh database already holds a few hundred of them where template1 installs an extension into
// public, they go and come back with the extension rather than one at a time, and the extension
// branch beside this one is what holds the set of extensions to the template's.
const publicObjects = `
		select object.type as kind, object.identity as name
			from pg_depend dependency,
				lateral pg_identify_object(dependency.classid, dependency.objid, 0) object
			where dependency.refclassid = 'pg_namespace'::regclass
				and dependency.refobjid = 'public'::regnamespace
				and dependency.deptype = 'n'
				and not exists (
					select 1 from pg_depend member
					where member.classid = dependency.classid and member.objid = dependency.objid
						and member.deptype = 'e')`

// emptyObjects is publicObjects as a branch of an empty pool's fingerprint, and nothing at all on
// a migrated pool's: it is how the fingerprint read after a reset checks that the objects of
// schema public are gone, down to a kind emptyDrops could not remove. fingerprintQuery says why
// a migrated pool does not carry it.
func (p *Pool) emptyObjects() string {
	if !p.config.Empty {
		return ""
	}
	return `
		union all
		select format('in public|%s|%s', kind, name) from (` + publicObjects + `
		) held`
}

// emptyDrop is how one kind of object of schema public is removed: the statement, whether that
// statement takes cascade, and where it runs among the others.
type emptyDrop struct {
	at      int
	command string
	cascade bool
}

// emptyDrops maps what pg_identify_object calls an object of schema public to the drop that
// removes it. A kind missing from it is left where it is, and the fingerprint then refuses the
// database, so the cost of a kind nobody thought of is a database dropped rather than a database
// handed on dirty.
//
// The order is what keeps the statements from needing each other's leavings: a table before the
// sequence its identity column owns, which cannot be dropped while that column wants it, and
// before the type one of its columns carries; a routine after the triggers and defaults that call
// it. Every statement carries `if exists`, since a cascade from an earlier one may already have
// taken its object, and `cascade`, since the objects outside public that depend on this one are
// going too - `drop statistics` is the one form PostgreSQL gives no cascade.

var emptyDrops = map[string]emptyDrop{
	"table":                     {0, "drop table", true},
	"foreign table":             {1, "drop foreign table", true},
	"view":                      {2, "drop view", true},
	"materialized view":         {3, "drop materialized view", true},
	"sequence":                  {4, "drop sequence", true},
	"type":                      {5, "drop type", true},
	"function":                  {6, "drop routine", true},
	"procedure":                 {6, "drop routine", true},
	"aggregate":                 {7, "drop aggregate", true},
	"operator":                  {8, "drop operator", true},
	"collation":                 {9, "drop collation", true},
	"conversion":                {10, "drop conversion", true},
	"text search configuration": {11, "drop text search configuration", true},
	"text search dictionary":    {12, "drop text search dictionary", true},
	"text search parser":        {13, "drop text search parser", true},
	"text search template":      {14, "drop text search template", true},
	"statistics object":         {15, "drop statistics", false},
}

// The kinds leftoversQuery answers with that name a database rather than an object of schema
// public. None of them can be what pg_identify_object calls an object in a schema, so one column
// carries both.
const (
	leftSchema       = "schema"
	leftExtension    = "extension"
	leftEventTrigger = "event trigger"
	leftPublication  = "publication"
)

// prepareEmpty reads the unmigrated template - the copy of template1 every database of an empty
// pool is a clone of - and builds the question a reset asks a database about what it is holding
// that the template does not. It ends on the fingerprint, which is what every reset database is
// held to afterwards.
func (p *Pool) prepareEmpty(ctx context.Context, template Querier) error {
	schemas, err := scanNames(ctx, template, templateSchemas)
	if err != nil {
		return fmt.Errorf("read the template's schemas: %w", err)
	}
	// The reset drops every schema but public and empties public itself. A template holding
	// another schema would therefore come back short of it, the fingerprint would refuse every
	// database of the pool, and the run would pay a create database and a drop database per test
	// with nothing to show for it - so it is said here rather than left to be measured.
	if len(schemas) != 1 || schemas[0] != "public" {
		return fmt.Errorf("an empty pool's template holds the schemas %s; it must hold public alone",
			strings.Join(schemas, ", "))
	}
	extensions, err := scanNames(ctx, template, templateExtensions)
	if err != nil {
		return fmt.Errorf("read the template's extensions: %w", err)
	}
	triggers, err := scanNames(ctx, template, templateEventTriggers)
	if err != nil {
		return fmt.Errorf("read the template's event triggers: %w", err)
	}
	publications, err := scanNames(ctx, template, templatePublications)
	if err != nil {
		return fmt.Errorf("read the template's publications: %w", err)
	}
	p.leftovers = leftoversQuery(extensions, triggers, publications)

	if err := template.QueryRow(ctx, p.fingerprintQuery()).Scan(&p.fingerprint); err != nil {
		return fmt.Errorf("read the template's schema fingerprint: %w", err)
	}
	return nil
}

// leftoversQuery is what a reset asks the database it is emptying: every object of schema public
// that no extension owns, and the schemas, extensions, event triggers and publications it holds
// that a fresh database of this server does not. The template's own names are held in the
// statement rather than passed as parameters, as rowProbe holds its tables, so the pool reads no
// name back out of its own answer.
//
// The extension branch leaves the extensions out of publicObjects: which of them may stay is the
// template's business, not the schema's, and an extension of pg_catalog - plpgsql, in every
// database - is in neither branch.
func leftoversQuery(extensions, triggers, publications []string) string {
	return publicObjects + ` and object.type <> '` + leftExtension + `'
		union all
		select '` + leftSchema + `', nspname from pg_namespace
			where nspname not like 'pg\_%' and nspname <> 'information_schema' and nspname <> 'public'
		union all
		select '` + leftExtension + `', extname from pg_extension
			where extname <> all (` + literalArray(extensions) + `)
		union all
		select '` + leftEventTrigger + `', evtname from pg_event_trigger
			where evtname <> all (` + literalArray(triggers) + `)
		union all
		select '` + leftPublication + `', pubname from pg_publication
			where pubname <> all (` + literalArray(publications) + `)
		order by 1, 2`
}

// emptyStatements is the reset of one database of an empty pool, built from what that database is
// holding now. It is one string because the caller runs the lot in one Exec, which under the
// simple protocol is one round trip and one implicit transaction: a database whose emptying fails
// part way through is never put back.
//
// The database's own settings go first. A test that ran `alter database ... set statement_timeout`
// - internal/dispatch/store's ask-reply backfill tests do, to bound a migration that could spin -
// has it applied to every session opened after it, this reset's own among them, and clearing the
// catalog here is what keeps the next reset of that database from inheriting it too. The event
// triggers go next, before any drop below them: an event trigger runs code of its own on every
// DDL statement, so one a test left would otherwise run on the whole of this reset.
//
// Every name below is either the identity pg_identify_object gave it, which is already quoted and
// schema-qualified, or a catalog name this quotes. Nothing depends on the search_path, which a
// test may have set on the database and which this session may still be carrying.
func (p *Pool) emptyStatements(ctx context.Context, conn Querier, name string) (string, error) {
	rows, err := conn.Query(ctx, p.leftovers)
	if err != nil {
		return "", fmt.Errorf("read what the test left behind: %w", err)
	}
	defer rows.Close()
	var schemas, extensions, triggers, publications []string
	// The objects of schema public, by the position emptyDrops gave their kind: several kinds
	// share one statement - a function and a procedure are both dropped as routines - so the
	// drop is held beside the names it will carry.
	objects := map[int][]string{}
	drops := map[int]emptyDrop{}
	for rows.Next() {
		var kind, left string
		if err := rows.Scan(&kind, &left); err != nil {
			return "", fmt.Errorf("read what the test left behind: %w", err)
		}
		switch kind {
		case leftSchema:
			schemas = append(schemas, pgx.Identifier{left}.Sanitize())
		case leftExtension:
			extensions = append(extensions, pgx.Identifier{left}.Sanitize())
		case leftEventTrigger:
			triggers = append(triggers, pgx.Identifier{left}.Sanitize())
		case leftPublication:
			publications = append(publications, pgx.Identifier{left}.Sanitize())
		default:
			// An object of schema public. A kind emptyDrops does not know stays, and the
			// fingerprint read after this reset is what refuses the database for it.
			if drop, known := emptyDrops[kind]; known {
				objects[drop.at] = append(objects[drop.at], left)
				drops[drop.at] = drop
			}
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read what the test left behind: %w", err)
	}

	statements := []string{
		"alter database " + name + " reset all",
		"alter role all in database " + name + " reset all",
	}
	statements = appendDrop(statements, "drop event trigger", false, triggers)
	statements = appendDrop(statements, "drop extension", true, extensions)
	for _, at := range slices.Sorted(maps.Keys(objects)) {
		statements = appendDrop(statements, drops[at].command, drops[at].cascade, objects[at])
	}
	statements = appendDrop(statements, "drop schema", true, schemas)
	statements = appendDrop(statements, "drop publication", false, publications)
	// A large object lives in no schema, so nothing above reaches one.
	statements = append(statements, "select lo_unlink(oid) from pg_largeobject_metadata")
	return strings.Join(statements, ";\n"), nil
}

// appendDrop adds one drop of every name given, or nothing when none were. One statement per kind
// rather than one per object is what lets PostgreSQL resolve what the objects of that kind need
// from each other.
func appendDrop(statements []string, command string, cascade bool, names []string) []string {
	if len(names) == 0 {
		return statements
	}
	statement := command + " if exists " + strings.Join(names, ", ")
	if cascade {
		statement += " cascade"
	}
	return append(statements, statement)
}

// scanNames reads a one-column query of catalog names.
func scanNames(ctx context.Context, from Querier, query string) ([]string, error) {
	rows, err := from.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// literalArray renders names as the text array a leftovers branch holds a catalog against. An
// empty one renders array[]::text[], which every name differs from, where `not in ()` is no SQL
// at all.
func literalArray(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = "'" + strings.ReplaceAll(name, "'", "''") + "'"
	}
	return "array[" + strings.Join(quoted, ", ") + "]::text[]"
}
