package pgmigratetest

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
)

// Each entry the binary leaves out gets the advice that fixes it: all: brings in a _ or . name, and
// nothing embeds a symlink, a version-control name, an empty directory or an irregular file, so
// those are named for what they are rather than sent back to all:.
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
				if err := os.Mkdir(filepath.Join(dir, "empty"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: "migrations/empty is a directory holding nothing //go:embed can carry",
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
				if err := syscall.Mkfifo(filepath.Join(dir, "0002_b.up.sql"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "migrations/0002_b.up.sql is not a regular file (a FIFO, socket or device), which no //go:embed directive embeds; remove it",
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
