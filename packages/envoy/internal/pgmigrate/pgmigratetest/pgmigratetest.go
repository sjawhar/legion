// Package pgmigratetest holds the rules Envoy's migration sets are held to in tests: what a store
// embeds is its whole migrations directory, the set is numbered 1 to N, every migration from a
// store's chosen number declares a census, and the census's reading of which tables a migration
// touches names only tables the set creates. It is an ordinary package rather than a _test.go file
// so Dispatch's and the broker's store tests can both call it; Go shares no test-only code across
// packages.
package pgmigratetest

import (
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"

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
// a migration is one the set creates in that migration or an earlier one. It holds the textual
// extractor to the real set: a statement form it misreads shows up as a name no migration creates.
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
