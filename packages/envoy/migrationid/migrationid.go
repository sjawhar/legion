// Package migrationid holds a set of migrations to the repository's rule about the identifiers
// their files carry, the one thing the loaders do not read: the numbered migrations a set grew up
// with are 1 to the number its series closed at, each taken once, and every identifier past them
// is the UTC moment its author created the migration, 14 digits YYYYMMDDHHMMSS
// (date -u +%Y%m%d%H%M%S), of an instant that exists. An identifier is a number when it is
// narrower than a timestamp, so a value of 13 digits reads as a number.
//
// Where each runner's numbered series closed is recorded once, in closings, by the runner's name,
// the file the series ended at and that file's number. Check finds the one series a set belongs
// to by the closing file it holds and holds the set to the numbers 1 to that one, each present,
// and nothing numbered above it: a branch that adds the next number, as every author did before,
// would otherwise pass on its own and collide only with another branch that took the same number
// once both merged. A set that holds no closing file, or the closing files of two series, is
// refused rather than guessed at. A series' closing is raised only when a forward merge brings a
// numbered migration that main took before this rule reached it; once the rule is on main, no
// numbered migration is added again and nothing in closings changes.
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

// closings records, once per runner, the file its numbered series ended at and that file's
// number. Each runner and each file appears once; TestEachSeriesClosesOnce holds it to that.
var closings = []struct {
	series   string
	file     string
	closedAt int
}{
	{series: "the Legion daemon (packages/daemon/internal/store/migrations)", file: "0064_pull_request_pass_settlement.up.sql", closedAt: 64},
	{series: "Dispatch (packages/envoy/internal/dispatch/store/migrations)", file: "0149_event_horizon.up.sql", closedAt: 149},
}

// ClosedAt is the number the numbered series of this set closed at, the series found by the
// closing file the set holds (closings): exactly one must match, so a set is read against its own
// series' closing number and never against another's or a guess. It refuses a set that holds no
// closing file, which a set from before this rule or a mistyped closing entry would be, and one
// that holds two. A caller that only needs where the numbers end, such as a test taking the
// numbered migrations of a set its runner keeps adding timestamped ones to, reads it here rather
// than writing the number down a second time.
func ClosedAt(versions map[int]string) (int, error) {
	var matched []int
	for i, closing := range closings {
		if versions[closing.closedAt] == closing.file {
			matched = append(matched, i)
		}
	}
	switch len(matched) {
	case 1:
		return closings[matched[0]].closedAt, nil
	case 0:
		return 0, errors.New("the migration set holds none of the files a numbered series closed at, so it is no runner's set this rule knows: a runner whose migrations moved to timestamps records its closing file in migrationid's closings, and a crafted set is checked with CheckClosedAt")
	default:
		names := make([]string, 0, len(matched))
		for _, i := range matched {
			names = append(names, closings[i].file)
		}
		return 0, fmt.Errorf("the migration set holds the closing files of %d series (%v), so it belongs to none of them alone", len(matched), names)
	}
}

// Check holds one runner's set to the rule, against the closing number ClosedAt finds for it, so
// a set that names no series or two is refused there rather than guessed at. Every other problem
// is CheckClosedAt's.
func Check(versions map[int]string) error {
	closedAt, err := ClosedAt(versions)
	if err != nil {
		return err
	}
	return CheckClosedAt(versions, closedAt)
}

// CheckClosedAt reports every way the identifiers of one set break the rule, each problem naming
// the file that carries the identifier, joined into one error; it returns nil for a set that keeps
// the rule. versions maps each version to the name of the file that carries it, which is what both
// loaders have once they have read a set, and closedAt is the number that set's numbered series
// closed at.
//
// It re-checks neither a version two files share nor one below 1: both loaders refuse those
// themselves, and a map holds one name per version anyway. The numbers are read in order, and the
// first identifier that is not where the sequence puts it is reported alone: every identifier
// after a gap sits one place off, so naming them all would bury the one file to fix.
//
// A closedAt below 1 names no migration, so it is the caller's mistake rather than the set's, and
// it is returned as such: a set checked against nothing would pass while checking nothing.
func CheckClosedAt(versions map[int]string, closedAt int) error {
	if closedAt < 1 {
		return fmt.Errorf(
			"the migration set was checked against closing number %d: the number a set's numbered series closed at is 1 or more, so this is a mistake in the caller that declares it, not in the set",
			closedAt)
	}

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

	var problems []error
	// The numbers split where the series closed: below the closing number they are the sequence,
	// and each one above it is a file to rename, so every one of those is named. A branch holding
	// one is no reason to leave the next unreported.
	split := slices.IndexFunc(numbers, func(version int) bool { return version > closedAt })
	if split < 0 {
		split = len(numbers)
	}
	numbered, beyond := numbers[:split], numbers[split:]
	for _, version := range beyond {
		problems = append(problems, fmt.Errorf(
			"migration %s takes version %d: the numbered migrations ended at %d, and every migration added since is named for the UTC moment it was created, 14 digits YYYYMMDDHHMMSS (date -u +%%Y%%m%%d%%H%%M%%S), so two branches open at once cannot take one version",
			versions[version], version, closedAt))
	}
	// The rest are the sequence 1 to closedAt, each taken once.
	for i := range closedAt {
		if i < len(numbered) && numbered[i] == i+1 {
			continue
		}
		if i < len(numbered) {
			problems = append(problems, fmt.Errorf(
				"migration %s is numbered %d where %04d comes next: the numbered migrations of a set are versions 1 to %d, each taken once, and every migration added since is named for the UTC moment it was created, 14 digits YYYYMMDDHHMMSS (date -u +%%Y%%m%%d%%H%%M%%S)",
				versions[numbered[i]], numbered[i], i+1, closedAt))
			break
		}
		problems = append(problems, fmt.Errorf(
			"no migration is numbered %04d: the numbered migrations of a set are versions 1 to %d, each taken once, and every migration added since is named for the UTC moment it was created, 14 digits YYYYMMDDHHMMSS (date -u +%%Y%%m%%d%%H%%M%%S)",
			i+1, closedAt))
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
