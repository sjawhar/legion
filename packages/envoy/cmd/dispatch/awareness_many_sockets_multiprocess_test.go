//go:build multiprocess

package main

import (
	"fmt"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/encoding"
)

// Two hundred sockets on A, each within its own bound of four awareness clients, keep their
// clients alive, re-announcing them every ten seconds as a tab's heartbeat does: 800 clients, as
// many tabs as one user may open. Alice, opening the document on A a second later, is seen at once
// by carol on A; she must be seen by bob on B within a second too, as she would be were all three
// on one process. A room bound below what the sockets a process admits may hold (the connection
// cap times maxAwarenessClientsPerSocket) fills, refuses alice's client id and never relays it.
func TestAReaderJoiningWhileManySocketsFillTheirBoundsIsSeenOnTheOtherProcess(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "MNY")
	pair.a.createProject(t, project, "Many sockets")
	_, artifactID := pair.a.createIssueIn(t, project, "Many sockets", "x")
	bob := pair.openStoredEditor(t, pair.b, "bob", artifactID, "x\n")
	carol := pair.openStoredEditor(t, pair.a, "carol", artifactID, "x\n")

	const sockets, perSocket = 200, 4
	conns := make([]*gws.Conn, sockets)
	for i := range conns {
		conn, _, err := gws.DefaultDialer.Dial(pair.a.documentURL(artifactID), asUser(fmt.Sprintf("tab%d", i)))
		if err != nil {
			t.Fatalf("connect socket %d to A: %v", i, err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		conns[i] = conn
	}
	announce := func(clock uint64) {
		for i, conn := range conns {
			first := uint64(700_000 + i*perSocket)
			update := encoding.EncodeBytes(func(e *encoding.Encoder) {
				e.WriteVarUint(perSocket)
				for client := first; client < first+perSocket; client++ {
					e.WriteVarUint(client)
					e.WriteVarUint(clock)
					e.WriteVarString(`{"user":{"name":"tab"}}`)
				}
			})
			frame := encoding.EncodeBytes(func(e *encoding.Encoder) {
				e.WriteVarString(artifactID)
				e.WriteVarUint(1) // awareness
				e.WriteVarBytes(update)
			})
			if err := conn.WriteMessage(gws.BinaryMessage, frame); err != nil {
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
}
