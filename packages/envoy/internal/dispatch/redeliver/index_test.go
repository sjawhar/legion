package redeliver

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/routing"
)

// A sweep reads its index through the filters pendingFilters answers, so those filters are the
// window: a minute none of them selects loses every delivery due in it until something else brings
// it back, and a minute outside the window costs the sweep entries it is not owed, which is the
// growth the index exists to hold down. They also have to be pairwise disjoint, since the server
// refuses a consumer whose filter subjects overlap outright, and no more than maxPendingFilters,
// which is what bounds one index read whatever a sweep's cursor says.
//
// Which keys a filter selects is routing.Match, the matcher the listener's own routing is built on
// (internal/routing): dots separate a key's tokens, `*` stands for one of them and `>` for one or
// more, as the server reads the consumer behind a watch (internal/bus/kv.go). The window is judged
// by matching keys with it rather than by deriving the days, hours and minutes a decomposition
// should hold, which would only restate pendingFilters.
func TestPendingFiltersCoverTheWindowWithoutOverlapping(t *testing.T) {
	base := time.Date(2025, 3, 14, 9, 26, 53, 0, time.UTC)
	day := time.Date(2025, 3, 14, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name           string
		floor, ceiling time.Time
	}{
		{"a few minutes", base, base.Add(3 * time.Minute)},
		{"crossing an hour", base, base.Add(45 * time.Minute)},
		{"crossing a day", day.Add(23*time.Hour + 50*time.Minute), day.Add(24*time.Hour + 10*time.Minute)},
		{"a minute into a day to a minute into the fourth", day.Add(time.Minute), day.Add(72*time.Hour + time.Minute)},
		{"the full retention", base.Add(-retention), base},
	} {
		t.Run(tc.name, func(t *testing.T) {
			window := tc.floor.Format(time.RFC3339) + " to " + tc.ceiling.Format(time.RFC3339)
			filters := pendingFilters(tc.floor, tc.ceiling)
			if len(filters) > maxPendingFilters {
				t.Fatalf("%d filters for the window %s, want no more than %d", len(filters), window, maxPendingFilters)
			}
			// Every minute of the window, and three hours of them on either side: a filter that
			// reaches past the window reaches into the day, hour or minute beside it, so the
			// minutes just outside are what catch it.
			from, to := minute(tc.floor), minute(tc.ceiling)
			used := make([]bool, len(filters))
			for at := from.Add(-3 * time.Hour); !at.After(to.Add(3 * time.Hour)); at = at.Add(time.Minute) {
				key := pendingKey(at, "guid-a")
				var matched []string
				for i, filter := range filters {
					if routing.Match(filter, key) {
						matched = append(matched, filter)
						used[i] = true
					}
				}
				inside := !at.Before(from) && !at.After(to)
				switch {
				case inside && len(matched) == 0:
					t.Fatalf("no filter of %v selects %s, a minute of the window %s", filters, key, window)
				case inside && len(matched) > 1:
					t.Fatalf("the filters %v all select %s, and the server refuses a consumer whose filters overlap", matched, key)
				case !inside && len(matched) > 0:
					t.Fatalf("the filters %v select %s, a minute outside the window %s", matched, key, window)
				}
			}
			for i, filter := range filters {
				if !used[i] {
					t.Fatalf("the filter %s selects no minute of the window %s, so what it reads is not the window's", filter, window)
				}
			}
			// A filter that selects a minute of the window and another far from it holds a
			// wildcard where the window has a time, which the minutes beside the window cannot
			// show.
			for _, away := range []time.Duration{365 * 24 * time.Hour, 30 * 24 * time.Hour, 7 * 24 * time.Hour} {
				for _, at := range []time.Time{from.Add(-away), to.Add(away)} {
					key := pendingKey(at, "guid-a")
					for _, filter := range filters {
						if routing.Match(filter, key) {
							t.Fatalf("the filter %s selects %s, %s from the window %s", filter, key, away, window)
						}
					}
				}
			}
			// A GUID holding dots of its own is still one key: `>` selects every token after the
			// minute, however many there are.
			dotted := pendingKey(from, "guid.a.b")
			matched := 0
			for _, filter := range filters {
				if routing.Match(filter, dotted) {
					matched++
				}
			}
			if matched != 1 {
				t.Fatalf("%d filters of %v select %s, want exactly one", matched, filters, dotted)
			}
		})
	}
}
