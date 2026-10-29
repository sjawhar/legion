package migrationid

import (
	"strconv"
	"strings"
	"testing"
)

// set reads migration files named as a store's are, each file's version its leading digits, into
// the map a loader hands Check.
func set(t *testing.T, names ...string) map[int]string {
	t.Helper()
	versions := make(map[int]string, len(names))
	for _, name := range names {
		digits, _, _ := strings.Cut(strings.TrimSuffix(name, ".up.sql"), "_")
		version, err := strconv.Atoi(digits)
		if err != nil {
			t.Fatalf("test set names %s, whose version is not digits: %v", name, err)
		}
		versions[version] = name
	}
	return versions
}

// Each identifier the rule does not allow is refused with the file that carries it: a number of
// the set's own sequence no file takes, digits that are 13 or 15 rather than 14, and an instant
// that does not exist.
func TestCheckRefusesAnIdentifierThatBreaksTheRule(t *testing.T) {
	for _, tc := range []struct {
		name     string
		versions map[int]string
		want     string
	}{
		{
			name:     "a numbered migration no file takes",
			versions: set(t, "0001_a.up.sql", "0003_c.up.sql"),
			want:     "migration 0003_c.up.sql is numbered 3 where 0002 comes next",
		},
		{
			// 13 digits is a number, not a timestamp, so it is read as one of the sequence, and
			// in a set whose numbers run 1 to N it is the number that leaves a gap.
			name:     "a timestamp of thirteen digits",
			versions: set(t, "0001_a.up.sql", "0002_b.up.sql", "2026103012000_short.up.sql"),
			want:     "migration 2026103012000_short.up.sql is numbered 2026103012000 where 0003 comes next",
		},
		{
			name:     "a timestamp of a month that does not exist",
			versions: set(t, "0001_a.up.sql", "20261330120000_month_thirteen.up.sql"),
			want:     "migration 20261330120000_month_thirteen.up.sql takes version 20261330120000",
		},
		{
			name:     "a timestamp of a day that does not exist",
			versions: set(t, "0001_a.up.sql", "20260231120000_february_thirty_first.up.sql"),
			want:     "migration 20260231120000_february_thirty_first.up.sql takes version 20260231120000",
		},
		{
			name:     "a timestamp of fifteen digits",
			versions: set(t, "0001_a.up.sql", "202610301200000_long.up.sql"),
			want:     "migration 202610301200000_long.up.sql takes version 202610301200000",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(tc.versions)
			if err == nil {
				t.Fatalf("Check took %v, want a refusal", tc.versions)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Check refused with %v, want a refusal holding %q", err, tc.want)
			}
		})
	}
}

// The set a store really has: its numbered migrations, each taken once, and timestamped ones past
// them, including an instant a leap day makes real.
func TestCheckTakesNumbersThenTimestamps(t *testing.T) {
	versions := set(t,
		"0002_b.up.sql",
		"0001_a.up.sql",
		"3_c.up.sql",
		"20240229235959_leap_day.up.sql",
		"20261030120000_t1.up.sql",
	)
	if err := Check(versions); err != nil {
		t.Fatalf("Check refused a set that keeps the rule: %v", err)
	}
}

// A set that breaks the rule twice over is refused once, naming both files: an author reading the
// refusal fixes every identifier it names rather than one per run.
func TestCheckReportsEveryProblem(t *testing.T) {
	versions := set(t, "0001_a.up.sql", "0003_c.up.sql", "20261332120000_month_thirteen.up.sql")
	err := Check(versions)
	if err == nil {
		t.Fatal("Check took a set that breaks the rule twice, want a refusal")
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("Check refused with %T, want the joined problems", err)
	}
	if reported := joined.Unwrap(); len(reported) != 2 {
		t.Fatalf("Check reported %d problems, want the 2 the set has: %v", len(reported), reported)
	}
	for _, name := range []string{"0003_c.up.sql", "20261332120000_month_thirteen.up.sql"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal does not name %s: %v", name, err)
		}
	}
}
