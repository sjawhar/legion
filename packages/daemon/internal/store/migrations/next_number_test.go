package migrations

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
)

// A branch that adds a migration numbered one past the daemon's highest numbered migration, as
// every author did before versions became timestamps, is refused on that branch, before it merges.
// Two branches open at once would each take that same number, and each set passes on its own: the
// clash would show only once the second merged, which is the conflict timestamps exist to remove.
// A set that grew one number has to read as a new numbered migration, not as a longer series.
func TestLoadRefusesANewMigrationNumberedAfterTheShippedSet(t *testing.T) {
	shipped, err := All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	fsys := fstest.MapFS{}
	highest := 0
	for _, migration := range shipped {
		fsys[migration.Name] = &fstest.MapFile{Data: []byte(migration.SQL)}
		if digits, _, _ := strings.Cut(migration.Name, "_"); len(digits) < 14 && migration.Version > highest {
			highest = migration.Version
		}
	}
	next := fmt.Sprintf("%04d_next_number.up.sql", highest+1)
	fsys[next] = &fstest.MapFile{Data: []byte("select 1;")}

	if _, err := Load(fsys); err == nil {
		t.Fatalf("Load accepted the shipped set plus %s: a branch that takes the next number passes on its own, so two open at once both take it", next)
	} else if !strings.Contains(err.Error(), next) {
		t.Errorf("Load refused the set without naming %s, the file to rename: %v", next, err)
	}
}
