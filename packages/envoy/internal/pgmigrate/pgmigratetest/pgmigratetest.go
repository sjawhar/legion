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
