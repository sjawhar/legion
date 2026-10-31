package docs

import (
	"testing"
	"time"
)

// The per-socket bound holds on a real websocket upgrade, not only on a connection a test hands
// the filter: a socket that announces ten awareness clients reaches the other readers of its room,
// on its own process and on the other, as maxAwarenessClientsPerSocket of them. gorilla reads an
// upgraded connection through the buffered reader the hijack returns, which in the gorilla this
// repository's workspace builds is not reset onto the connection the hijack hands back, so a
// filter that only wraps the connection never sees a frame.
func TestADocumentSocketHoldsItsAwarenessBoundThroughARealUpgrade(t *testing.T) {
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	carol := pair.openReader(t, pair.serverA, "carol")
	bob := pair.openReader(t, pair.serverB, "bob")
	mallory := pair.openReader(t, pair.serverA, "mallory")

	const announced = 10
	for client := uint64(500_000); client < 500_000+announced; client++ {
		if err := mallory.SetAwareness(client, map[string]any{"user": map[string]any{"name": "mallory"}}); err != nil {
			t.Fatalf("announce client %d: %v", client, err)
		}
	}
	time.Sleep(2 * time.Second)
	onA, onB := 0, 0
	for client := range carol.AwarenessStates() {
		if client >= 500_000 && client < 500_000+announced {
			onA++
		}
	}
	for client := range bob.AwarenessStates() {
		if client >= 500_000 && client < 500_000+announced {
			onB++
		}
	}
	if onA > maxAwarenessClientsPerSocket || onB > maxAwarenessClientsPerSocket {
		t.Fatalf("one document socket announced %d awareness clients and carol on task A holds %d of them, bob on task B %d; want at most %d each (maxAwarenessClientsPerSocket): the filter never read the socket's frames",
			announced, onA, onB, maxAwarenessClientsPerSocket)
	}
	if onA == 0 {
		t.Fatalf("carol on task A holds none of the socket's clients, so the socket's awareness never reached its room")
	}
}
