//go:build multiprocess

package main

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
)

// Five readers on A, each on a socket of its own, place their cursors at the same moment, as tabs
// do when a document loads for several people or every tab reconnects after a deploy. Bob on B
// must see all five within a second, as a reader on A does. A room relaying only the newest of
// its pending awareness changes loses the others until each reader's next heartbeat (15 s in
// y-protocols), and these readers send none.
func TestFiveReadersPlacingCursorsAtOnceAreAllSeenOnTheOtherProcess(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "SIM")
	pair.a.createProject(t, project, "Simultaneous cursors")
	_, artifactID := pair.a.createIssueIn(t, project, "Five at once", "x")
	bob := pair.openStoredEditor(t, pair.b, "bob", artifactID, "x\n")
	const readers = 5
	editors := make([]*docstest.Peer, readers)
	for i := range editors {
		editors[i] = pair.openStoredEditor(t, pair.a, fmt.Sprintf("reader%d", i), artifactID, "x\n")
	}
	carol := pair.openStoredEditor(t, pair.a, "carol", artifactID, "x\n")
	for i, editor := range editors {
		if err := editor.SetAwareness(uint64(8000+i), cursorState(fmt.Sprintf("reader%d", i), i)); err != nil {
			t.Fatalf("reader %d's cursor: %v", i, err)
		}
	}
	all := func(states map[uint64]json.RawMessage) bool {
		for i := range readers {
			if !holdsState(uint64(8000+i), cursorState(fmt.Sprintf("reader%d", i), i))(states) {
				return false
			}
		}
		return true
	}
	if err := carol.WaitForAwareness(liveWithin, all); err != nil {
		t.Fatalf("carol on A did not see all five readers on A within %v: %v", liveWithin, err)
	}
	if err := bob.WaitForAwareness(liveWithin, all); err != nil {
		held := []uint64{}
		for client := range bob.AwarenessStates() {
			held = append(held, client)
		}
		t.Fatalf("five readers on A placed their cursors at once: carol on A saw all five, bob on B holds only %v after %v", held, liveWithin)
	}
}
