// Package migrations carries the daemon's schema as SQL embedded in the binary,
// so a daemon can migrate a database with nothing but itself. Migrations are
// forward-only: each is applied once, in version order, and never reversed.
package migrations

import (
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
)

//go:embed *.up.sql
var files embed.FS

// Migration is one numbered forward migration.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// All reads every embedded migration in ascending version order. A file whose
// name carries no version, and two files sharing one, are refused by name:
// either would otherwise apply in an order nobody chose or be skipped in
// silence.
func All() ([]Migration, error) {
	names, err := fs.Glob(files, "*.up.sql")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	all := make([]Migration, 0, len(names))
	for _, name := range names {
		version, err := versionOf(name)
		if err != nil {
			return nil, err
		}
		sql, err := files.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, err)
		}
		all = append(all, Migration{Version: version, Name: name, SQL: string(sql)})
	}
	slices.SortFunc(all, func(a, b Migration) int { return a.Version - b.Version })
	for i := 1; i < len(all); i++ {
		if all[i].Version == all[i-1].Version {
			return nil, fmt.Errorf("migrations %s and %s share version %d", all[i-1].Name, all[i].Name, all[i].Version)
		}
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
