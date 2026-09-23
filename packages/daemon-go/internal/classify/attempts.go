// Package classify contains the pure event-decision rules the workflow applies to durable records.
package classify

import "github.com/sjawhar/legion/daemon/internal/record"

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
