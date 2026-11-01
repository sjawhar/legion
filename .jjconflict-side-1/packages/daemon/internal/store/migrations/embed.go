// Package migrations carries the daemon's schema as SQL embedded in the binary,
// so a daemon can migrate a database with nothing but itself. Migrations are
// forward-only: each is applied once, in version order, and never reversed.
//
// A migration's file is named <version>_<slug>.up.sql, and its version is the
// leading digits read as an integer. The numbered versions are the sequence the
// daemon grew up with; each of those files keeps the number it landed with.
// Every migration added since is named for the UTC moment its author created
// it, 14 digits YYYYMMDDHHMMSS (date -u +%Y%m%d%H%M%S), so two branches open at
// the same time cannot both claim one version the way two branches each taking
// the next number did. A timestamp sorts after every numbered migration and
// timestamps sort among themselves by time, and migrationid.Check holds a set
// to that rule: the daemon's numbered series closed at
// 0067_phase_assignment.up.sql (migrationid's closings), so a set holding a
// number above it is refused.
package migrations

import (
	"cmp"
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/migrationid"
)

//go:embed *.up.sql
var files embed.FS

// Migration is one forward migration, identified by the version its file name
// carries.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// All reads every embedded migration in ascending version order.
func All() ([]Migration, error) {
	return Load(files)
}

// Load reads every *.up.sql in fsys in ascending version order. A file whose
// name carries no version, and two files sharing one, are refused by name:
// either would otherwise apply in an order nobody chose or be skipped in
// silence. So is a set migrationid.Check refuses, which is what keeps a daemon
// carrying a version two branches could collide on from booting at all.
func Load(fsys fs.FS) ([]Migration, error) {
	return load(fsys, migrationid.Check)
}

// load is Load with the identifier check given, so a test can read a crafted set
// of its own few numbered migrations against its own closing number
// (migrationid.CheckClosedAt) rather than one shaped like the embedded set.
func load(fsys fs.FS, check func(map[int]string) error) ([]Migration, error) {
	names, err := fs.Glob(fsys, "*.up.sql")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	all := make([]Migration, 0, len(names))
	for _, name := range names {
		version, err := versionOf(name)
		if err != nil {
			return nil, err
		}
		sql, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, err)
		}
		all = append(all, Migration{Version: version, Name: name, SQL: string(sql)})
	}
	slices.SortFunc(all, func(a, b Migration) int { return cmp.Compare(a.Version, b.Version) })
	versions := make(map[int]string, len(all))
	for i, migration := range all {
		if i > 0 && migration.Version == all[i-1].Version {
			return nil, fmt.Errorf("migrations %s and %s share version %d", all[i-1].Name, migration.Name, migration.Version)
		}
		versions[migration.Version] = migration.Name
	}
	if err := check(versions); err != nil {
		return nil, err
	}
	return all, nil
}

func versionOf(name string) (int, error) {
	stem, ok := strings.CutSuffix(name, ".up.sql")
	if !ok {
		return 0, fmt.Errorf("migration %q does not end in .up.sql", name)
	}
	digits, _, ok := strings.Cut(stem, "_")
	if !ok {
		return 0, fmt.Errorf("migration %q is not named <version>_<slug>.up.sql", name)
	}
	version, err := strconv.Atoi(digits)
	if err != nil || version < 1 {
		return 0, fmt.Errorf("migration %q does not begin with a version number", name)
	}
	return version, nil
}
