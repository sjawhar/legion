// Package pgmigratetest holds the numbering rule Envoy's migration sets are held to in tests. It is
// an ordinary package rather than a _test.go file so Dispatch's and the broker's store tests can
// both call it; Go shares no test-only code across packages.
package pgmigratetest

import (
	"fmt"
	"io/fs"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

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
