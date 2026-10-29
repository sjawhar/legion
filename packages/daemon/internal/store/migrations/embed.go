// Package migrations carries the daemon's schema as SQL embedded in the binary,
// so a daemon can migrate a database with nothing but itself. Migrations are
// forward-only: each is applied once, in version order, and never reversed.
//
// A migration's file is named <version>_<slug>.up.sql, and its version is the
// leading digits read as an integer. Versions 1 through LastNumbered are the
// sequence the daemon grew up with; each of those files keeps the number it
// landed with. Every migration added since is named for the UTC moment its
// author created it, 14 digits YYYYMMDDHHMMSS (date -u +%Y%m%d%H%M%S), so two
// branches open at the same time cannot both claim one version the way two
// branches each taking the next number did. A timestamp sorts after every
// numbered migration and timestamps sort among themselves by time, and
// CheckIdentifiers holds a set to that rule.
package migrations

import (
	"cmp"
	"embed"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"time"
)

//go:embed *.up.sql
var files embed.FS

// LastNumbered is the highest version the numbered migrations reached,
// 0061_pull_request_head_run.up.sql. Nothing is added at or below it again:
// every version above it is a timestamp.
const LastNumbered = 61

// timestampLayout is how a version above LastNumbered reads as a UTC instant.
const timestampLayout = "20060102150405"

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
// silence. So is a set CheckIdentifiers refuses, which is what keeps a daemon
// carrying a version two branches could collide on from booting at all.
func Load(fsys fs.FS) ([]Migration, error) {
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
	for i := 1; i < len(all); i++ {
		if all[i].Version == all[i-1].Version {
			return nil, fmt.Errorf("migrations %s and %s share version %d", all[i-1].Name, all[i].Name, all[i].Version)
		}
	}
	if err := CheckIdentifiers(all); err != nil {
		return nil, err
	}
	return all, nil
}

// CheckIdentifiers holds a set of migrations to the identifier rule: versions 1
// through LastNumbered appear exactly once each, and every other version is a
// 14-digit UTC timestamp — real digits of a real instant, so that one author's
// migration cannot take the identifier another author's already has. It reads
// nothing and writes nothing; Load runs it, and a test can run it on a set it
// crafts.
func CheckIdentifiers(all []Migration) error {
	byVersion := make(map[int]string, len(all))
	for _, migration := range all {
		if other, taken := byVersion[migration.Version]; taken {
			return fmt.Errorf("migrations %s and %s share version %d", other, migration.Name, migration.Version)
		}
		byVersion[migration.Version] = migration.Name
	}
	for version := 1; version <= LastNumbered; version++ {
		if _, present := byVersion[version]; !present {
			return fmt.Errorf("migration %04d is missing: versions 1 through %d are the numbered migrations, each of which appears exactly once", version, LastNumbered)
		}
	}
	for _, migration := range all {
		if migration.Version < 1 {
			return fmt.Errorf("migration %s takes version %d: a version is a positive number", migration.Name, migration.Version)
		}
		if migration.Version <= LastNumbered {
			continue
		}
		if !isTimestamp(migration.Version) {
			return fmt.Errorf("migration %s takes version %d: a migration added after %04d is named for the UTC moment it was created, 14 digits YYYYMMDDHHMMSS (date -u +%%Y%%m%%d%%H%%M%%S)", migration.Name, migration.Version, LastNumbered)
		}
	}
	return nil
}

// isTimestamp answers whether version is 14 digits of a UTC instant that exists:
// a month 13 or a 31st of April parses into neither, and anything shorter or
// longer is not a timestamp at all.
func isTimestamp(version int) bool {
	digits := strconv.Itoa(version)
	if len(digits) != len(timestampLayout) {
		return false
	}
	stamp, err := time.ParseInLocation(timestampLayout, digits, time.UTC)
	return err == nil && stamp.Format(timestampLayout) == digits
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
