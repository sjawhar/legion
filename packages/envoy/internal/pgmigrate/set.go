// Package pgmigrate holds what Envoy's two Postgres migration runners share: Dispatch's
// (internal/dispatch/store) and the secrets broker's (internal/broker/store). Both record an
// applied migration by its version alone and apply every version they have not recorded, so both
// trust the set they embed to give each version one file, and both run a migration's statements
// under locks that production's reads and writes queue behind. Load refuses a set either runner
// would apply other than as written, before the runner opens a transaction; Exec applies each
// migration with its lock waits bounded by LockTimeout, and names the lock a migration gave up on
// and the sessions that held it; Census reads, before a deployment applies the migrations a
// database has not, what they would lock and the rows their own censuses count, and writes
// nothing.
package pgmigrate

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Migration is one numbered forward migration.
type Migration struct {
	Version int
	Name    string
	SQL     string
	// Census is the text of <version>_<name>.census.sql when the migration declares one: one
	// select answering one integer, the number of existing rows the migration would refuse or
	// rewrite, which a deployment reads with Census before it applies the migration. Empty when
	// the migration declares none. No runner runs it.
	Census string
	// CensusName is the census file's name, for messages; empty when Census is.
	CensusName string
}

// Load reads every entry of dir in fsys and returns the forward migrations in version order.
//
// An entry is <version>_<name>.up.sql, a forward migration, <version>_<name>.down.sql, a
// rollback script an operator runs by hand and no runner applies, or <version>_<name>.census.sql,
// the migration's census (Migration.Census), which a deployment reads with Census and no runner
// runs. <version> is decimal digits naming a number from 1 to the largest the runners' integer
// version column holds. Load refuses the whole set, naming the files, when:
//
//   - two forward migrations share a version: a runner records a migration by its version, so it
//     would apply the first, pass over the rest as already applied, and report success;
//   - an entry is named any other way: a runner reading only *.up.sql would never see it, while
//     whoever wrote it believes it runs;
//   - a rollback script or a census has no forward migration of its name;
//   - a census is not one select, or calls pg_terminate_backend, pg_cancel_backend, pg_sleep or
//     an advisory-lock function (checkCensus);
//   - a forward migration or a census cannot be read.
//
// It reports every problem it finds and returns no migration when it finds one, so a runner that
// calls it before opening a transaction applies nothing from a set it refuses. The order is the
// versions', never the file names': 9_a.up.sql runs before 0010_b.up.sql. Versions need not be
// contiguous for a runner to apply them; that a set skips no number is the repository's rule
// about handing numbers out, which pgmigratetest.CheckNumberedOneToN holds both stores' sets to.
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
	// A census can be listed before its migration, so the texts are kept by stem and attached
	// once every migration is read.
	censuses := make(map[string]string)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			problems = append(problems, fmt.Errorf("%s is a directory, not a migration named <version>_<name>.up.sql", name))
			continue
		}
		if stem, ok := strings.CutSuffix(name, ".census.sql"); ok {
			if !present[stem+".up.sql"] {
				problems = append(problems, fmt.Errorf("census %s has no forward migration %s.up.sql", name, stem))
				continue
			}
			text, err := fs.ReadFile(fsys, path.Join(dir, name))
			if err != nil {
				problems = append(problems, fmt.Errorf("read census %s: %w", name, err))
				continue
			}
			if err := checkCensus(string(text)); err != nil {
				problems = append(problems, fmt.Errorf("census %s: %w", name, err))
				continue
			}
			censuses[stem] = string(text)
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
				"%s is not named <version>_<name>.up.sql, <version>_<name>.down.sql or <version>_<name>.census.sql, so no runner would apply it", name))
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
	for i := range migrations {
		stem := strings.TrimSuffix(migrations[i].Name, ".up.sql")
		if text, ok := censuses[stem]; ok {
			migrations[i].Census = text
			migrations[i].CensusName = stem + ".census.sql"
		}
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
				"migrations %s share version %d: a runner records a migration by its version, so it would apply the first and skip the rest as already applied; keep the number on the file that merged to main first, since a database may already have applied it, and renumber the rest to the next free numbers on main",
				joinNames(migrations[start:end]), migrations[start].Version))
		}
		start = end
	}
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	return migrations, nil
}

// checkCensus refuses a census that is not one select: the deployment runs it inside a read-only
// transaction and requires one integer back (Census), so anything else fails there too, but a set
// that cannot be run as written is refused here, before any runner opens a transaction, as the
// rest of Load's rules are. It also refuses the functions a read-only transaction does not stop
// and no census needs: pg_terminate_backend and pg_cancel_backend (the census runs as the
// service's own role, so either ends the live service's sessions), pg_sleep, and the advisory
// lock family. A census is production-executed code; this is a tripwire, and review is the
// control.
func checkCensus(text string) error {
	body := strings.ToLower(strings.TrimSpace(stripSQLComments(text)))
	if body == "" {
		return errors.New("a census is one select answering one integer; the file holds no statement")
	}
	if !strings.HasPrefix(body, "select") && !strings.HasPrefix(body, "with") {
		return errors.New("a census is one select answering one integer; the file's statement is not a select")
	}
	for _, forbidden := range censusForbiddenFunctions {
		if strings.Contains(body, forbidden) {
			return fmt.Errorf("a census may not call %s: it reads counts and nothing else", forbidden)
		}
	}
	return nil
}

// censusForbiddenFunctions are matched as substrings of the lowercased, comment-stripped census.
var censusForbiddenFunctions = []string{"pg_terminate_backend", "pg_cancel_backend", "pg_sleep", "pg_advisory_"}

// stripSQLComments removes -- comments to the end of their line and /* */ blocks. It does not
// read string literals, so a -- or /* inside one is removed as a comment would be: checkCensus
// reads only a statement's first word and four function names, and TouchedTables' reading of the
// repository's own sets is held to them by pgmigratetest.CheckTouchedTablesAreKnown.
func stripSQLComments(sql string) string {
	sql = blockComment.ReplaceAllString(sql, " ")
	return lineComment.ReplaceAllString(sql, "")
}

var (
	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	lineComment  = regexp.MustCompile(`--[^\n]*`)
)

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
