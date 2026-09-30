// Package pgmigrate holds what Envoy's two Postgres migration runners share: Dispatch's
// (internal/dispatch/store) and the secrets broker's (internal/broker/store). Both record an
// applied migration by its version alone and apply every version they have not recorded, so both
// trust the set they embed to give each version one file, and both run a migration's statements
// under locks that production's reads and writes queue behind. Load refuses a set either runner
// would apply other than as written, before the runner opens a transaction; LockTimeout bounds
// every lock wait a migration makes; and a Watch names the lock a migration gave up on and the
// session that held it.
package pgmigrate

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path"
	"slices"
	"strconv"
	"strings"
)

// Migration is one numbered forward migration.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Load reads every entry of dir in fsys and returns the forward migrations in version order.
//
// An entry is <version>_<name>.up.sql, a forward migration, or <version>_<name>.down.sql, a
// rollback script an operator runs by hand and no runner applies. <version> is decimal digits
// naming a number from 1 to the largest the runners' integer version column holds. Load refuses
// the whole set, naming the files, when:
//
//   - two forward migrations share a version: a runner records a migration by its version, so it
//     would apply the first, pass over the rest as already applied, and report success;
//   - an entry is named any other way: a runner reading only *.up.sql would never see it, while
//     whoever wrote it believes it runs;
//   - a rollback script has no forward migration of its name;
//   - a forward migration cannot be read.
//
// It reports every problem it finds and returns no migration when it finds one, so a runner that
// calls it before opening a transaction applies nothing from a set it refuses. The order is the
// versions', never the file names': 9_a.up.sql runs before 0010_b.up.sql. Versions need not be
// contiguous; whether a repository lets a set skip a number is a rule about how it hands numbers
// out, which a store's own tests state.
func Load(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("list migrations in %s: %w", dir, err)
	}
	present := make(map[string]bool, len(entries))
	for _, entry := range entries {
		present[entry.Name()] = true
	}
	var problems []error
	migrations := make([]Migration, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			problems = append(problems, fmt.Errorf("%s is a directory, not a migration named <version>_<name>.up.sql", name))
			continue
		}
		if stem, ok := strings.CutSuffix(name, ".down.sql"); ok {
			if !present[stem+".up.sql"] {
				problems = append(problems, fmt.Errorf("rollback script %s has no forward migration %s.up.sql", name, stem))
			}
			continue
		}
		stem, ok := strings.CutSuffix(name, ".up.sql")
		if !ok {
			problems = append(problems, fmt.Errorf(
				"%s is not named <version>_<name>.up.sql or <version>_<name>.down.sql, so no runner would apply it", name))
			continue
		}
		version, err := parseVersion(stem)
		if err != nil {
			problems = append(problems, fmt.Errorf("migration %s: %w", name, err))
			continue
		}
		sql, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			problems = append(problems, fmt.Errorf("read migration %s: %w", name, err))
			continue
		}
		migrations = append(migrations, Migration{Version: version, Name: name, SQL: string(sql)})
	}
	// Stable, so files that share a version stay in name order for the refusal that names them.
	slices.SortStableFunc(migrations, func(a, b Migration) int { return a.Version - b.Version })
	for start := 0; start < len(migrations); {
		end := start + 1
		for end < len(migrations) && migrations[end].Version == migrations[start].Version {
			end++
		}
		if end-start > 1 {
			problems = append(problems, fmt.Errorf(
				"migrations %s share version %d: a runner records a migration by its version, so it would apply the first and skip the rest as already applied; renumber all but one",
				joinNames(migrations[start:end]), migrations[start].Version))
		}
		start = end
	}
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	return migrations, nil
}

// parseVersion reads the version a forward migration's stem begins with.
func parseVersion(stem string) (int, error) {
	digits, _, ok := strings.Cut(stem, "_")
	if !ok || digits == "" || strings.Trim(digits, "0123456789") != "" {
		return 0, errors.New("its name does not begin with a version: <version>_<name>.up.sql, <version> decimal digits")
	}
	version, err := strconv.ParseInt(digits, 10, 32)
	if err != nil || version < 1 {
		return 0, fmt.Errorf("version %s is not a number from 1 to %d", digits, math.MaxInt32)
	}
	return int(version), nil
}

// joinNames lists the migrations' file names for a sentence: a and b, or a, b and c.
func joinNames(migrations []Migration) string {
	names := make([]string, len(migrations))
	for i, migration := range migrations {
		names[i] = migration.Name
	}
	last := len(names) - 1
	return strings.Join(names[:last], ", ") + " and " + names[last]
}
