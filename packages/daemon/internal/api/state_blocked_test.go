package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// The state's `blocked` is every issue Legion is waiting to start behind a Dispatch issue nobody
// has finished — a waiting root and a child held at `admitted` alike — in one array in issue-key
// order, each with the keys it waits on. The root is in `waiting` as well, which keeps its own
// meaning: the roots' waiting line, which no held child is part of.
func TestTheStateCarriesBlockedRootsAndChildrenInOneArray(t *testing.T) {
	encoded, err := json.Marshal(Admission{
		Cap:     2,
		Active:  []string{"LEGION-208"},
		Waiting: []string{"LEGION-211"},
		Blocked: []BlockedIssue{
			{Issue: "LEGION-209", BlockedBy: []string{"LEGION-300"}},
			{Issue: "LEGION-211", BlockedBy: []string{"LEGION-301", "LEGION-302"}},
		},
	})
	if err != nil {
		t.Fatalf("marshal the admission: %v", err)
	}
	want := `"waiting":["LEGION-211"],` +
		`"blocked":[{"issue":"LEGION-209","blockedBy":["LEGION-300"]},` +
		`{"issue":"LEGION-211","blockedBy":["LEGION-301","LEGION-302"]}]`
	if !strings.Contains(string(encoded), want) {
		t.Fatalf("admission = %s, want it to carry %s", encoded, want)
	}
}

// Both the list and each row's keys are arrays on the wire, empty rather than null: the plugin's
// strict reader refuses a null, and an admission with nothing blocked is the common case.
func TestTheStatesBlockedIsNeverNull(t *testing.T) {
	encoded, err := json.Marshal(Admission{Cap: 2})
	if err != nil {
		t.Fatalf("marshal the admission: %v", err)
	}
	if !strings.Contains(string(encoded), `"blocked":[]`) {
		t.Fatalf("admission = %s, want an empty blocked array rather than null", encoded)
	}

	row, err := json.Marshal(BlockedIssue{Issue: "LEGION-209"})
	if err != nil {
		t.Fatalf("marshal the blocked issue: %v", err)
	}
	if got, want := string(row), `{"issue":"LEGION-209","blockedBy":[]}`; got != want {
		t.Fatalf("blocked issue = %s, want %s", got, want)
	}
}
