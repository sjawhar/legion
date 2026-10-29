package migrations

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
)

// craftedSet is a set shaped like the embedded one — versions 1 through
// LastNumbered, one file each — with the named extra files added. A test
// crafts the set it wants to see refused rather than adding a file to the
// package the daemon ships.
func craftedSet(extra ...string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for version := 1; version <= LastNumbered; version++ {
		fsys[fmt.Sprintf("%04d_numbered.up.sql", version)] = &fstest.MapFile{Data: []byte("select 1;\n")}
	}
	for _, name := range extra {
		fsys[name] = &fstest.MapFile{Data: []byte("select 1;\n")}
	}
	return fsys
}

func TestTheEmbeddedMigrationsKeepTheIdentifierRule(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	if err := CheckIdentifiers(all); err != nil {
		t.Fatalf("the embedded migrations break the identifier rule: %v", err)
	}
	highest := 0
	for _, migration := range all {
		if migration.Version > highest && migration.Version <= LastNumbered {
			highest = migration.Version
		}
	}
	if highest != LastNumbered {
		t.Errorf("the highest numbered embedded migration is %d, want LastNumbered = %d", highest, LastNumbered)
	}
}

// Two branches open at once can each create a migration; a timestamp to the
// second is what keeps them apart, and the day two authors hit the same second
// the loader refuses the pair by name rather than applying one and dropping the
// other.
func TestLoadRefusesTwoMigrationsSharingATimestamp(t *testing.T) {
	_, err := Load(craftedSet("20261030120000_a.up.sql", "20261030120000_b.up.sql"))
	if err == nil {
		t.Fatal("two migrations sharing a timestamp were loaded, want a refusal")
	}
	for _, name := range []string{"20261030120000_a.up.sql", "20261030120000_b.up.sql"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("refusal does not name %s: %v", name, err)
		}
	}
}

func TestCheckIdentifiersRefusesASetThatBreaksTheRule(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fsys  fstest.MapFS
		names []string
	}{
		{
			name: "a numbered migration gone missing",
			fsys: func() fstest.MapFS {
				fsys := craftedSet()
				delete(fsys, "0007_numbered.up.sql")
				return fsys
			}(),
			names: []string{"0007"},
		},
		{
			name:  "a new migration taking the next number",
			fsys:  craftedSet("0062_x.up.sql"),
			names: []string{"0062_x.up.sql"},
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
			all := parse(t, tc.fsys)
			err := CheckIdentifiers(all)
			if err == nil {
				t.Fatal("the set passed CheckIdentifiers, want a refusal")
			}
			for _, name := range tc.names {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("refusal does not name %s: %v", name, err)
				}
			}
			if _, err := Load(tc.fsys); err == nil {
				t.Error("Load took the set CheckIdentifiers refuses")
			}
		})
	}
}

func TestLoadTakesANumberedSetWithATimestampedMigration(t *testing.T) {
	all, err := Load(craftedSet("20261030120000_x.up.sql"))
	if err != nil {
		t.Fatalf("load a set that keeps the rule: %v", err)
	}
	if len(all) != LastNumbered+1 {
		t.Fatalf("loaded %d migrations, want %d", len(all), LastNumbered+1)
	}
	last := all[len(all)-1]
	if last.Version != 20261030120000 || last.Name != "20261030120000_x.up.sql" {
		t.Errorf("the last migration is %s at version %d, want the timestamped one last", last.Name, last.Version)
	}
	if err := CheckIdentifiers(all); err != nil {
		t.Errorf("a set that keeps the rule was refused: %v", err)
	}
}

// parse reads a crafted set the way Load does, without the rule Load applies,
// so a test can hand CheckIdentifiers the very set whose refusal it is about.
func parse(t *testing.T, fsys fstest.MapFS) []Migration {
	t.Helper()
	all := make([]Migration, 0, len(fsys))
	for name := range fsys {
		version, err := versionOf(name)
		if err != nil {
			t.Fatalf("version of %s: %v", name, err)
		}
		all = append(all, Migration{Version: version, Name: name})
	}
	return all
}
