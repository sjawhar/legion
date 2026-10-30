package docs

import (
	"testing"
	"time"

	"github.com/reearth/ygo/encoding"
)

// trackedAwarenessClients is how many client entries room's awareness on service holds, the
// tombstones of the clients that left included: the clients a full awareness update encodes.
func trackedAwarenessClients(t *testing.T, service *Service, room string) int {
	t.Helper()
	aw, resident := service.srv.GetAwareness(room)
	if !resident {
		t.Fatalf("no resident room %s on the service", room)
	}
	entries, err := decodeAwarenessEntries(aw.EncodeUpdate(nil))
	if err != nil {
		t.Fatalf("read the room's full awareness: %v", err)
	}
	return len(entries)
}

// awarenessFloodFrom is awarenessFlood with client ids from first: count clients, each with state,
// as one peer's frame carries them.
func awarenessFloodFrom(first, count uint64, state string) []byte {
	return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(count)
		for client := first; client < first+count; client++ {
			encoder.WriteVarUint(client)
			encoder.WriteVarUint(1)
			encoder.WriteVarString(state)
		}
	})
}

// A peer that sends frame after frame of invented awareness clients - some announced and taken
// away again, some sent as null states straight away - must not grow a room's awareness without
// limit, on the process its socket is on or on the one the relay reaches: each entry, a tombstone
// included, is memory until a purge two expiries later, and the frames cost the peer nothing.
// Here 16 frames of 1,024 clients each reach task A, half taken away again and half sent as nulls,
// and each room may track no more than its own readers plus the relay's bound of live clients per
// process: 2 * maxAwarenessClientsPerRoom.
func TestRepeatedAwarenessFloodsDoNotGrowARoomsAwarenessWithoutLimit(t *testing.T) {
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	pair.openReader(t, pair.serverA, "alice")
	pair.openReader(t, pair.serverB, "bob")
	aw, resident := pair.taskA.srv.GetAwareness(pair.artifactID)
	if !resident {
		t.Fatal("task A's room is not resident, so it has no awareness to flood")
	}

	const frames, perFrame = 16, 1024
	next := uint64(1_000_000)
	for frame := range frames {
		if frame%2 == 0 {
			if err := aw.ApplyUpdate(awarenessFloodFrom(next, perFrame, `{"user":{"name":"flood"}}`), "a peer of task A"); err != nil {
				t.Fatalf("frame %d: %v", frame, err)
			}
			if err := aw.ApplyUpdate(awarenessFloodFrom(next, perFrame, "null"), "a peer of task A"); err != nil {
				t.Fatalf("frame %d's removal: %v", frame, err)
			}
		} else if err := aw.ApplyUpdate(awarenessFloodFrom(next, perFrame, "null"), "a peer of task A"); err != nil {
			t.Fatalf("frame %d: %v", frame, err)
		}
		next += perFrame
	}

	bound := 2 * maxAwarenessClientsPerRoom
	waitFor(t, roomWait(t), "task A's relay to have sent what it relays of the frames", func() bool {
		return relayedClients(pair.taskA, pair.artifactID) == 0
	})
	onA := trackedAwarenessClients(t, pair.taskA, pair.artifactID)
	if onA > bound {
		t.Errorf("after %d frames of %d invented awareness clients, task A's room tracks %d client entries, want at most %d: each frame grows the room by its clients until a purge two expiries later",
			frames, perFrame, onA, bound)
	}
	// What task B takes in arrives through room sync: read its room until it passes the bound or
	// the relay has had three seconds, sixty coalescing windows, to land.
	var onB int
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if onB = trackedAwarenessClients(t, pair.taskB, pair.artifactID); onB > bound {
			break
		}
	}
	if onB > bound {
		t.Errorf("after %d frames of %d invented awareness clients on task A, task B's room tracks %d client entries, want at most %d: every relayed removal of a client B never held is stored as a tombstone",
			frames, perFrame, onB, bound)
	}
	t.Logf("task A's room tracks %d client entries, task B's %d (bound %d)", onA, onB, bound)
}
