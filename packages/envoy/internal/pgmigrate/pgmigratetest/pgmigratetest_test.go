package pgmigratetest

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// Each entry the binary leaves out gets the advice that fixes it: all: brings in a _ or . name, and
// nothing embeds a symlink, a version-control name, a directory //go:embed took nothing from (an
// empty one, for instance) or an irregular file, so those are named for what they are rather than
// sent back to all:. Each row is the entry that tells one branch of the advice from its absence.
func TestCheckEmbedsEveryFileNamesWhyAnEntryIsMissing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		place func(t *testing.T, dir string)
		want  string
	}{
		{
			name: "an editor's dangling lock symlink",
			place: func(t *testing.T, dir string) {
				if err := os.Symlink("user@host.1234:1700000000", filepath.Join(dir, ".#0001_a.up.sql")); err != nil {
					t.Fatal(err)
				}
			},
			want: "migrations/.#0001_a.up.sql is a symlink, which no //go:embed directive embeds",
		},
		{
			name: "an empty directory",
			place: func(t *testing.T, dir string) {
				if err := os.Mkdir(filepath.Join(dir, ".empty"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "migrations/.empty is a directory //go:embed took nothing from (an empty one, for instance), and a migrations directory holds files alone; remove it",
		},
		{
			name: "a directory holding a file, which a directive without all: drops whole",
			place: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "_old"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "_old", "0002_b.up.sql"), []byte("select 2"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "migrations/_old is a directory //go:embed took nothing from (an empty one, for instance), and a migrations directory holds files alone; remove it",
		},
		{
			name: "a name a directive without all: drops",
			place: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "_0002_b.up.sql"), []byte("select 2"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "migrations/_0002_b.up.sql is on disk but not embedded, so no runner would see it; embed the directory with all:",
		},
		{
			name: "an editor's swap file, a . name",
			place: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, ".0001_a.up.sql.swp"), []byte("swap"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "migrations/.0001_a.up.sql.swp is on disk but not embedded, so no runner would see it; embed the directory with all:, or rename it if all: is already there, since Go's module paths refuse some names (one holding a colon, for instance)",
		},
		{
			name: "a regular file the store's pattern leaves out",
			place: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "0002_b.up.sql"), []byte("select 2"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "migrations/0002_b.up.sql is on disk but not embedded, so no runner would see it; check the store's //go:embed pattern",
		},
		{
			name: "a version-control directory, which all: does not bring in",
			place: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, ".git", "objects"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "migrations/.git is a version-control name, which no //go:embed directive embeds, all: included; remove it",
		},
		{
			name: "a FIFO, which is not a regular file",
			place: func(t *testing.T, dir string) {
				if err := syscall.Mkfifo(filepath.Join(dir, "_0002_b.up.sql"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "migrations/_0002_b.up.sql is not a regular file (a FIFO, socket or device), which no //go:embed directive embeds; remove it",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if err := os.Mkdir("migrations", 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join("migrations", "0001_a.up.sql"), []byte("select 1"), 0o644); err != nil {
				t.Fatal(err)
			}
			tc.place(t, "migrations")
			embedded := fstest.MapFS{"migrations/0001_a.up.sql": {Data: []byte("select 1")}}
			err := CheckEmbedsEveryFile(embedded, "migrations")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CheckEmbedsEveryFile = %v, want it to say %q", err, tc.want)
			}
		})
	}
}

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

// recordingTB reads back what a check reports: CheckIdentifiers names each file that breaks the
// rule through the test it is given, so the test that holds it to its own rule records those
// reports instead of failing on them. Every other method is the real test's.
type recordingTB struct {
	testing.TB
	reported []string
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.reported = append(r.reported, fmt.Sprintf(format, args...))
}

// identifiers is a set of migrations named as a store's are, each file's version its leading
// digits, which is what pgmigrate.Load reads.
func identifiers(t *testing.T, names ...string) []pgmigrate.Migration {
	t.Helper()
	set := make([]pgmigrate.Migration, 0, len(names))
	for _, name := range names {
		digits, _, _ := strings.Cut(strings.TrimSuffix(name, ".up.sql"), "_")
		version, err := strconv.Atoi(digits)
		if err != nil {
			t.Fatalf("test set names %s, whose version is not digits: %v", name, err)
		}
		set = append(set, pgmigrate.Migration{Version: version, Name: name})
	}
	return set
}

// Each identifier the rule does not allow is named with its file: a number of the store's own
// sequence that no file takes, a new migration taking the number after the last numbered one
// rather than a timestamp, digits that are not 14, an instant that does not exist, and a version
// two files take.
func TestCheckIdentifiersNamesEachFileThatBreaksTheRule(t *testing.T) {
	for _, tc := range []struct {
		name         string
		set          []pgmigrate.Migration
		lastNumbered int
		want         string
	}{
		{
			name:         "a numbered migration no file takes",
			set:          identifiers(t, "0001_a.up.sql", "0003_c.up.sql"),
			lastNumbered: 3,
			want:         "migration 0002 is missing: versions 1 through 3 are the numbered migrations",
		},
		{
			name:         "a new migration taking the next number",
			set:          identifiers(t, "0001_a.up.sql", "0002_b.up.sql", "0003_c.up.sql"),
			lastNumbered: 2,
			want:         "migration 0003_c.up.sql takes version 3: 2 is the last numbered migration, and every migration added since is named for the UTC moment it was created, 14 digits YYYYMMDDHHMMSS (date -u +%Y%m%d%H%M%S)",
		},
		{
			name:         "a timestamp of thirteen digits",
			set:          identifiers(t, "0001_a.up.sql", "2026103012000_short.up.sql"),
			lastNumbered: 1,
			want:         "migration 2026103012000_short.up.sql takes version 2026103012000",
		},
		{
			name:         "a timestamp of an instant that does not exist",
			set:          identifiers(t, "0001_a.up.sql", "20261332120000_month_thirteen.up.sql"),
			lastNumbered: 1,
			want:         "migration 20261332120000_month_thirteen.up.sql takes version 20261332120000",
		},
		{
			name:         "two files taking one version",
			set:          identifiers(t, "20261030120000_a.up.sql", "20261030120000_b.up.sql"),
			lastNumbered: 0,
			want:         "migrations 20261030120000_a.up.sql and 20261030120000_b.up.sql share version 20261030120000",
		},
		{
			// pgmigrate.Load refuses this one itself, so the set is written out here: a check
			// handed a version no file of a loaded set carries still names it.
			name:         "a version below one",
			set:          []pgmigrate.Migration{{Version: 0, Name: "0000_zero.up.sql"}},
			lastNumbered: 0,
			want:         "migration 0000_zero.up.sql takes version 0: a version is a positive number",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &recordingTB{TB: t}
			CheckIdentifiers(recorder, tc.set, tc.lastNumbered)
			if len(recorder.reported) != 1 || !strings.Contains(recorder.reported[0], tc.want) {
				t.Fatalf("CheckIdentifiers reported %q, want one report holding %q", recorder.reported, tc.want)
			}
		})
	}
}

// The set a store really has: its numbered migrations, each taken once and in no particular file
// order, and timestamped ones past them, including an instant a leap day makes real.
func TestCheckIdentifiersAcceptsNumbersThenTimestamps(t *testing.T) {
	set := identifiers(t,
		"0002_b.up.sql",
		"0001_a.up.sql",
		"3_c.up.sql",
		"20240229235959_leap_day.up.sql",
		"20261030120000_t1.up.sql",
	)
	recorder := &recordingTB{TB: t}
	CheckIdentifiers(recorder, set, 3)
	if len(recorder.reported) != 0 {
		t.Fatalf("CheckIdentifiers reported %q, want nothing", recorder.reported)
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
