package pgmigrate

import (
	"reflect"
	"testing"
	"time"
)

// What the watch reports after a sequence of readings. One empty reading of a wait that named its
// holders is the cancellation race between pg_locks and pg_blocking_pids and is held back; two in a
// row mean nobody holding the lock can be named - the holder left, or only a prepared transaction,
// which pg_blocking_pids reports as pid 0, still blocks - and a departed holder must not be named.
// Each row is the sequence that tells one term of the rule from its absence.
func TestObserveNamesOnlyAHolderTheLatestReadingsSupport(t *testing.T) {
	named := func(pid uint32) *LockWait {
		return &LockWait{LockType: "relation", Mode: "AccessExclusiveLock", Object: "messages", Holders: []Session{{PID: pid}}}
	}
	empty := func() *LockWait {
		return &LockWait{LockType: "relation", Mode: "AccessExclusiveLock", Object: "messages"}
	}
	otherLock := &LockWait{LockType: "relation", Mode: "AccessExclusiveLock", Object: "asks"}
	otherMode := &LockWait{LockType: "relation", Mode: "ShareLock", Object: "messages"}
	for _, tc := range []struct {
		name     string
		readings []*LockWait
		want     *LockWait
	}{
		{"one empty reading as the cancellation lands keeps the holder", []*LockWait{named(97), empty()}, named(97)},
		{"two empty readings in a row name nobody", []*LockWait{named(97), empty(), empty()}, empty()},
		{"a holder named after a held-back reading replaces it, and the next empty reading is held back again", []*LockWait{named(97), empty(), named(98), empty()}, named(98)},
		{"a holder that takes over replaces the one before it", []*LockWait{named(97), named(98)}, named(98)},
		{"an empty reading of another lock replaces at once", []*LockWait{named(97), otherLock}, otherLock},
		{"an empty reading of another mode on the same table replaces at once", []*LockWait{named(97), otherMode}, otherMode},
		{"a reading of not waiting ends the run, so the next empty one is held back", []*LockWait{named(97), empty(), nil, empty()}, named(97)},
		{"a first reading that names nobody is recorded, so the refusal names the lock", []*LockWait{empty()}, empty()},
		{"a reading of not waiting after a named one keeps it, and the next empty one is held back", []*LockWait{named(97), nil, empty()}, named(97)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var w watch
			for _, reading := range tc.readings {
				w.observe(reading)
			}
			if !reflect.DeepEqual(w.wait, tc.want) {
				t.Errorf("after %d readings the watch reports %+v, want %+v", len(tc.readings), w.wait, tc.want)
			}
		})
	}
}

// A runner that configures no Locking applies production's: five seconds, three attempts, two
// seconds apart, with the watch reading every 200 ms. Every test of the bound, the retry and the
// give-up now runs under a Locking of its own (internal/dispatch/store's testLocking), so this is
// the one place that holds the zero value to the constants a deployment gets.
func TestAZeroLockingIsProductionsDiscipline(t *testing.T) {
	zero := Locking{}.resolve()
	if zero != DefaultLocking {
		t.Errorf("a zero Locking resolves to %+v, want %+v", zero, DefaultLocking)
	}
	if zero.Timeout != 5*time.Second || zero.Attempts != 3 || zero.RetryPause != 2*time.Second {
		t.Errorf("production's discipline is %+v, want a five-second timeout, three attempts and two-second pauses", zero)
	}
	if got := (Locking{}).setting(); got != "5000ms" {
		t.Errorf("lock_timeout is set to %q, want 5000ms", got)
	}
	if got := (Locking{}).watchInterval(); got != 200*time.Millisecond {
		t.Errorf("the watch reads every %s, want the 200ms it has always read at", got)
	}
	// A runner on a short timeout still reads many times inside it, which is what names the
	// holder of the lock it gave up on.
	if got := (Locking{Timeout: 500 * time.Millisecond}).watchInterval(); got != 20*time.Millisecond {
		t.Errorf("a 500ms timeout reads every %s, want 20ms", got)
	}
}
