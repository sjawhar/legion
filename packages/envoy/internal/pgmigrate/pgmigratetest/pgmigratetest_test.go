package pgmigratetest

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestCheckNumberedOneToNRefusesAGap(t *testing.T) {
	set := fstest.MapFS{
		"migrations/0001_a.up.sql": {Data: []byte("select 1")},
		"migrations/0002_b.up.sql": {Data: []byte("select 2")},
		"migrations/0004_d.up.sql": {Data: []byte("select 4")},
	}
	err := CheckNumberedOneToN(set, "migrations")
	if err == nil || !strings.Contains(err.Error(), "migration 0004_d.up.sql is numbered 4 where 3 comes next") {
		t.Fatalf("CheckNumberedOneToN = %v, want it to name 0004_d.up.sql and the missing 3", err)
	}
}

func TestCheckNumberedOneToNRefusesASetNotStartingAtOne(t *testing.T) {
	set := fstest.MapFS{"migrations/0002_b.up.sql": {Data: []byte("select 2")}}
	err := CheckNumberedOneToN(set, "migrations")
	if err == nil || !strings.Contains(err.Error(), "numbered 2 where 1 comes next") {
		t.Fatalf("CheckNumberedOneToN = %v, want it to name the missing 1", err)
	}
}

func TestCheckNumberedOneToNAcceptsOneToN(t *testing.T) {
	set := fstest.MapFS{
		"migrations/0001_a.up.sql":   {Data: []byte("select 1")},
		"migrations/0001_a.down.sql": {Data: []byte("select -1")},
		"migrations/2_b.up.sql":      {Data: []byte("select 2")},
	}
	if err := CheckNumberedOneToN(set, "migrations"); err != nil {
		t.Fatalf("CheckNumberedOneToN = %v, want nil", err)
	}
}

// The rule starts at its number: a migration below it needs no census, and the first one at it
// without one is named with the file to add.
func TestCheckCensusDeclaredFromNamesTheFirstMigrationFromItsNumberWithoutOne(t *testing.T) {
	set := fstest.MapFS{
		"migrations/0001_a.up.sql": {Data: []byte("create table a (id integer)")},
		"migrations/0002_b.up.sql": {Data: []byte("alter table a add column b integer")},
	}
	if err := CheckCensusDeclaredFrom(set, "migrations", 3); err != nil {
		t.Errorf("CheckCensusDeclaredFrom from 3 = %v, want nil: no migration is numbered 3 or more", err)
	}
	err := CheckCensusDeclaredFrom(set, "migrations", 2)
	if err == nil || !strings.Contains(err.Error(), "migration 0002_b.up.sql declares no census: add 0002_b.census.sql") {
		t.Fatalf("CheckCensusDeclaredFrom from 2 = %v, want it to name 0002_b.up.sql and the file to add", err)
	}
	set["migrations/0002_b.census.sql"] = &fstest.MapFile{Data: []byte("-- a new nullable column: no row can violate it\nselect 0")}
	if err := CheckCensusDeclaredFrom(set, "migrations", 2); err != nil {
		t.Errorf("CheckCensusDeclaredFrom with the census = %v, want nil", err)
	}
}

// A table the census's extractor finds that no migration up to it creates is named: either the
// migration names a table that does not exist or the extractor misread a statement. A rename
// makes the new name known.
func TestCheckTouchedTablesAreKnownNamesATableNoMigrationCreates(t *testing.T) {
	set := fstest.MapFS{
		"migrations/0001_a.up.sql": {Data: []byte("create table if not exists public.a (id integer)")},
		"migrations/0002_b.up.sql": {Data: []byte("alter table a rename to b")},
		"migrations/0003_c.up.sql": {Data: []byte("alter table b add column c integer")},
	}
	if err := CheckTouchedTablesAreKnown(set, "migrations"); err != nil {
		t.Fatalf("CheckTouchedTablesAreKnown = %v, want nil", err)
	}
	set["migrations/0004_d.up.sql"] = &fstest.MapFile{Data: []byte("update ghost set c = 1")}
	err := CheckTouchedTablesAreKnown(set, "migrations")
	if err == nil || !strings.Contains(err.Error(), `migration 0004_d.up.sql touches "ghost", which no migration up to it creates`) {
		t.Fatalf("CheckTouchedTablesAreKnown = %v, want it to name 0004_d.up.sql and ghost", err)
	}
}
