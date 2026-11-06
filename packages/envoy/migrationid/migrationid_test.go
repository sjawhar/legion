package migrationid

import (
	"fmt"
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
// the set's own sequence no file takes, a number past the one the series closed at, digits that
// are 13 or 15 rather than 14, and an instant that does not exist. Each crafted set is held to
// the number its own series closed at, as a runner holds its set to the one it declares.
func TestCheckRefusesAnIdentifierThatBreaksTheRule(t *testing.T) {
	for _, tc := range []struct {
		name     string
		versions map[int]string
		closedAt int
		want     string
	}{
		{
			name:     "a numbered migration no file takes",
			versions: set(t, "0001_a.up.sql", "0003_c.up.sql"),
			closedAt: 3,
			want:     "migration 0003_c.up.sql is numbered 3 where 0002 comes next",
		},
		{
			name:     "the last numbered migration no file takes",
			versions: set(t, "0001_a.up.sql", "0002_b.up.sql"),
			closedAt: 3,
			want:     "no migration is numbered 0003",
		},
		{
			name:     "a number past the one the series closed at",
			versions: set(t, "0001_a.up.sql", "0002_b.up.sql", "0003_next.up.sql"),
			closedAt: 2,
			want:     "migration 0003_next.up.sql takes version 3: the numbered migrations ended at 2",
		},
		{
			// 13 digits is a number, not a timestamp, so it is read as one of the sequence, and
			// in a set whose numbers closed at 2 it is a number taken past the series.
			name:     "a timestamp of thirteen digits",
			versions: set(t, "0001_a.up.sql", "0002_b.up.sql", "2026103012000_short.up.sql"),
			closedAt: 2,
			want:     "migration 2026103012000_short.up.sql takes version 2026103012000: the numbered migrations ended at 2",
		},
		{
			name:     "a timestamp of a month that does not exist",
			versions: set(t, "0001_a.up.sql", "20261330120000_month_thirteen.up.sql"),
			closedAt: 1,
			want:     "migration 20261330120000_month_thirteen.up.sql takes version 20261330120000",
		},
		{
			name:     "a timestamp of a day that does not exist",
			versions: set(t, "0001_a.up.sql", "20260231120000_february_thirty_first.up.sql"),
			closedAt: 1,
			want:     "migration 20260231120000_february_thirty_first.up.sql takes version 20260231120000",
		},
		{
			name:     "a timestamp of fifteen digits",
			versions: set(t, "0001_a.up.sql", "202610301200000_long.up.sql"),
			closedAt: 1,
			want:     "migration 202610301200000_long.up.sql takes version 202610301200000",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckClosedAt(tc.versions, tc.closedAt)
			if err == nil {
				t.Fatalf("Check took %v, want a refusal", tc.versions)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Check refused with %v, want a refusal holding %q", err, tc.want)
			}
		})
	}
}

// The set a store really has: its numbered migrations, 1 to the number its series closed at and
// each taken once, and timestamped ones past them, including an instant a leap day makes real.
func TestCheckTakesNumbersThenTimestamps(t *testing.T) {
	versions := set(t,
		"0002_b.up.sql",
		"0001_a.up.sql",
		"3_c.up.sql",
		"20240229235959_leap_day.up.sql",
		"20261030120000_t1.up.sql",
	)
	if err := CheckClosedAt(versions, 3); err != nil {
		t.Fatalf("Check refused a set that keeps the rule: %v", err)
	}
}

// A set that breaks the rule twice over is refused once, naming both files: an author reading the
// refusal fixes every identifier it names rather than one per run.
func TestCheckReportsEveryProblem(t *testing.T) {
	versions := set(t, "0001_a.up.sql", "0003_c.up.sql", "20261332120000_month_thirteen.up.sql")
	err := CheckClosedAt(versions, 3)
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

// A set takes no number above the one its series closed at: a branch that adds the next number
// passes on its own and collides with another that took the same number. The refusal names the
// file to rename and the number the series ended at, which is what tells its author what to name
// it instead.
func TestCheckRefusesANumberAboveTheOneTheSeriesClosedAt(t *testing.T) {
	const closedAt = 5
	versions := map[int]string{}
	for version := 1; version <= closedAt; version++ {
		versions[version] = fmt.Sprintf("%04d_m.up.sql", version)
	}
	versions[20261030120000] = "20261030120000_later.up.sql"
	if err := CheckClosedAt(versions, closedAt); err != nil {
		t.Fatalf("a set closed at %d with a timestamp after it was refused: %v", closedAt, err)
	}
	next := fmt.Sprintf("%04d_next.up.sql", closedAt+1)
	versions[closedAt+1] = next
	err := CheckClosedAt(versions, closedAt)
	if err == nil {
		t.Fatalf("a set closed at %d accepted %s", closedAt, next)
	}
	if !strings.Contains(err.Error(), next) {
		t.Errorf("the refusal does not name %s: %v", next, err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("ended at %d", closedAt)) {
		t.Errorf("the refusal does not name the number the series ended at: %v", err)
	}
}

// Every number past the closing one is named, not only the first: each is a file to rename, and
// an author fixing one would otherwise come back for the next.
func TestCheckNamesEveryNumberAboveTheClosingOne(t *testing.T) {
	versions := set(t, "0001_a.up.sql", "0002_b.up.sql", "0003_c.up.sql")
	err := CheckClosedAt(versions, 1)
	if err == nil {
		t.Fatal("a set closed at 1 holding 2 and 3 was taken, want a refusal")
	}
	for _, name := range []string{"0002_b.up.sql", "0003_c.up.sql"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("the refusal does not name %s: %v", name, err)
		}
	}
}

// A closing number below 1 names no migration, so no set can keep it: it is the caller's mistake,
// and Check says so rather than passing a set it checked nothing about.
func TestCheckRefusesAClosingNumberBelowOne(t *testing.T) {
	versions := set(t, "0001_a.up.sql")
	err := CheckClosedAt(versions, 0)
	if err == nil {
		t.Fatal("Check took a closing number of 0, want a refusal")
	}
	if !strings.Contains(err.Error(), "mistake in the caller") {
		t.Errorf("the refusal does not say the caller declared it wrong: %v", err)
	}
}

// Each runner's series closes once: a second entry for one runner, or one file named by two,
// would let the set's answer depend on which entry was read.
func TestEachSeriesClosesOnce(t *testing.T) {
	series, files := map[string]bool{}, map[string]bool{}
	for _, closing := range closings {
		if series[closing.series] || files[closing.file] {
			t.Errorf("closings names %s or %s twice", closing.series, closing.file)
		}
		series[closing.series], files[closing.file] = true, true
		if closing.closedAt < 1 || !strings.HasPrefix(closing.file, fmt.Sprintf("%04d_", closing.closedAt)) {
			t.Errorf("closings records %s at %d, which is not the number its name carries", closing.file, closing.closedAt)
		}
	}
}

// Check finds a runner's set by the closing file it holds, refuses the next number in it, and
// refuses a set that holds no closing file or the closing files of two series, rather than guess.
func TestCheckFindsTheSeriesByItsClosingFile(t *testing.T) {
	for _, closing := range closings {
		versions := map[int]string{}
		for v := 1; v < closing.closedAt; v++ {
			versions[v] = fmt.Sprintf("%04d_m.up.sql", v)
		}
		versions[closing.closedAt] = closing.file
		versions[20261030120000] = "20261030120000_later.up.sql"
		if err := Check(versions); err != nil {
			t.Fatalf("%s's set with a timestamp after it was refused: %v", closing.series, err)
		}
		next := fmt.Sprintf("%04d_next.up.sql", closing.closedAt+1)
		versions[closing.closedAt+1] = next
		if err := Check(versions); err == nil || !strings.Contains(err.Error(), next) {
			t.Errorf("%s's set plus %s: %v, want a refusal naming it", closing.series, next, err)
		}
	}
	if err := Check(map[int]string{1: "0001_a.up.sql", 2: "0002_b.up.sql"}); err == nil {
		t.Error("a set holding no closing file was accepted")
	}
	both := map[int]string{}
	for _, closing := range closings {
		both[closing.closedAt] = closing.file
	}
	if len(closings) > 1 {
		if err := Check(both); err == nil {
			t.Error("a set holding two series' closing files was accepted")
		}
	}
}

// ClosedAt answers where one set's numbers end, found the way Check finds it: a set holding a
// series' closing file and a timestamp past it reads as that series, and a set holding no closing
// file or the closing files of two is refused rather than given a number.
func TestClosedAtIsTheClosingNumberOfTheSeriesTheSetHolds(t *testing.T) {
	for _, closing := range closings {
		versions := set(t, closing.file, "20261030120000_later.up.sql")
		closedAt, err := ClosedAt(versions)
		if err != nil {
			t.Fatalf("%s's set with a timestamp after it: %v", closing.series, err)
		}
		if closedAt != closing.closedAt {
			t.Errorf("%s's set closed at %d, want %d", closing.series, closedAt, closing.closedAt)
		}
	}
	if closedAt, err := ClosedAt(set(t, "0001_a.up.sql", "0002_b.up.sql")); err == nil {
		t.Errorf("a set holding no closing file was read as closing at %d", closedAt)
	}
	if len(closings) > 1 {
		both := map[int]string{}
		for _, closing := range closings {
			both[closing.closedAt] = closing.file
		}
		if closedAt, err := ClosedAt(both); err == nil {
			t.Errorf("a set holding two series' closing files was read as closing at %d", closedAt)
		}
	}
}
