//go:build multiprocess

package main

import (
	"fmt"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// Two hundred sockets on A, each within its own bound of four awareness clients
// (MaxAwarenessClientsPerSocketForTest), keep their clients alive, re-announcing them every ten
// seconds as a tab's heartbeat does: 800 clients, as many tabs as one user may open, every one of
// them a client a socket is allowed and no flood. Alice, opening the document on A a second
// later, is seen at once by carol on A; she must be seen by bob on B within a second too, as she
// would be were all three on one process. A room bound below what the sockets a process admits
// may hold - the connection cap times maxAwarenessClientsPerSocket - fills and leaves neither
// room a place for her: A refuses the clients its own sockets are allowed, and B's room, full of
// the ones it was relayed, refuses her client id, so the reader carol sees on A is seen by nobody
// on B. The room's bound is derived from those two caps instead
// (MaxAwarenessClientsPerRoomForTest), and the entries the rooms track stay under it.
func TestAReaderJoiningWhileManySocketsFillTheirBoundsIsSeenOnTheOtherProcess(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "MNY")
	pair.a.createProject(t, project, "Many sockets")
	_, artifactID := pair.a.createIssueIn(t, project, "Many sockets", "x")
	bob := pair.openStoredEditor(t, pair.b, "bob", artifactID, "x\n")
	carol := pair.openStoredEditor(t, pair.a, "carol", artifactID, "x\n")

	const sockets, perSocket = 200, docs.MaxAwarenessClientsPerSocketForTest
	kept := sockets * perSocket
	conns := make([]*gws.Conn, sockets)
	for i := range conns {
		conns[i] = dialTabSocket(t, pair.a, artifactID, fmt.Sprintf("tab%d", i))
	}
	tabState := func(uint64) string { return `{"user":{"name":"tab"}}` }
	announce := func(clock uint64) {
		for i, conn := range conns {
			first := uint64(700_000 + i*perSocket)
			if err := announceAwarenessClients(conn, artifactID, first, perSocket, clock, tabState); err != nil {
				t.Errorf("socket %d's announcement at clock %d: %v", i, clock, err)
			}
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

	// Every socket's four clients are in A's room before the reader opens the document, so the
	// room she joins is as full as those sockets can make it.
	deadline := time.Now().Add(30 * time.Second)
	tracked := pair.a.roomCounters(t, artifactID).AwarenessClients
	for tracked < kept && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		tracked = pair.a.roomCounters(t, artifactID).AwarenessClients
	}
	if tracked < kept {
		// Not fatal: the reader who opens the document next is what this row is about, and a room
		// that refused these clients refuses hers too.
		t.Errorf("A's room tracks %d awareness client entries, want at least the %d clients %d sockets may hold (%d each): the room refused clients its own sockets are allowed",
			tracked, kept, sockets, perSocket)
	}

	alice := pair.openStoredEditor(t, pair.a, "alice", artifactID, "x\n")
	if err := alice.SetAwareness(aliceAwarenessClient, cursorState("alice", 0)); err != nil {
		t.Fatalf("announce alice's cursor: %v", err)
	}
	if err := carol.WaitForAwareness(liveWithin, holdsState(aliceAwarenessClient, cursorState("alice", 0))); err != nil {
		t.Fatalf("carol on A did not see alice within %v: %v", liveWithin, err)
	}
	if err := bob.WaitForAwareness(liveWithin, holdsState(aliceAwarenessClient, cursorState("alice", 0))); err != nil {
		t.Fatalf("alice, who opened the document on A while %d sockets there each kept their %d awareness clients alive, was seen at once by carol on A but not by bob on B within %v (bob holds %d clients): A's room bound is below what the sockets it admits may hold, so it refuses her client id and never relays it",
			sockets, perSocket, liveWithin, len(bob.AwarenessStates()))
	}

	for _, process := range []*dispatchProcess{pair.a, pair.b} {
		held := process.roomCounters(t, artifactID).AwarenessClients
		if held > docs.MaxAwarenessClientsPerRoomForTest {
			t.Errorf("%s's room tracks %d awareness client entries, want at most the room's bound of %d",
				pair.name(process), held, docs.MaxAwarenessClientsPerRoomForTest)
		}
		t.Logf("%s's room tracks %d awareness client entries (bound %d)", pair.name(process), held, docs.MaxAwarenessClientsPerRoomForTest)
	}
}
