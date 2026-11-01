// Package classify contains the pure event-decision rules the workflow applies to durable records.
package classify

import (
	"sort"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// AttemptSetOrder describes an incoming check-run set relative to the stored per-name fence.
type AttemptSetOrder string

const (
	AttemptSetNewer AttemptSetOrder = "newer"
	AttemptSetEqual AttemptSetOrder = "equal"
	AttemptSetOlder AttemptSetOrder = "older"
	AttemptSetMixed AttemptSetOrder = "mixed"
)

// CompareAttemptSets orders only names reported by the incoming observation. Stored-only names are
// deliberately ignored because an incomplete observation may omit a check that remains fenced.
func CompareAttemptSets(stored, incoming []record.AttemptRun) AttemptSetOrder {
	known := make(map[string]int64, len(stored))
	for _, run := range stored {
		known[run.Name] = run.ID
	}

	var higher, lower bool
	for _, run := range incoming {
		storedID, found := known[run.Name]
		if !found || run.ID > storedID {
			higher = true
		} else if run.ID < storedID {
			lower = true
		}
	}

	switch {
	case higher && lower:
		return AttemptSetMixed
	case higher:
		return AttemptSetNewer
	case lower:
		return AttemptSetOlder
	default:
		return AttemptSetEqual
	}
}

// mergeAttemptSets is the fence after accepting incoming: for each check name the whole run of
// highest id over both sets, sorted by name. The run is kept whole - its id, the head GitHub
// reported it on and what it concluded - because those facts belong to one run, and a name that
// took the id of one run and the conclusion of another would report a conclusion no run of it
// ever had. Nothing is pruned, so a check a settlement leaves out keeps its run, and an older
// view of that check stays older. An equal id is the same run reported again and the later report
// is taken, which is how a run stored before it carried a head and a conclusion gains them.
func mergeAttemptSets(stored, incoming []record.AttemptRun) []record.AttemptRun {
	merged := make(map[string]record.AttemptRun, len(stored)+len(incoming))
	for _, set := range [][]record.AttemptRun{stored, incoming} {
		for _, run := range set {
			if known, found := merged[run.Name]; !found || run.ID >= known.ID {
				merged[run.Name] = run
			}
		}
	}
	fence := make([]record.AttemptRun, 0, len(merged))
	for _, run := range merged {
		fence = append(fence, run)
	}
	sort.Slice(fence, func(i, j int) bool { return fence[i].Name < fence[j].Name })
	return fence
}
