package tmux

import (
	"reflect"
	"testing"
	"time"
)

// Owned windows the daemon does not know, from `list-windows -F
// "#{window_id}\t#{@legion_owner}\t#{window_activity}"` (tmux.ts:455-489): a window marked with
// another owner, or with none, is someone else's and never a candidate.
func TestReadUnknownWindows(t *testing.T) {
	stdout := "@0\t\t1790103302\n" + // the bootstrap window: no owner marker
		"@1\tlegion-omp\t1790103302\n" + // known
		"@2\tlegion-omp\t1790100000\n" + // an orphan
		"@3\tlegion-other\t1790103302\n" + // another daemon's
		"@4\tlegion-omp\tnot-a-time\n" + // cannot be timed: never reaped
		"garbage\n"
	got := readUnknownWindows(stdout, "legion-omp", map[string]bool{"@1": true})
	want := []ownedWindow{{id: "@2", activity: time.Unix(1790100000, 0)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("readUnknownWindows = %+v, want %+v", got, want)
	}
}

// Unknown worker-shim panes anywhere on the server, in owned windows (tmux.ts:491-545). The row
// shape is tmux 3.7c's own rendering of a `/bin/sh -c` pane (captured on the devbox): the start
// command's newlines and tabs are printed escaped, and the activity column is the window's.
func TestReadUnknownPanes(t *testing.T) {
	stdout := "%0\t@0\t\t\"sleep 3600\"\t1790103321\n" +
		"%1\t@1\tlegion-omp\t/bin/sh -c \"cd /w && /opt/legion worker-shim --connect unix:///s -- omp \\\"a\\n\\nb\\ttab\\\"\"\t1790103321\n" +
		"%2\t@1\tlegion-omp\t/bin/sh -c \"cd /w && /opt/legion worker-shim --connect unix:///s -- omp\"\t1790103322\n" +
		"%3\t@1\tlegion-omp\tbash\t1790103322\n" + // not a Legion process: never reaped here
		"%4\t@5\tlegion-other\t/bin/sh -c \"legion worker-shim\"\t1790103322\n" + // another daemon's
		"%5\t@1\tlegion-omp\t/bin/sh -c \"legion worker-shim\"\t\n" // cannot be timed
	got := readUnknownPanes(stdout, "legion-omp", map[string]bool{"%2": true})
	want := []ownedPane{{id: "%1", window: "@1", activity: time.Unix(1790103321, 0)}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("readUnknownPanes = %+v, want %+v", got, want)
	}
}
