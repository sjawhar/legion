// Package pgmigratetest holds the rules Envoy's migration sets are held to in tests: what a store
// embeds is its whole migrations directory, the set is numbered 1 to N, every migration from a
// store's chosen number declares a census, and the census's reading of which tables a migration
// touches is held to the set, both by name (CheckTouchedTablesAreKnown) and against the locks each
// migration really takes (CheckTouchedTablesAgainstLocks). It is an ordinary package rather than a
// _test.go file so Dispatch's and the broker's store tests can both call it; Go shares no
// test-only code across packages.
package pgmigratetest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
// alone and needs no database, and it is one-sided: it catches a misread name, never a statement
// form the extractor misses, which names nothing. CheckTouchedTablesAgainstLocks catches that.
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
				return fmt.Errorf("migration %s touches %q, which no migration up to it creates: either the migration names a table that does not exist or pgmigrate.TouchedTables misread a statement; fix the extractor's patterns in census.go", migration.Name, table)
			}
		}
	}
	return nil
}

// CheckTouchedTablesAgainstLocks applies every migration of the set in fsys, in version order and
// each in a transaction of its own as the runners do, to the empty database conn is connected to,
// and reports an error unless what the census reads from each migration is exactly what it locks.
// The census's reading is pgmigrate.TouchedTables plus the tables of the indexes
// pgmigrate.TouchedIndexes names; what the migration locks is read from pg_locks for its own
// backend before it commits, an index lock counted as its table's. Only tables that existed before
// the migration count, since a table it creates holds no row and nobody else's lock. Two ways to
// fail:
//
//   - a table the migration locks above ACCESS SHARE (every write and every DDL) that the reading
//     misses: a statement form the census's patterns do not know, whose table the census would
//     neither count nor check for holders;
//   - a table the reading names that the migration never locks: a misread statement, which would
//     refuse a deploy over a table the migration leaves alone.
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
	read := map[string]bool{}
	for _, table := range pgmigrate.TouchedTables(migration.SQL) {
		read[table] = true
	}
	for _, index := range pgmigrate.TouchedIndexes(migration.SQL) {
		var table uint32
		err := conn.QueryRow(ctx, "select indrelid from pg_index where indexrelid = to_regclass($1)", index).Scan(&table)
		switch {
		case err == nil:
			if name, ok := before[table]; ok {
				read[name] = true
			}
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("migration %s: resolve index %s: %w", migration.Name, index, err)
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
	for _, name := range sortedKeys(lockedAboveRead) {
		if !read[name] {
			return fmt.Errorf("migration %s locks %s above ACCESS SHARE, which pgmigrate.TouchedTables does not read from it: the census would neither count that table nor check who holds it; add the statement's form to the patterns in census.go and a row to TestTouchedTablesFindsEveryStatementFormTheMigrationsUse", migration.Name, name)
		}
	}
	for _, name := range sortedKeys(read) {
		if existed[name] && !locked[name] {
			return fmt.Errorf("migration %s names %s, which pgmigrate.TouchedTables reads from it, but takes no lock on it: the extractor misread a statement, and the census would refuse a deploy over a table the migration leaves alone; fix the patterns in census.go", migration.Name, name)
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

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
