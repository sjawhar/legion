//go:build multiprocess

package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/encoding"
)

// One tab on A sends a single awareness frame that names 2,048 clients, then leaves. A reader who
// opens the document on A a moment later must still be shown to a reader on B within a second, as
// a reader on A is shown to the other readers on A at once. With the room's client bound at 256
// (maxAwarenessClientsPerRoom), B's room is full of the frame's clients, which stay there until
// they expire and their tombstones are purged (about 75 s, though the flooding tab left at once), and
// B drops every client it has not seen before: the new reader stays invisible on B for that long,
// while on A it is seen at once.
func TestAReaderJoiningAfterAnAwarenessFloodIsSeenOnTheOtherProcess(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "FLD")
	pair.a.createProject(t, project, "Awareness flood")
	_, artifactID := pair.a.createIssueIn(t, project, "Flooded", "x")
	bob := pair.openStoredEditor(t, pair.b, "bob", artifactID, "x\n")
	carol := pair.openStoredEditor(t, pair.a, "carol", artifactID, "x\n")

	flood, _, err := gws.DefaultDialer.Dial(pair.a.documentURL(artifactID), asUser("mallory"))
	if err != nil {
		t.Fatalf("connect the flooding tab to A: %v", err)
	}
	go func() {
		for {
			if _, _, err := flood.ReadMessage(); err != nil {
				return
			}
		}
	}()
	const invented = 2048
	update := encoding.EncodeBytes(func(e *encoding.Encoder) {
		e.WriteVarUint(invented)
		for i := range invented {
			e.WriteVarUint(uint64(500_000 + i))
			e.WriteVarUint(1)
			e.WriteVarString(fmt.Sprintf(`{"user":{"name":"invented %d"}}`, i))
		}
	})
	frame := encoding.EncodeBytes(func(e *encoding.Encoder) {
		e.WriteVarString(artifactID)
		e.WriteVarUint(1) // awareness
		e.WriteVarBytes(update)
	})
	if err := flood.WriteMessage(gws.BinaryMessage, frame); err != nil {
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
