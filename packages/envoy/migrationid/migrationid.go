// Package migrationid holds a set of migrations to the repository's rule about the identifiers
// their files carry, the one thing the loaders do not read: the numbered migrations a set grew up
// with are 1 to the last one its series closed at, each taken once, and every identifier past them
// is the UTC moment its author created the migration, 14 digits YYYYMMDDHHMMSS
// (date -u +%Y%m%d%H%M%S), of an instant that exists. An identifier is a number when it is
// narrower than a timestamp, so a value of 13 digits reads as a number.
//
// Closed records where each runner's numbered series ended, by the file that ends it. A set that
// holds one of those files may hold no number above it: a branch that adds the next number, as
// every author did before, would pass on its own and collide only with another branch that took
// the same number once both merged. A set that holds none of them (a test's crafted set) is held
// to numbers 1 to N with no gap.
//
// Two branches open at once cannot both take the next number without colliding: nothing conflicts
// in git, and the clash shows only once one of them merges. A timestamp belongs to the moment it
// was created, so no two authors take one identifier, and it sorts after every number, so a fresh
// database applies the numbered migrations in their old order and the timestamped ones in the
// order they were written.
//
// It is public because the Legion daemon's module and envoy hold their sets to one rule; each
// module's loader refuses a file named no version, a version below 1 and two files sharing one,
// and Check reads what is left.
package migrationid

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// timestampLayout is how an identifier past the numbered series reads as a UTC instant.
const timestampLayout = "20060102150405"

// narrowerThanATimestamp is the first identifier too wide to be one of a set's numbers: a
// timestamp is 14 digits, so everything below 10^13 is a number and everything from it up is read
// as a timestamp.
const narrowerThanATimestamp = 10_000_000_000_000

// Closed names, for each runner whose migrations moved to timestamps, the file its numbered series
// ended at and that file's number. It is raised only when a forward merge brings a numbered
// migration that main took before this rule reached it; once the rule is on main, no numbered
// migration is added again and nothing here changes.
var Closed = map[string]int{
	// The Legion daemon: packages/daemon/internal/store/migrations.
	"0061_pull_request_head_run.up.sql": 61,
	// Dispatch: packages/envoy/internal/dispatch/store/migrations.
	"0141_drop_broadcast_idempotency_keys.up.sql": 141,
}

// Check reports every way the identifiers of one set break the rule, each problem naming the file
// that carries the identifier, joined into one error; it returns nil for a set that keeps the
// rule. versions maps each version to the name of the file that carries it, which is what both
// loaders have once they have read a set.
//
// It re-checks neither a version two files share nor one below 1: both loaders refuse those
// themselves, and a map holds one name per version anyway. The numbers are read in order, and the
// first identifier that is not where the sequence puts it is reported alone: every identifier
// after a gap sits one place off, so naming them all would bury the one file to fix.
func Check(versions map[int]string) error {
	numbers := make([]int, 0, len(versions))
	timestamps := make([]int, 0, len(versions))
	for version := range versions {
		if version < narrowerThanATimestamp {
			numbers = append(numbers, version)
			continue
		}
		timestamps = append(timestamps, version)
	}
	slices.Sort(numbers)
	slices.Sort(timestamps)

	// The set's numbered series ends at the closing file it holds, or, for a set that holds none,
	// at the count of its numbers.
	last := len(numbers)
	closing := ""
	for version, name := range versions {
		if closed, ok := Closed[name]; ok && closed == version {
			last, closing = version, name
		}
	}
	var problems []error
	for i, version := range numbers {
		if version > last && closing != "" {
			problems = append(problems, fmt.Errorf(
				"migration %s takes version %d: the numbered migrations ended at %s, and every migration added since is named for the UTC moment it was created, 14 digits YYYYMMDDHHMMSS (date -u +%%Y%%m%%d%%H%%M%%S), so two branches open at once cannot take one version",
				versions[version], version, closing))
			continue
		}
		if version == i+1 {
			continue
		}
		problems = append(problems, fmt.Errorf(
			"migration %s is numbered %d where %04d comes next: the numbered migrations of a set are versions 1 to %d, each taken once, and every migration added since is named for the UTC moment it was created, 14 digits YYYYMMDDHHMMSS (date -u +%%Y%%m%%d%%H%%M%%S)",
			versions[version], version, i+1, last))
		break
	}
	for _, version := range timestamps {
		if isUTCTimestamp(version) {
			continue
		}
		problems = append(problems, fmt.Errorf(
			"migration %s takes version %d: a migration past the numbered series is named for the UTC moment it was created, 14 digits YYYYMMDDHHMMSS (date -u +%%Y%%m%%d%%H%%M%%S), an instant that exists",
			versions[version], version))
	}
	return errors.Join(problems...)
}

// isUTCTimestamp answers whether version is 14 digits of a UTC instant that exists: a month 13 or
// a 31st of February parses into neither, and anything shorter or longer is not a timestamp at
// all.
func isUTCTimestamp(version int) bool {
	digits := strconv.Itoa(version)
	if len(digits) != len(timestampLayout) {
		return false
	}
	stamp, err := time.ParseInLocation(timestampLayout, digits, time.UTC)
	return err == nil && stamp.Format(timestampLayout) == digits
}
