package pgmigrate

import (
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func file(sql string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(sql)} }

// The runners apply what Load returns in the order it returns it, so the order is the versions'
// even where the file names sort otherwise, and a rollback script is never among them.
func TestLoadReturnsForwardMigrationsInVersionOrder(t *testing.T) {
	set := fstest.MapFS{
		"migrations/0002_second.up.sql":   file("select 2"),
		"migrations/0002_second.down.sql": file("select -2"),
		"migrations/0010_tenth.up.sql":    file("select 10"),
		"migrations/9_ninth.up.sql":       file("select 9"),
	}
	got, err := Load(set, "migrations")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []Migration{
		{Version: 2, Name: "0002_second.up.sql", SQL: "select 2"},
		{Version: 9, Name: "9_ninth.up.sql", SQL: "select 9"},
		{Version: 10, Name: "0010_tenth.up.sql", SQL: "select 10"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Load = %+v, want %+v", got, want)
	}
}

// unreadable is a set in which one file is listed but cannot be read.
type unreadable struct {
	fstest.MapFS
	name string
}

var errUnreadable = errors.New("input/output error")

func (u unreadable) Open(name string) (fs.File, error) {
	if name == u.name {
		return nil, errUnreadable
	}
	return u.MapFS.Open(name)
}

func (u unreadable) ReadFile(name string) ([]byte, error) {
	if name == u.name {
		return nil, errUnreadable
	}
	return u.MapFS.ReadFile(name)
}

// Each shape Load refuses is refused whole, naming what it found, and returns no migration: a
// runner that applied the rest would apply a set other than the one written.
func TestLoadRefusesASetARunnerWouldApplyOtherThanAsWritten(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  fs.FS
		want []string
	}{
		{
			name: "two migrations share a version",
			set: fstest.MapFS{
				"migrations/0001_first.up.sql":            file("select 1"),
				"migrations/0053_asks_approval.up.sql":    file("select 53"),
				"migrations/0053_message_delivery.up.sql": file("select 53"),
			},
			want: []string{"migrations 0053_asks_approval.up.sql and 0053_message_delivery.up.sql share version 53"},
		},
		{
			name: "three migrations share a version written two ways",
			set: fstest.MapFS{
				"migrations/0007_a.up.sql": file("select 7"),
				"migrations/007_b.up.sql":  file("select 7"),
				"migrations/7_c.up.sql":    file("select 7"),
			},
			want: []string{"migrations 0007_a.up.sql, 007_b.up.sql and 7_c.up.sql share version 7"},
		},
		{
			name: "a name that does not begin with a version",
			set:  fstest.MapFS{"migrations/00x3_typo.up.sql": file("select 3")},
			want: []string{"migration 00x3_typo.up.sql: its name does not begin with a version"},
		},
		{
			name: "a signed version",
			set:  fstest.MapFS{"migrations/+3_signed.up.sql": file("select 3")},
			want: []string{"migration +3_signed.up.sql: its name does not begin with a version"},
		},
		{
			// A bare //go:embed directory pattern leaves such a name out; embedded with all:, the
			// runner is given it, and it is refused here rather than skipped.
			name: "a name beginning with an underscore",
			set:  fstest.MapFS{"migrations/_0053_hidden.up.sql": file("select 53")},
			want: []string{"migration _0053_hidden.up.sql: its name does not begin with a version"},
		},
		{
			name: "no separator after the version",
			set:  fstest.MapFS{"migrations/0003.up.sql": file("select 3")},
			want: []string{"migration 0003.up.sql: its name does not begin with a version"},
		},
		{
			name: "version zero",
			set:  fstest.MapFS{"migrations/0000_zero.up.sql": file("select 0")},
			want: []string{"migration 0000_zero.up.sql: version 0000 is not a number from 1 to 2147483647"},
		},
		{
			name: "a version past the integer column",
			set:  fstest.MapFS{"migrations/2147483648_big.up.sql": file("select 1")},
			want: []string{"migration 2147483648_big.up.sql: version 2147483648 is not a number from 1 to 2147483647"},
		},
		{
			name: "a forward migration missing .up",
			set:  fstest.MapFS{"migrations/0054_forgot_up.sql": file("select 54")},
			want: []string{"0054_forgot_up.sql is not named <version>_<name>.up.sql, <version>_<name>.down.sql or <version>_<name>.census.sql, so no runner would apply it"},
		},
		{
			name: "a file that is not SQL",
			set:  fstest.MapFS{"migrations/README.md": file("notes")},
			want: []string{"README.md is not named <version>_<name>.up.sql"},
		},
		{
			name: "a rollback script without its forward migration",
			set:  fstest.MapFS{"migrations/0054_orphan.down.sql": file("select -54")},
			want: []string{"rollback script 0054_orphan.down.sql has no forward migration 0054_orphan.up.sql"},
		},
		{
			name: "a directory",
			set:  fstest.MapFS{"migrations/0054_nested.up.sql/inner.sql": file("select 54")},
			want: []string{"0054_nested.up.sql is a directory"},
		},
		{
			name: "a file that cannot be read",
			set: unreadable{
				MapFS: fstest.MapFS{
					"migrations/0001_first.up.sql":  file("select 1"),
					"migrations/0002_broken.up.sql": file("select 2"),
				},
				name: "migrations/0002_broken.up.sql",
			},
			want: []string{"read migration 0002_broken.up.sql: input/output error"},
		},
		{
			name: "every problem at once",
			set: fstest.MapFS{
				"migrations/0001_first.up.sql":  file("select 1"),
				"migrations/0002_a.up.sql":      file("select 2"),
				"migrations/0002_b.up.sql":      file("select 2"),
				"migrations/0003_forgot_up.sql": file("select 3"),
			},
			want: []string{
				"0003_forgot_up.sql is not named",
				"migrations 0002_a.up.sql and 0002_b.up.sql share version 2",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Load(tc.set, "migrations")
			if err == nil {
				t.Fatalf("Load accepted the set and returned %+v", got)
			}
			if got != nil {
				t.Errorf("Load refused the set but returned %+v; want no migration", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Load error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

// A census file beside its migration is read onto that migration, and a migration without one
// carries none.
func TestLoadReadsACensusBesideItsMigration(t *testing.T) {
	set := fstest.MapFS{
		"migrations/0001_first.up.sql":     {Data: []byte("create table a (id integer)")},
		"migrations/0001_first.census.sql": {Data: []byte("-- rows the check would refuse\nselect count(*) from a where id < 0")},
		"migrations/0002_second.up.sql":    {Data: []byte("create table b (id integer)")},
	}
	migrations, err := Load(set, "migrations")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if migrations[0].CensusName != "0001_first.census.sql" || !strings.Contains(migrations[0].Census, "select count(*) from a") {
		t.Errorf("migration 1 census = %q (%q), want the file's text", migrations[0].CensusName, migrations[0].Census)
	}
	if migrations[1].Census != "" || migrations[1].CensusName != "" {
		t.Errorf("migration 2 declares no census, got %q", migrations[1].CensusName)
	}
}

// A census with no migration of its stem, one that is not one select, and one calling a function
// a read-only transaction does not stop are each refused, naming the census file and the reason.
// The four functions are a tripwire, matched in any case: the census runs as the service's own
// role, so terminating or cancelling a backend ends the live service's sessions, and a sleep or an
// advisory lock holds the deploy and the database for nothing a count needs.
func TestLoadRefusesACensusWithNoMigrationOneThatIsNotASelectAndOneThatCallsAForbiddenFunction(t *testing.T) {
	const first = "create table a (id integer)"
	for name, tc := range map[string]struct {
		set  fstest.MapFS
		want string
	}{
		"orphan": {
			set: fstest.MapFS{
				"migrations/0001_first.up.sql":      {Data: []byte(first)},
				"migrations/0002_absent.census.sql": {Data: []byte("select 0")},
			},
			want: "census 0002_absent.census.sql has no forward migration 0002_absent.up.sql",
		},
		"not a select": {
			set: fstest.MapFS{
				"migrations/0001_first.up.sql":     {Data: []byte(first)},
				"migrations/0001_first.census.sql": {Data: []byte("delete from a")},
			},
			want: "census 0001_first.census.sql: a census is one select answering one integer; the file's statement is not a select",
		},
		"empty": {
			set: fstest.MapFS{
				"migrations/0001_first.up.sql":     {Data: []byte(first)},
				"migrations/0001_first.census.sql": {Data: []byte("-- nothing here\n")},
			},
			want: "census 0001_first.census.sql: a census is one select answering one integer; the file holds no statement",
		},
		"terminates a backend": {
			set: fstest.MapFS{
				"migrations/0001_first.up.sql":     {Data: []byte(first)},
				"migrations/0001_first.census.sql": {Data: []byte("select count(*) from (select pg_terminate_backend(pid) from pg_stat_activity) t")},
			},
			want: "census 0001_first.census.sql: a census may not call pg_terminate_backend",
		},
		"cancels a backend": {
			set: fstest.MapFS{
				"migrations/0001_first.up.sql":     {Data: []byte(first)},
				"migrations/0001_first.census.sql": {Data: []byte("select count(*) from (select pg_cancel_backend(pid) from pg_stat_activity) t")},
			},
			want: "census 0001_first.census.sql: a census may not call pg_cancel_backend",
		},
		"sleeps": {
			set: fstest.MapFS{
				"migrations/0001_first.up.sql":     {Data: []byte(first)},
				"migrations/0001_first.census.sql": {Data: []byte("select 0 from PG_SLEEP(30)")},
			},
			want: "census 0001_first.census.sql: a census may not call pg_sleep",
		},
		"advisory lock": {
			set: fstest.MapFS{
				"migrations/0001_first.up.sql":     {Data: []byte(first)},
				"migrations/0001_first.census.sql": {Data: []byte("select case when pg_advisory_lock(1) is null then 0 end")},
			},
			want: "census 0001_first.census.sql: a census may not call pg_advisory_",
		},
	} {
		t.Run(name, func(t *testing.T) {
			migrations, err := Load(tc.set, "migrations")
			if err == nil {
				t.Fatalf("Load accepted the set and returned %d migrations", len(migrations))
			}
			if migrations != nil {
				t.Errorf("Load refused the set but returned %+v; want no migration", migrations)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A forbidden name inside a comment is not a call, and the census is read without its comments.
func TestLoadAcceptsACensusWhoseCommentNamesAForbiddenFunction(t *testing.T) {
	set := fstest.MapFS{
		"migrations/0001_first.up.sql":     {Data: []byte("create table a (id integer)")},
		"migrations/0001_first.census.sql": {Data: []byte("-- no pg_sleep here\n/* nor pg_terminate_backend */\nselect 0")},
	}
	if _, err := Load(set, "migrations"); err != nil {
		t.Fatalf("Load: %v", err)
	}
}
