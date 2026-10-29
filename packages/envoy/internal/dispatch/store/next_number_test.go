package store

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/migrationid"
)

// A branch that adds a migration numbered one past Dispatch's highest numbered migration, as every
// author did before versions became timestamps, fails the identifier check on that branch, before
// it merges. Two branches open at once would each take that same number, and each set passes on
// its own: the clash would show only once the second merged, which is the conflict timestamps
// exist to remove. The set is read the way TestMigrationSetTakesNumbersThenTimestamps reads it.
func TestMigrationSetRefusesANewMigrationNumberedAfterTheShippedSet(t *testing.T) {
	shipped, err := pgmigrate.Load(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	versions := make(map[int]string, len(shipped)+1)
	highest := 0
	for _, migration := range shipped {
		versions[migration.Version] = migration.Name
		if digits, _, _ := strings.Cut(migration.Name, "_"); len(digits) < 14 && migration.Version > highest {
			highest = migration.Version
		}
	}
	next := fmt.Sprintf("%04d_next_number.up.sql", highest+1)
	versions[highest+1] = next

	if err := migrationid.Check(versions); err == nil {
		t.Fatalf("the identifier check accepted the shipped set plus %s: a branch that takes the next number passes on its own, so two open at once both take it", next)
	} else if !strings.Contains(err.Error(), next) {
		t.Errorf("the identifier check refused the set without naming %s, the file to rename: %v", next, err)
	}
}
