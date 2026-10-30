//go:build multiprocess

package main

import (
	"fmt"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/encoding"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// Two hundred sockets of one person open the same document on A, each announcing eight awareness
// clients, of which the filter keeps the four a socket may hold
// (MaxAwarenessClientsPerSocketForTest): eight hundred clients, every one of them a client a
// socket is allowed and no flood. A reader who opens the document on A after them must be seen by
// carol on A at once and by bob on B within a second, as she would be with nobody else in the
// room. A room bound below what the sockets a process admits may hold - 256 entries, which
// sixty-four such sockets fill - leaves neither room with a place for her: it refuses the
// clients the sockets on A are allowed, and B's room, full of the ones it was relayed, refuses
// her client id, so the reader carol sees on A is seen by nobody on B. The room's bound is
// derived from those two caps instead (MaxAwarenessClientsPerRoomForTest), and the entries the
// rooms track stay under it.
func TestTwoHundredSocketsOfOneUserDoNotFillARoomAgainstANewReader(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "MNY")
	pair.a.createProject(t, project, "Many sockets")
	_, artifactID := pair.a.createIssueIn(t, project, "Many sockets", "x")
	bob := pair.openStoredEditor(t, pair.b, "bob", artifactID, "x\n")
	carol := pair.openStoredEditor(t, pair.a, "carol", artifactID, "x\n")

	const sockets, announcedPerSocket = 200, 2 * docs.MaxAwarenessClientsPerSocketForTest
	kept := sockets * docs.MaxAwarenessClientsPerSocketForTest
	for socket := range sockets {
		tab, response, err := gws.DefaultDialer.Dial(pair.a.documentURL(artifactID), asUser("mallory"))
		if err != nil {
			t.Fatalf("connect socket %d of %d to A: response=%#v err=%v: a process admits a thousand connections, so two hundred are taken in",
				socket+1, sockets, response, err)
		}
		t.Cleanup(func() { _ = tab.Close() })
		go func() {
			for {
				if _, _, err := tab.ReadMessage(); err != nil {
					return
				}
			}
		}()
		first := uint64(700_000 + socket*announcedPerSocket)
		update := encoding.EncodeBytes(func(e *encoding.Encoder) {
			e.WriteVarUint(announcedPerSocket)
			for client := first; client < first+announcedPerSocket; client++ {
				e.WriteVarUint(client)
				e.WriteVarUint(1)
				e.WriteVarString(fmt.Sprintf(`{"user":{"name":"mallory %d"}}`, client))
			}
		})
		frame := encoding.EncodeBytes(func(e *encoding.Encoder) {
			e.WriteVarString(artifactID)
			e.WriteVarUint(1) // awareness
			e.WriteVarBytes(update)
		})
		if err := tab.WriteMessage(gws.BinaryMessage, frame); err != nil {
			t.Fatalf("announce socket %d's %d awareness clients: %v", socket+1, announcedPerSocket, err)
		}
	}

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
			tracked, kept, sockets, docs.MaxAwarenessClientsPerSocketForTest)
	}

	alice := pair.openStoredEditor(t, pair.a, "alice", artifactID, "x\n")
	if err := alice.SetAwareness(aliceAwarenessClient, cursorState("alice", 0)); err != nil {
		t.Fatalf("announce alice's cursor: %v", err)
	}
	if err := carol.WaitForAwareness(liveWithin, holdsState(aliceAwarenessClient, cursorState("alice", 0))); err != nil {
		t.Fatalf("carol on A did not see alice, who opened the document on A behind %d sockets holding %d awareness clients, within %v: %v",
			sockets, kept, liveWithin, err)
	}
	if err := bob.WaitForAwareness(liveWithin, holdsState(aliceAwarenessClient, cursorState("alice", 0))); err != nil {
		t.Fatalf("alice, who opened the document on A behind %d sockets holding %d awareness clients, was seen by carol on A but not by bob on B within %v: A's room is full and never relays her",
			sockets, kept, liveWithin)
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
