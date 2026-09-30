// Package pgmigratetest holds the rules Envoy's migration sets are held to in tests: what a store
// embeds is its whole migrations directory, and the set is numbered 1 to N. It is an ordinary
// package rather than a _test.go file so Dispatch's and the broker's store tests can both call
// it; Go shares no test-only code across packages.
package pgmigratetest

import (
	"fmt"
	"io/fs"
	"os"
	"slices"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// CheckEmbedsEveryFile reports an error unless embedded holds, in dir, every entry the directory
// dir holds on disk, read from the test's working directory, the store's package directory.
//
// This is where the stores' embed rule is stated. pgmigrate.Load refuses a file named any other way
// than a migration, but only a file it is given, and a //go:embed pattern can leave an entry out
// without an error: a directory pattern without all: drops every name beginning with _ or ., so
// both stores embed all:migrations; no directive embeds a symlink or an empty directory. A migration
// file left out would go unapplied while it sits in the tree, and an empty directory, which holds
// nothing to apply, is still an entry Load never judges. On today's tree the all: prefix changes
// nothing, since neither directory holds a _ or . name; this check fails when an entry on disk is
// missing from the binary: such a name under a directive without all:, a symlink, or an empty
// directory.
func CheckEmbedsEveryFile(embedded fs.FS, dir string) error {
	onDisk, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("list %s on disk: %w", dir, err)
	}
	inBinary, err := fs.ReadDir(embedded, dir)
	if err != nil {
		return fmt.Errorf("list embedded %s: %w", dir, err)
	}
	embeddedNames := make([]string, len(inBinary))
	for i, entry := range inBinary {
		embeddedNames[i] = entry.Name()
	}
	for _, entry := range onDisk {
		name := entry.Name()
		if slices.Contains(embeddedNames, name) {
			continue
		}
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s/%s is a symlink, which no //go:embed directive embeds, so no runner would see it; replace it with the file it points to, or remove it (an editor's lock file, such as Emacs's .#<file>, is one)", dir, name)
		case entry.IsDir():
			return fmt.Errorf("%s/%s is a directory holding nothing //go:embed can carry, such as an empty one, and a migrations directory holds files alone; remove it", dir, name)
		}
		return fmt.Errorf("%s/%s is on disk but not embedded, so no runner would see it; embed the directory with all:", dir, name)
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
