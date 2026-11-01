package wait

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// A wait that names its moment is a wait as any other, and UntilAt reads the moment back from
// anywhere in the chain; a wait that names none, or a failure, has none.
func TestUntilIsAWaitThatNamesItsMoment(t *testing.T) {
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	err := Until(at, "tree LEGION-300 lingers past its deadline; member LEGION-301's production check is open")
	if !errors.Is(err, ErrWaiting) {
		t.Fatalf("errors.Is(Until, ErrWaiting) = false, want a wait")
	}
	if err.Error() != "tree LEGION-300 lingers past its deadline; member LEGION-301's production check is open" {
		t.Fatalf("Until message = %q", err.Error())
	}
	got, ok := UntilAt(err)
	if !ok || !got.Equal(at) {
		t.Fatalf("UntilAt(Until) = %s, %v; want %s, true", got, ok, at)
	}
	wrapped := fmt.Errorf("linger of LEGION-300: %w", err)
	if got, ok := UntilAt(wrapped); !ok || !got.Equal(at) {
		t.Fatalf("UntilAt of a wrapped Until = %s, %v; want %s, true", got, ok, at)
	}
	if !errors.Is(wrapped, ErrWaiting) {
		t.Fatalf("a wrapped Until is no longer a wait")
	}
	for name, other := range map[string]error{
		"a plain wait":      New("start waits for a turn"),
		"a formatted wait":  Errorf("start of %s waits", "LEGION-300"),
		"a failure":         errors.New("GitHub refused"),
		"a wrapped failure": fmt.Errorf("compare: %w", errors.New("GitHub refused")),
	} {
		if got, ok := UntilAt(other); ok || !got.IsZero() {
			t.Errorf("UntilAt(%s) = %s, %v; want the zero time and false", name, got, ok)
		}
	}
}
