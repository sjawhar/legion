package migrations

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/envoy/migrationid"
)

// craftedNumbered is how many numbered migrations a crafted set holds, and the number such a set's
// series closed at: enough for a gap to show, and nothing about the set the daemon ships, whose
// own closing file migrationid records. A crafted set is read by load with that number
// (crafted), rather than by Load, which finds the daemon's series by its closing file.
const craftedNumbered = 3

// crafted checks a crafted set against its own closing number.
func crafted(versions map[int]string) error {
	return migrationid.CheckClosedAt(versions, craftedNumbered)
}

// craftedSet is a set shaped like the embedded one — versions 1 through
// craftedNumbered, one file each — with the named extra files added. A test
// crafts the set it wants to see refused rather than adding a file to the
// package the daemon ships.
func craftedSet(extra ...string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for version := 1; version <= craftedNumbered; version++ {
		fsys[fmt.Sprintf("%04d_numbered.up.sql", version)] = &fstest.MapFile{Data: []byte("select 1;\n")}
	}
	for _, name := range extra {
		fsys[name] = &fstest.MapFile{Data: []byte("select 1;\n")}
	}
	return fsys
}

// The set the daemon ships keeps the identifier rule: Load holds every set to it, so reading the
// embedded one is the test.
func TestTheEmbeddedMigrationsKeepTheIdentifierRule(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatalf("the embedded migrations were refused: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no migration is embedded")
	}
}

// Two branches open at once can each create a migration; a timestamp to the
// second is what keeps them apart, and the day two authors hit the same second
// the loader refuses the pair by name rather than applying one and dropping the
// other.
func TestLoadRefusesTwoMigrationsSharingATimestamp(t *testing.T) {
	_, err := load(craftedSet("20261030120000_a.up.sql", "20261030120000_b.up.sql"), crafted)
	if err == nil {
		t.Fatal("two migrations sharing a timestamp were loaded, want a refusal")
	}
	for _, name := range []string{"20261030120000_a.up.sql", "20261030120000_b.up.sql"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("refusal does not name %s: %v", name, err)
		}
	}
}

func TestLoadRefusesASetThatBreaksTheIdentifierRule(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fsys  fstest.MapFS
		names []string
	}{
		{
			name: "a numbered migration gone missing",
			fsys: func() fstest.MapFS {
				fsys := craftedSet()
				delete(fsys, "0002_numbered.up.sql")
				return fsys
			}(),
			names: []string{"0002"},
		},
		{
			name:  "a timestamp of thirteen digits",
			fsys:  craftedSet("2026103012000_x.up.sql"),
			names: []string{"2026103012000_x.up.sql"},
		},
		{
			name:  "a timestamp of a month that does not exist",
			fsys:  craftedSet("20261340120000_x.up.sql"),
			names: []string{"20261340120000_x.up.sql"},
		},
		{
			name:  "a timestamp of a day that does not exist",
			fsys:  craftedSet("20260231120000_x.up.sql"),
			names: []string{"20260231120000_x.up.sql"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(tc.fsys, crafted)
			if err == nil {
				t.Fatal("Load took a set that breaks the identifier rule, want a refusal")
			}
			for _, name := range tc.names {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("refusal does not name %s: %v", name, err)
				}
			}
		})
	}
}

func TestLoadTakesANumberedSetWithATimestampedMigration(t *testing.T) {
	all, err := load(craftedSet("20261030120000_x.up.sql"), crafted)
	if err != nil {
		t.Fatalf("load a set that keeps the rule: %v", err)
	}
	if len(all) != craftedNumbered+1 {
		t.Fatalf("loaded %d migrations, want %d", len(all), craftedNumbered+1)
	}
	last := all[len(all)-1]
	if last.Version != 20261030120000 || last.Name != "20261030120000_x.up.sql" {
		t.Errorf("the last migration is %s at version %d, want the timestamped one last", last.Name, last.Version)
	}
}
