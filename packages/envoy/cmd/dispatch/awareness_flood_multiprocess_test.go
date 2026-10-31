//go:build multiprocess

package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// One tab on A sends a single awareness frame that names 2,048 clients, then leaves. A reader who
// opens the document on A a moment later must still be shown to a reader on B within a second, as
// a reader on A is shown to the other readers on A at once. With a room client bound below what
// the sockets of a process may hold (maxAwarenessClientsPerRoom), B's room is full of the frame's
// clients, which stay there until they expire and their tombstones are purged (about 75 s, though
// the flooding tab left at once), and B drops every client it has not seen before: the new reader
// stays invisible on B for that long, while on A it is seen at once.
func TestAReaderJoiningAfterAnAwarenessFloodIsSeenOnTheOtherProcess(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "FLD")
	pair.a.createProject(t, project, "Awareness flood")
	_, artifactID := pair.a.createIssueIn(t, project, "Flooded", "x")
	bob := pair.openStoredEditor(t, pair.b, "bob", artifactID, "x\n")
	carol := pair.openStoredEditor(t, pair.a, "carol", artifactID, "x\n")

	flood := dialTabSocket(t, pair.a, artifactID, "mallory")
	const invented = 2048
	if err := announceAwarenessClients(flood, artifactID, 500_000, invented, 1, inventedState(500_000)); err != nil {
		t.Fatalf("send the flooding frame: %v", err)
	}
	if err := bob.WaitForAwareness(5*time.Second, func(states map[uint64]json.RawMessage) bool { return len(states) > 0 }); err != nil {
		t.Fatalf("none of the flooding frame's clients reached bob on B: %v", err)
	}
	flood.Close()
	time.Sleep(time.Second)

	alice := pair.openStoredEditor(t, pair.a, "alice", artifactID, "x\n")
	if err := alice.SetAwareness(aliceAwarenessClient, cursorState("alice", 0)); err != nil {
		t.Fatalf("announce alice's cursor: %v", err)
	}
	if err := carol.WaitForAwareness(liveWithin, holdsState(aliceAwarenessClient, cursorState("alice", 0))); err != nil {
		t.Fatalf("carol on A did not see alice, who joined A after the flood, within %v: %v", liveWithin, err)
	}
	if err := bob.WaitForAwareness(liveWithin, holdsState(aliceAwarenessClient, cursorState("alice", 0))); err != nil {
		t.Fatalf("alice, who opened the document on A a second after one tab's %d-client awareness frame, was seen at once by carol on A but not by bob on B within %v: B's room still counts the frame's clients, the tombstones of the ones that left included, against its bound, and drops every client it has not seen before",
			invented, liveWithin)
	}
}

// The same flood, from a tab that stays: it keeps its 2,048 invented clients alive, re-announcing
// them every ten seconds as a tab's heartbeat does. Alice, opening the document on A a second
// later, is seen at once by carol on A; she must be seen by bob on B within a second too, as she
// would be were all three on one process. With a room bound that counts every client a room holds
// (maxAwarenessClientsPerRoom), A's room is full of the tab's clients, refuses alice's client id,
// and never relays it: bob sees her only once the flooding tab stops re-announcing and its clients
// expire.
func TestAReaderJoiningDuringALiveAwarenessFloodIsSeenOnTheOtherProcess(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "FLL")
	pair.a.createProject(t, project, "Live awareness flood")
	_, artifactID := pair.a.createIssueIn(t, project, "Flooded live", "x")
	bob := pair.openStoredEditor(t, pair.b, "bob", artifactID, "x\n")
	carol := pair.openStoredEditor(t, pair.a, "carol", artifactID, "x\n")

	flood := dialTabSocket(t, pair.a, artifactID, "mallory")
	const invented = 2048
	announce := func(clock uint64) {
		if err := announceAwarenessClients(flood, artifactID, 600_000, invented, clock, inventedState(600_000)); err != nil {
			t.Errorf("announce the flooding tab's clients at clock %d: %v", clock, err)
		}
	}
	announce(1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for clock := uint64(2); ; clock++ {
			select {
			case <-stop:
				return
			case <-ticker.C:
				announce(clock)
			}
		}
	}()
	time.Sleep(time.Second)

	alice := pair.openStoredEditor(t, pair.a, "alice", artifactID, "x\n")
	if err := alice.SetAwareness(aliceAwarenessClient, cursorState("alice", 0)); err != nil {
		t.Fatalf("announce alice's cursor: %v", err)
	}
	if err := carol.WaitForAwareness(liveWithin, holdsState(aliceAwarenessClient, cursorState("alice", 0))); err != nil {
		t.Fatalf("carol on A did not see alice within %v: %v", liveWithin, err)
	}
	if err := bob.WaitForAwareness(liveWithin, holdsState(aliceAwarenessClient, cursorState("alice", 0))); err != nil {
		t.Fatalf("alice, who opened the document on A while one tab there kept %d invented awareness clients alive, was seen at once by carol on A but not by bob on B within %v: A's room is full of that tab's clients and never relays hers",
			invented, liveWithin)
	}
}

// inventedState is the state a flooding tab gives each of the client ids it invents from first:
// a cursor-sized one, as the tabs of this suite's readers announce.
func inventedState(first uint64) func(client uint64) string {
	return func(client uint64) string {
		return fmt.Sprintf(`{"user":{"name":"invented %d"}}`, client-first)
	}
}
