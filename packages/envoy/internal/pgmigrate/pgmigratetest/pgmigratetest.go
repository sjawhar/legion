// Package pgmigratetest holds the rules Envoy's migration sets are held to in tests: what a store
// embeds is its whole migrations directory, the set is numbered 1 to N, every migration from a
// store's chosen number declares a census, a census names the new migrations whose objects it
// reads (CheckCensusNamesTheMigrationsItReads), and the census's reading of which tables a
// migration touches is held to the set, both by name (CheckTouchedTablesAreKnown) and against the
// locks each migration really takes (CheckTouchedTablesAgainstLocks). It is an ordinary package
// rather than a _test.go file so Dispatch's and the broker's store tests can both call it; Go
// shares no test-only code across packages.
package pgmigratetest

import (
	"context"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// CheckEmbedsEveryFile reports an error unless embedded holds, in dir, every entry the directory
// dir holds on disk, read from the test's working directory, the store's package directory.
// pgmigrate.Load refuses a file named any other way than a migration, but only a file it is
// given: a //go:embed pattern naming a directory without all: leaves out every name beginning
// with _ or ., so a migration named _0054_x.up.sql would go unembedded and unapplied, with no
// error, while its file sits in the tree.
func CheckEmbedsEveryFile(embedded fs.FS, dir string) error {
	onDisk, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("list %s on disk: %w", dir, err)
	}
	inBinary, err := fs.ReadDir(embedded, dir)
	if err != nil {
		return fmt.Errorf("list embedded %s: %w", dir, err)
	}
	names := func(entries []fs.DirEntry) []string {
		out := make([]string, len(entries))
		for i, entry := range entries {
			out[i] = entry.Name()
		}
		return out
	}
	for _, name := range names(onDisk) {
		if !slices.Contains(names(inBinary), name) {
			return fmt.Errorf("%s/%s is on disk but not embedded, so no runner would see it; embed the directory with all:", dir, name)
		}
	}
	return nil
}

// CheckNumberedOneToN reports an error unless the set pgmigrate.Load reads from dir in fsys is
// numbered 1 to N with no number missing. It reads file names alone, so it needs no database.
//
// pgmigrate.Load does not require this, since a runner does not depend on it: a set with a gap
// applies as written. The repository requires it because a gap is how one migration comes to run
// in two orders. A branch that takes a number ahead of one not yet on main (0054 while another pull
// request holds 0053) passes every other check; if it merges first, production applies 0054, and
// the 0053 merged after it runs after 0054 there while every fresh database runs it before. Holding
// every set to 1 to N keeps such a branch red until the lower number lands, which is the price of
// every database applying one order.
func CheckNumberedOneToN(fsys fs.FS, dir string) error {
	migrations, err := pgmigrate.Load(fsys, dir)
	if err != nil {
		return err
	}
	for i, migration := range migrations {
		if migration.Version != i+1 {
			return fmt.Errorf(
				"migration %s is numbered %d where %d comes next: every version from 1 to the highest is taken once, or a lower number could merge after a higher one and run after it in production, before it everywhere else; take the next free number on main",
				migration.Name, migration.Version, i+1)
		}
	}
	return nil
}

// CheckCensusDeclaredFrom reports an error unless every migration numbered from or above `from`
// declares a census file (<version>_<name>.census.sql). A deployment reads the census before it
// applies the migration (pgmigrate.Census); a migration that cannot refuse or rewrite any row
// declares `select 0` with a comment saying why, so the author's judgment is on record.
func CheckCensusDeclaredFrom(fsys fs.FS, dir string, from int) error {
	migrations, err := pgmigrate.Load(fsys, dir)
	if err != nil {
		return err
	}
	for _, migration := range migrations {
		if migration.Version >= from && migration.Census == "" {
			return fmt.Errorf("migration %s declares no census: add %s.census.sql beside it, one select answering how many existing rows the migration would refuse or rewrite (select 0, with a comment, when none can be)",
				migration.Name, strings.TrimSuffix(migration.Name, ".up.sql"))
		}
	}
	return nil
}

var (
	createTable = regexp.MustCompile(`(?is)\bcreate\s+table\s+(?:if\s+not\s+exists\s+)?(?:public\.)?([a-z_][a-z0-9_]*)`)
	renameTable = regexp.MustCompile(`(?is)\balter\s+table\s+(?:if\s+exists\s+)?(?:only\s+)?(?:public\.)?([a-z_][a-z0-9_]*)\s+rename\s+to\s+([a-z_][a-z0-9_]*)`)
)

// CheckTouchedTablesAreKnown reports an error unless every table pgmigrate.TouchedTables finds in
// a migration is one the set creates in that migration or an earlier one. It reads file text
// alone and needs no database, so a store's plain unit tests, run without a test database, still
// catch a name the extractor misread. It is one-sided: it never catches a statement form the
// extractor misses, which names nothing; CheckTouchedTablesAgainstLocks catches that, and a misread
// name too, where a database is at hand.
func CheckTouchedTablesAreKnown(fsys fs.FS, dir string) error {
	migrations, err := pgmigrate.Load(fsys, dir)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, migration := range migrations {
		for _, match := range createTable.FindAllStringSubmatch(migration.SQL, -1) {
			known[strings.ToLower(match[1])] = true
		}
		for _, match := range renameTable.FindAllStringSubmatch(migration.SQL, -1) {
			known[strings.ToLower(match[2])] = true
		}
		for _, table := range pgmigrate.TouchedTables(migration.SQL) {
			if !known[table] {
				return fmt.Errorf("migration %s touches %q, which no migration up to it creates: either the migration names a table that does not exist or pgmigrate.TouchedTables misread a statement; fix the extractor's patterns in touched.go", migration.Name, table)
			}
		}
	}
	return nil
}

// CheckTouchedTablesAgainstLocks applies every migration of the set in fsys, in version order and
// each in a transaction of its own as the runners do, on the database conn is connected to, and
// reports an error unless what the census reads from each migration, pgmigrate.CensusTables read
// just before the migration applies, is what it locks above ACCESS SHARE. What the migration locks
// is read from pg_locks for its own backend before it commits, an index lock counted as its
// table's. Only tables that existed before the migration count, since a table it creates holds no
// row and nobody else's lock. Two ways to fail:
//
//   - a table the migration locks above ACCESS SHARE (every write and every DDL) that the reading
//     misses: a statement form the census's patterns do not know, whose table the census would
//     neither count nor check for holders;
//   - a table a statement of the migration is read to name that the migration never locks: a
//     misread statement, which would refuse a deploy over a table the migration leaves alone. A
//     table a foreign key reaches is not held to this, since it is locked only when the migration
//     writes a row there or in the table it is reached from; nor is one only a DO block's body
//     names (TouchedTable.Conditional), since the block can branch on rows the audit's database
//     does not hold.
//
// The locks a foreign key takes are taken on rows: a row the migration writes is checked against,
// or cascades into, the tables its keys connect. The audit sees them only where the set's earlier
// migrations leave rows, as the tests' own sets do; a store's real set is applied to an empty
// database, where the audit holds its statements' own locks to the reading, and the tables a
// foreign key reaches are the catalog's (CensusTables).
//
// A table the migration only reads (ACCESS SHARE: insert … select from, create view … as) is left
// out of the reading on purpose: its lock waits only behind an ACCESS EXCLUSIVE holder, and
// counting it would refuse a deploy over every long reader, the nightly dump among them.
func CheckTouchedTablesAgainstLocks(ctx context.Context, conn *pgx.Conn, fsys fs.FS, dir string) error {
	migrations, err := pgmigrate.Load(fsys, dir)
	if err != nil {
		return err
	}
	for _, migration := range migrations {
		if err := auditMigrationLocks(ctx, conn, migration); err != nil {
			return err
		}
	}
	return nil
}

func auditMigrationLocks(ctx context.Context, conn *pgx.Conn, migration pgmigrate.Migration) error {
	before, err := schemaTables(ctx, conn)
	if err != nil {
		return fmt.Errorf("migration %s: list tables before it: %w", migration.Name, err)
	}
	existed := map[string]bool{}
	for _, name := range before {
		existed[name] = true
	}
	reading, err := pgmigrate.CensusTables(ctx, conn, migration.SQL)
	if err != nil {
		return fmt.Errorf("migration %s: %w", migration.Name, err)
	}
	read, named := map[string]bool{}, map[string]bool{}
	for _, table := range reading {
		read[table.Name] = true
		if table.Through == "" && !table.Conditional {
			named[table.Name] = true
		}
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migration %s: begin: %w", migration.Name, err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, migration.SQL); err != nil {
		return fmt.Errorf("migration %s: apply: %w", migration.Name, err)
	}
	rows, err := tx.Query(ctx, `
		select coalesce(i.indrelid, l.relation), l.mode
		from pg_locks l left join pg_index i on i.indexrelid = l.relation
		where l.pid = pg_backend_pid() and l.locktype = 'relation' and l.granted
			and l.database = (select oid from pg_database where datname = current_database())`)
	if err != nil {
		return fmt.Errorf("migration %s: read its locks: %w", migration.Name, err)
	}
	defer rows.Close()
	locked := map[string]bool{}
	lockedAboveRead := map[string]bool{}
	for rows.Next() {
		var relation uint32
		var mode string
		if err := rows.Scan(&relation, &mode); err != nil {
			return fmt.Errorf("migration %s: read its locks: %w", migration.Name, err)
		}
		name, existed := before[relation]
		if !existed {
			continue
		}
		locked[name] = true
		if mode != "AccessShareLock" {
			lockedAboveRead[name] = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("migration %s: read its locks: %w", migration.Name, err)
	}
	for _, name := range slices.Sorted(maps.Keys(lockedAboveRead)) {
		if !read[name] {
			return fmt.Errorf("migration %s locks %s above ACCESS SHARE, which pgmigrate.CensusTables does not read from it: the census would neither count that table nor check who holds it; add the statement's form to the patterns in touched.go and a row to TestTouchedTablesFindsEveryStatementFormTheMigrationsUse", migration.Name, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(named)) {
		if existed[name] && !locked[name] {
			return fmt.Errorf("migration %s names %s, which pgmigrate.CensusTables reads from it, but takes no lock on it: the extractor misread a statement, and the census would refuse a deploy over a table the migration leaves alone; fix the patterns in touched.go", migration.Name, name)
		}
	}
	return tx.Commit(ctx)
}

// schemaTables maps each ordinary or partitioned table of the schema the connection creates
// tables in (current_schema(): public for Dispatch's tests, a schema of its own for the broker's)
// to its name.
func schemaTables(ctx context.Context, conn *pgx.Conn) (map[uint32]string, error) {
	rows, err := conn.Query(ctx, "select oid, relname from pg_class where relnamespace = current_schema()::regnamespace and relkind in ('r', 'p')")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := map[uint32]string{}
	for rows.Next() {
		var oid uint32
		var name string
		if err := rows.Scan(&oid, &name); err != nil {
			return nil, err
		}
		tables[oid] = name
	}
	return tables, rows.Err()
}

// CheckCensusNamesTheMigrationsItReads applies every migration of the set in fsys, in version
// order and each in a transaction of its own, on the empty database conn is connected to, recording
// which migration created each table, column, function and type, and which last renamed one or
// gave a column a new type, and reports an error unless every census names, in its text, each
// migration numbered from `from` on that created or changed what it reads. What a census reads is
// what Postgres records it depends on as a view, created just before its migration applies and
// rolled back.
//
// A deployment takes every census before any migration of its release applies, so a census that
// reads what an earlier migration of the same release creates, renames or retypes names something
// the database does not have yet, and refuses every deploy of that release. Where releases split
// is not in the set, so the test cannot tell; naming the migration in the census, in a comment
// saying it ships in an earlier release, is the author's word that it does. A table or column an
// earlier migration of the same release creates holds no row the census could count, so such a
// census does not read it.
func CheckCensusNamesTheMigrationsItReads(ctx context.Context, conn *pgx.Conn, fsys fs.FS, dir string, from int) error {
	migrations, err := pgmigrate.Load(fsys, dir)
	if err != nil {
		return err
	}
	created := map[catalogObject]createdBy{}
	for _, migration := range migrations {
		if migration.Census != "" {
			reads, err := censusReads(ctx, conn, migration.Census)
			if err != nil {
				return fmt.Errorf("census %s: %w", migration.CensusName, err)
			}
			for _, object := range reads {
				creator, ok := created[object]
				if !ok || creator.migration.Version < from || strings.Contains(migration.Census, strings.TrimSuffix(creator.migration.Name, ".up.sql")) {
					continue
				}
				advice := "write the census without it"
				if creator.verb == "creates" {
					advice += " (a table or column the same release creates holds no row to count)"
				}
				return fmt.Errorf("census %s reads %s, which %s %s: a deployment takes the census before any migration of its release applies, so a release carrying both refuses every deploy; ship %s in an earlier release and name it in a comment in the census, or %s",
					migration.CensusName, creator.name, creator.migration.Name, creator.verb, creator.migration.Name, advice)
			}
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("migration %s: begin: %w", migration.Name, err)
		}
		if _, err := tx.Exec(ctx, migration.SQL); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("migration %s: apply: %w", migration.Name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("migration %s: commit: %w", migration.Name, err)
		}
		if err := recordChanges(ctx, conn, migration, created); err != nil {
			return fmt.Errorf("migration %s: %w", migration.Name, err)
		}
	}
	return nil
}

// catalogObject is a catalog row a census can depend on, as pg_depend names it: the catalog
// (pg_class, pg_proc, pg_type), the object's oid, and a column's number, 0 for the object itself.
type catalogObject struct {
	class, oid uint32
	sub        int32
}

// createdBy is the migration that last made a catalog object what a census reads: what it did to
// it (verb), and the name and, for a column, the type it left.
type createdBy struct {
	migration pgmigrate.Migration
	verb      string
	name      string
	typ       uint32
}

// recordChanges records migration as the creator of every table, column, function and type of the
// schema the connection creates in that no earlier migration created, and as the last changer of
// one an earlier migration made whose name it changes, or a column's type: a rename or a new type
// keeps the catalog row's oid, but a census that reads the object under its new name or type reads
// what the database does not have before migration applies.
func recordChanges(ctx context.Context, conn *pgx.Conn, migration pgmigrate.Migration, created map[catalogObject]createdBy) error {
	rows, err := conn.Query(ctx, `
		select 'pg_class'::regclass::oid, c.oid, 0, c.relname::text, 0::oid from pg_class c where c.relnamespace = current_schema()::regnamespace
		union all
		select 'pg_class'::regclass::oid, c.oid, a.attnum::int, c.relname || '.' || a.attname, a.atttypid
		from pg_attribute a join pg_class c on c.oid = a.attrelid
		where c.relnamespace = current_schema()::regnamespace and a.attnum > 0 and not a.attisdropped
		union all
		select 'pg_proc'::regclass::oid, p.oid, 0, p.proname::text, 0::oid from pg_proc p where p.pronamespace = current_schema()::regnamespace
		union all
		select 'pg_type'::regclass::oid, t.oid, 0, t.typname::text, 0::oid from pg_type t where t.typnamespace = current_schema()::regnamespace`)
	if err != nil {
		return fmt.Errorf("list what it created: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var object catalogObject
		var name string
		var typ uint32
		if err := rows.Scan(&object.class, &object.oid, &object.sub, &name, &typ); err != nil {
			return fmt.Errorf("list what it created: %w", err)
		}
		prior, ok := created[object]
		switch {
		case !ok:
			created[object] = createdBy{migration: migration, verb: "creates", name: name, typ: typ}
		case prior.name != name:
			created[object] = createdBy{migration: migration, verb: "renames from " + prior.name, name: name, typ: typ}
		case prior.typ != typ:
			created[object] = createdBy{migration: migration, verb: "gives a new type", name: name, typ: typ}
		}
	}
	return rows.Err()
}

// censusReads is every catalog object census depends on, read from pg_depend for a temporary
// view of it, which a rolled-back transaction creates.
func censusReads(ctx context.Context, conn *pgx.Conn, census string) ([]catalogObject, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "create temporary view pgmigratetest_census as "+strings.TrimSuffix(strings.TrimSpace(census), ";")); err != nil {
		return nil, fmt.Errorf("read it as a view: %w", err)
	}
	// Ordered so that a table comes before its columns, and one failure names the same object on
	// every run.
	rows, err := tx.Query(ctx, `
		select d.refclassid, d.refobjid, d.refobjsubid
		from pg_depend d join pg_rewrite r on r.oid = d.objid
		where d.classid = 'pg_rewrite'::regclass and r.ev_class = 'pgmigratetest_census'::regclass
			and d.refobjid <> 'pgmigratetest_census'::regclass
		order by 1, 2, 3`)
	if err != nil {
		return nil, fmt.Errorf("read what it depends on: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (catalogObject, error) {
		var object catalogObject
		return object, row.Scan(&object.class, &object.oid, &object.sub)
	})
}
