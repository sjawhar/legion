package docs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/encoding"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
)

// Awareness - readers' cursors and presence - crosses processes through the awareness relay
// (awareness_relay.go): each process sends its own rooms' awareness on documentAwarenessChannel and
// applies the others' to a room of its own that a reader of its own is in. The tests here run two
// services on one database, as production's two tasks are, each with its own document socket and
// its own room sync.

// The awareness client ids the readers below announce, as a Yjs provider announces its
// document's client id.
const (
	aliceClient uint64 = 1001
	bobClient   uint64 = 2002
)

// runAwareness runs service's room sync, as cmd/dispatch does, and returns a channel that receives
// each awareness notification once the service's relay has handled it.
func runAwareness(t *testing.T, service *Service) <-chan awarenessMessage {
	t.Helper()
	rs := newRoomSync(service)
	relay := rs.awareness
	handled := make(chan awarenessMessage, 256)
	rs.awareness = func(ctx context.Context, payload string) {
		relay(ctx, payload)
		var message awarenessMessage
		if err := json.Unmarshal([]byte(payload), &message); err != nil {
			t.Errorf("an awareness notification does not parse: %v: %s", err, payload)
			return
		}
		select {
		case handled <- message:
		default:
			t.Errorf("the test read no note of an awareness notification for %s", message.Room)
		}
	}
	runRoomSync(t, rs)
	return handled
}

// cursorOf is a reader's awareness state as the dashboard announces it.
func cursorOf(login string, at int) map[string]any {
	return map[string]any{"user": map[string]any{"name": login}, "cursor": map[string]any{"anchor": at, "head": at}}
}

// holdsAwareness is a WaitForAwareness condition: client's state is want, compared as JSON values.
func holdsAwareness(client uint64, want any) func(map[uint64]json.RawMessage) bool {
	encoded, _ := json.Marshal(want)
	var wanted any
	_ = json.Unmarshal(encoded, &wanted)
	return func(states map[uint64]json.RawMessage) bool {
		raw, present := states[client]
		if !present {
			return false
		}
		var got any
		return json.Unmarshal(raw, &got) == nil && reflect.DeepEqual(got, wanted)
	}
}

// lacksAwareness is a WaitForAwareness condition: client has no state.
func lacksAwareness(client uint64) func(map[uint64]json.RawMessage) bool {
	return func(states map[uint64]json.RawMessage) bool {
		_, present := states[client]
		return !present
	}
}

// roomHoldsClient reports whether service's resident room of artifactID holds a state of client.
func roomHoldsClient(service *Service, artifactID string, client uint64) bool {
	aw, resident := service.srv.GetAwareness(artifactID)
	if !resident {
		return false
	}
	_, held := aw.GetStates()[client]
	return held
}

// setAwareness announces client's state through reader, failing the test when it cannot.
func setAwareness(t *testing.T, reader *docstest.Peer, client uint64, state any) {
	t.Helper()
	if err := reader.SetAwareness(client, state); err != nil {
		t.Fatalf("announce client %d's awareness: %v", client, err)
	}
}

// requireAwareness waits for reader to see states that hold, failing with what as the cause.
func requireAwareness(t *testing.T, reader *docstest.Peer, holds func(map[uint64]json.RawMessage) bool, what string) {
	t.Helper()
	started := time.Now()
	if err := reader.WaitForAwareness(roomWait(t), holds); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	t.Logf("%s: %v", what, time.Since(started).Round(time.Millisecond))
}

// A reader's cursor set on one process reaches a reader of the same document on the other, each
// way, and so does its next move.
func TestAwarenessSetOnOneProcessReachesAReaderOnTheOther(t *testing.T) {
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	bob := pair.openReader(t, pair.serverB, "bob")

	setAwareness(t, alice, aliceClient, cursorOf("alice", 1))
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursorOf("alice", 1)), "alice's cursor on task A reaching bob on task B")
	setAwareness(t, bob, bobClient, cursorOf("bob", 2))
	requireAwareness(t, alice, holdsAwareness(bobClient, cursorOf("bob", 2)), "bob's cursor on task B reaching alice on task A")
	setAwareness(t, alice, aliceClient, cursorOf("alice", 3))
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursorOf("alice", 3)), "alice's cursor moving on task A reaching bob on task B")
}

// Every reader who moves at the same moment on one process reaches the readers on the other
// within a second, not only the last of them: ygo's outbound lane keeps one awareness blob per
// room, latest only (internal/relaylane/lane.go), and each blob holds only the clients the
// change it came from touched, so a relay that sent what that lane handed it would drop the
// cursors it superseded until each client's next heartbeat - 15 s in a browser, and never for
// the readers here, which send none.
func TestEveryReaderMovingAtOnceOnOneProcessReachesAReaderOnTheOther(t *testing.T) {
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	const movers = 5
	readers := make([]*docstest.Peer, movers)
	for reader := range readers {
		readers[reader] = pair.openReader(t, pair.serverA, fmt.Sprintf("reader%d", reader))
	}
	bob := pair.openReader(t, pair.serverB, "bob")

	// Each reader announces its own client in the same instant, as a room of readers typing does.
	start := make(chan struct{})
	var moved sync.WaitGroup
	for reader, peer := range readers {
		moved.Add(1)
		go func() {
			defer moved.Done()
			<-start
			if err := peer.SetAwareness(movingClient(reader), cursorOf(fmt.Sprintf("reader%d", reader), reader)); err != nil {
				t.Errorf("announce reader %d's cursor: %v", reader, err)
			}
		}()
	}
	close(start)
	moved.Wait()

	held := func(states map[uint64]json.RawMessage) bool {
		for reader := range readers {
			if !holdsAwareness(movingClient(reader), cursorOf(fmt.Sprintf("reader%d", reader), reader))(states) {
				return false
			}
		}
		return true
	}
	if err := bob.WaitForAwareness(time.Second, held); err != nil {
		t.Fatalf("the cursors %d readers on task A set at once did not all reach bob on task B within a second: %v", movers, err)
	}
}

// movingClient is the awareness client of the reader'th reader in the test above, as a provider
// announces its tab's own.
func movingClient(reader int) uint64 { return 3_000 + uint64(reader) }

// A reader who opens the document second, on the other process, sees the first reader's cursor
// at once: the room that loads for it asks the other processes for their readers' states, and
// they answer. The test's readers send no heartbeat, so nothing else would bring it.
func TestAReaderOpeningSecondOnTheOtherProcessSeesTheFirstReadersAwareness(t *testing.T) {
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	setAwareness(t, alice, aliceClient, cursorOf("alice", 1))
	waitFor(t, roomWait(t), "task A's room to hold alice's cursor", func() bool {
		return roomHoldsClient(pair.taskA, pair.artifactID, aliceClient)
	})

	bob := pair.openReader(t, pair.serverB, "bob")
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursorOf("alice", 1)),
		"bob, opening second on task B, seeing the cursor alice set on task A")
}

// A reader who joins a room already resident on its process, and that loaded with no reader in
// it, asks for the other processes' readers' states as it joins: the load asked nobody.
func TestAReaderJoiningAResidentRoomOnTheOtherProcessSeesTheFirstReadersAwareness(t *testing.T) {
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	handledB := runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	setAwareness(t, alice, aliceClient, cursorOf("alice", 1))
	waitForAwarenessState(t, handledB, pair.artifactID, "alice's state reaching task B")
	if err := pair.taskB.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task B's room: %v", err)
	}
	if roomHoldsClient(pair.taskB, pair.artifactID, aliceClient) {
		t.Fatal("task B's room, loaded with no reader, already holds alice's cursor, so nothing here is tested")
	}

	bob := pair.openReader(t, pair.serverB, "bob")
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursorOf("alice", 1)),
		"bob, joining task B's resident room, seeing the cursor alice set on task A")
}

// A reader leaving on one process leaves on the other: ygo's removal of the departing peer's
// states is relayed like any other change.
func TestADisconnectOnOneProcessRemovesTheClientsAwarenessOnTheOther(t *testing.T) {
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	bob := pair.openReader(t, pair.serverB, "bob")
	setAwareness(t, alice, aliceClient, cursorOf("alice", 1))
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursorOf("alice", 1)), "alice's cursor on task A reaching bob on task B")

	alice.Close()
	requireAwareness(t, bob, lacksAwareness(aliceClient), "alice leaving task A removing her from bob's reader on task B")
}

// A reader leaving is relayed at the clock its awareness client stood at, whatever else the room
// is doing in that moment. ygo fires a room's update observers with the awareness unlocked, and
// every inbound apply and every other removal purges the room's tombstones as it goes
// (reclaimAwarenessSlots, a grace of one nanosecond), so a purge can free the departing client's
// entry between the removal's apply and the relay's encode of it. Read from the room's awareness
// then, the removal would name the client at a clock of 0, which task B drops as older than the
// state it holds, and alice's cursor would sit in bob's reader until it expired (awarenessExpiry,
// 30 s). Here the purge is put ahead of the relay's own observer, so it lands in that window
// every time.
func TestAReaderLeavingIsRelayedAtItsClientsClockThoughAPurgeRacesTheEncode(t *testing.T) {
	pair := newSeededProcessPair(t, "First.\n\nSecond.\n")
	var purged atomic.Int64
	pair.taskA.beforeAwarenessWatch = func(room string, aw *awareness.Awareness) {
		aw.OnUpdate(func(event awareness.UpdateEvent) {
			if len(event.Removed) == 0 {
				return
			}
			purged.Add(1)
			reclaimAwarenessSlots(room, aw)
		})
	}
	runAwareness(t, pair.taskA)
	handledB := runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	bob := pair.openReader(t, pair.serverB, "bob")
	setAwareness(t, alice, aliceClient, cursorOf("alice", 1))
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursorOf("alice", 1)), "alice's cursor on task A reaching bob on task B")
	clock := awarenessClockOf(t, pair.taskA, pair.artifactID, aliceClient)

	alice.Close()
	relayed := relayedRemovalClock(t, handledB, pair.artifactID, aliceClient)
	if purged.Load() == 0 {
		t.Fatal("no purge ran ahead of the relay's observer on task A's room, so the race this test is about never happened")
	}
	if relayed < clock {
		t.Errorf("alice's removal was relayed at clock %d, want at least the %d her client stood at: task B holds her at %d and drops a lower clock, so her cursor stays in its room until it expires",
			relayed, clock, clock)
	}
	requireAwareness(t, bob, lacksAwareness(aliceClient),
		"alice leaving task A, with a tombstone purge between the removal and the relay's encode of it, removing her from bob's reader on task B")
}

// A removal the relay sends stands at the clock the departing client held, raised to the clock
// the room still holds for that client (touched), which is right for the tombstone a departure
// leaves and wrong for a client that is in the room: ygo's Meta answers for a live client as
// readily as for a tombstone (awareness/awareness.go:361-375). A client can be both in one
// moment - removed at clock c by one path (a disconnecting tab whose frame echoed another
// reader's clients, an expiry) and announcing itself again at c+1 before the relay's observer
// reads the room. Raised to c+1, the removal is the clock of the state the client is live at:
// every other process takes it, since it is no older than what it holds, and drops the cursor
// of a reader who is in the room, while this process takes that client off the list it relays
// and answers queries from (local), so nothing but the reader's own next announcement brings it
// back. Its own entry, recorded by the announcement, is overwritten by the removal in the same
// batch.
//
// Here alice is announced again from an observer ahead of the relay's own, so her return lands
// in that window every time. Her removal must be dropped and her own entry must carry her
// state.
func TestAReaderInTheRoomAgainKeepsItsStateThoughARemovalRacesItsReturn(t *testing.T) {
	pair := newSeededProcessPair(t, "First.\n\nSecond.\n")
	returned := cursorOf("alice", 2)
	state, err := json.Marshal(returned)
	if err != nil {
		t.Fatalf("encode alice's next state: %v", err)
	}
	var announced atomic.Int64
	pair.taskA.beforeAwarenessWatch = func(room string, aw *awareness.Awareness) {
		aw.OnUpdate(func(event awareness.UpdateEvent) {
			if !slices.Contains(event.Removed, aliceClient) || announced.Add(1) > 1 {
				return
			}
			meta, known := aw.Meta(aliceClient)
			if !known {
				t.Errorf("task A's room holds nothing of alice's client to announce her again from")
				return
			}
			// Alice's next frame, at the clock after the one she was taken away at.
			if err := aw.ApplyUpdate(awarenessStates(aliceClient, 1, meta.Clock+1, string(state)), "alice's next frame"); err != nil {
				t.Errorf("announce alice again at clock %d: %v", meta.Clock+1, err)
			}
		})
	}
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	bob := pair.openReader(t, pair.serverB, "bob")
	setAwareness(t, alice, aliceClient, cursorOf("alice", 1))
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursorOf("alice", 1)), "alice's cursor on task A reaching bob on task B")
	clock := awarenessClockOf(t, pair.taskA, pair.artifactID, aliceClient)

	aw, resident := pair.taskA.srv.GetAwareness(pair.artifactID)
	if !resident {
		t.Fatal("task A's room is not resident, so nothing holds alice's client")
	}
	// The removal another of the room's paths makes while alice's own tab is still open, as a
	// disconnecting tab's echoed clients and an expiry do: applied to the room's awareness
	// directly, the way those paths reach it.
	if err := aw.ApplyUpdate(awarenessStates(aliceClient, 1, clock, "null"), "a removal of task A's own"); err != nil {
		t.Fatalf("remove alice's client at clock %d: %v", clock, err)
	}
	if announced.Load() == 0 {
		t.Fatal("alice was never announced again between her removal and the relay's observer, so the race this test is about never happened")
	}
	if !roomHoldsClient(pair.taskA, pair.artifactID, aliceClient) {
		t.Fatal("task A's room does not hold alice's client after her return, so nothing here is tested")
	}

	if relayed, listed := relayedClientClock(pair.taskA, pair.artifactID, aliceClient); !listed {
		t.Errorf("task A's relay no longer lists alice among the clients it relays, though she is in its room: its answer to another process's query leaves her out")
	} else if relayed <= clock {
		t.Errorf("task A's relay holds alice at clock %d, want the %d her return stands at", relayed, clock+1)
	}
	requireAwareness(t, bob, holdsAwareness(aliceClient, returned),
		"alice, removed on task A and in its room again before the relay's observer, keeping her cursor on bob's reader on task B")
}

// relayedClientClock is the clock service's relay holds for client among room's clients it
// relays to the other processes (awarenessRelay.local), and whether it lists the client at all.
func relayedClientClock(service *Service, room string, client uint64) (uint64, bool) {
	relay := service.awarenessRelay
	relay.mu.Lock()
	defer relay.mu.Unlock()
	clock, listed := relay.local[room][client]
	return clock, listed
}

// awarenessClockOf is the clock service's resident room of artifactID holds client's state at.
func awarenessClockOf(t *testing.T, service *Service, artifactID string, client uint64) uint64 {
	t.Helper()
	aw, resident := service.srv.GetAwareness(artifactID)
	if !resident {
		t.Fatalf("no resident room of %s to read client %d's clock from", artifactID, client)
	}
	state, held := aw.GetStates()[client]
	if !held {
		t.Fatalf("the resident room of %s holds no state of client %d", artifactID, client)
	}
	return state.Clock
}

// relayedRemovalClock is the clock of the removal of client that reaches handled for artifactID,
// the first one relayed within roomWait.
func relayedRemovalClock(t *testing.T, handled <-chan awarenessMessage, artifactID string, client uint64) uint64 {
	t.Helper()
	deadline := time.After(roomWait(t))
	for {
		select {
		case message := <-handled:
			if message.Room != artifactID || message.Query {
				continue
			}
			update, err := base64.StdEncoding.DecodeString(message.Data)
			if err != nil {
				t.Fatalf("decode a relayed awareness update: %v", err)
			}
			entries, err := decodeAwarenessEntries(update)
			if err != nil {
				t.Fatalf("read a relayed awareness update: %v", err)
			}
			for _, entry := range entries {
				if entry.client == client && entry.removed {
					return entry.clock
				}
			}
		case <-deadline:
			t.Fatalf("no removal of client %d was relayed for %s within %v", client, artifactID, roomWait(t))
		}
	}
}

// A room this process closes itself - here a forced close, as a replaced history, a closed issue
// or a failed room's eviction makes - removes its readers' states on the other processes too,
// though the close disconnects them and tears the room's relay down together.
func TestARoomThisProcessClosesRemovesItsReadersAwarenessOnTheOther(t *testing.T) {
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	bob := pair.openReader(t, pair.serverB, "bob")
	setAwareness(t, alice, aliceClient, cursorOf("alice", 1))
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursorOf("alice", 1)), "alice's cursor on task A reaching bob on task B")

	if err := pair.taskA.srv.CloseRoom(pair.artifactID, true); err != nil {
		t.Fatalf("close task A's room: %v", err)
	}
	select {
	case <-alice.Ended:
	case <-time.After(roomWait(t)):
		t.Fatal("alice's socket outlived the close of her room")
	}
	requireAwareness(t, bob, lacksAwareness(aliceClient), "task A closing alice's room removing her from bob's reader on task B")
}

// waitForAwarenessState waits for handled to note an awareness state update (not a query) for
// artifactID, which the relay has handled by then.
func waitForAwarenessState(t *testing.T, handled <-chan awarenessMessage, artifactID, what string) {
	t.Helper()
	deadline := time.After(roomWait(t))
	for {
		select {
		case message := <-handled:
			if message.Room == artifactID && !message.Query {
				return
			}
		case <-deadline:
			t.Fatalf("%s: no awareness state for %s handled within %v", what, artifactID, roomWait(t))
		}
	}
}

// Another process's awareness is applied only to a room this process holds with a reader of its
// own in it: it does not load the document on a process no reader of it is on, and it does not
// reach - nor restamp the idle clock of - a room resident here with nobody reading it.
func TestRelayedAwarenessNeitherLoadsNorKeepsARoomWithNoReaderOfItsOwn(t *testing.T) {
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	handledB := runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")

	setAwareness(t, alice, aliceClient, cursorOf("alice", 1))
	waitForAwarenessState(t, handledB, pair.artifactID, "alice's state reaching task B, which holds no room")
	if slices.Contains(pair.taskB.srv.Rooms(), pair.artifactID) {
		t.Fatal("alice's awareness loaded a room on task B, where nobody reads the document")
	}

	if err := pair.taskB.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task B's room: %v", err)
	}
	setAwareness(t, alice, aliceClient, cursorOf("alice", 2))
	waitForAwarenessState(t, handledB, pair.artifactID, "alice's next state reaching task B, which holds a room with no reader")
	if roomHoldsClient(pair.taskB, pair.artifactID, aliceClient) {
		t.Fatal("alice's awareness reached task B's room, which no reader of task B is in: relayed heartbeats would keep it resident")
	}
}

// A client state too large for one notification is dropped and counted rather than sent, and
// what the room relays after it still crosses.
func TestAnAwarenessStateTooLargeToRelayIsDroppedAndCounted(t *testing.T) {
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	bob := pair.openReader(t, pair.serverB, "bob")

	oversized := map[string]any{"user": map[string]any{"name": strings.Repeat("a", roomSyncPayloadLimit)}}
	setAwareness(t, alice, aliceClient, oversized)
	waitFor(t, roomWait(t), "task A's relay to count the oversized state", func() bool {
		return pair.taskA.awarenessRelay.oversized.Load() == 1
	})
	const otherClient = aliceClient + 1
	setAwareness(t, alice, otherClient, cursorOf("alice", 1))
	requireAwareness(t, bob, holdsAwareness(otherClient, cursorOf("alice", 1)), "the small state after the oversized one reaching bob on task B")
	if _, present := bob.AwarenessStates()[aliceClient]; present {
		t.Fatal("the oversized state reached bob on task B")
	}
	if dropped := pair.taskA.awarenessRelay.oversized.Load(); dropped != 1 {
		t.Fatalf("task A's relay counted %d oversized states, want 1", dropped)
	}
}

// awarenessStates is one y-protocols awareness update announcing count clients from first, each at
// clock with state, as one peer's frame carries them: the state of a cursor, or "null" to take
// those clients away as the disconnect of the peer that sent them does. It is the one encoder the
// tests of this package announce awareness clients with.
func awarenessStates(first, count, clock uint64, state string) []byte {
	return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(count)
		for client := first; client < first+count; client++ {
			encoder.WriteVarUint(client)
			encoder.WriteVarUint(clock)
			encoder.WriteVarString(state)
		}
	})
}

// floodState is the state each client of a flood announces below.
const floodState = `{"user":{"name":"flood"}}`

// relayedClients is how many of room's clients service's relay is sending to the other processes.
func relayedClients(service *Service, room string) int {
	relay := service.awarenessRelay
	relay.mu.Lock()
	defer relay.mu.Unlock()
	return len(relay.local[room])
}

// One frame inventing far more awareness clients than any room has readers costs the room, and
// the other processes, the bound and no more: the room takes the bound's clients of the frame
// and refuses the rest (maxAwarenessClientsPerRoom), and the relay sends those in the few
// notifications they fill, not one per client. What keeps such a frame from reaching a room at
// all is the bound each socket is held to before ygo applies it (awareness_limit.go); this one
// is applied to the room's awareness directly, as ygo's own expiry and removal paths do.
func TestAnAwarenessFrameOverTheClientBoundRelaysNoMoreThanTheBound(t *testing.T) {
	pair := newProcessPair(t)
	handledB := runAwareness(t, pair.taskB)
	if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task A's room: %v", err)
	}
	aw, resident := pair.taskA.srv.GetAwareness(pair.artifactID)
	if !resident {
		t.Fatal("task A's room is not resident, so it has no awareness to flood")
	}

	const flood = 8 * maxAwarenessClientsPerRoom
	if err := aw.ApplyUpdate(awarenessStates(1, flood, 1, floodState), "a peer of task A"); err != nil {
		t.Fatalf("apply a frame of %d awareness clients: %v", flood, err)
	}
	waitFor(t, roomWait(t), "task A's relay to be sending the bound's clients of the frame", func() bool {
		return relayedClients(pair.taskA, pair.artifactID) == maxAwarenessClientsPerRoom
	})
	tracked, err := pair.taskA.heldAwarenessClients(pair.artifactID)
	if err != nil {
		t.Fatalf("read the awareness task A's room holds: %v", err)
	}
	if tracked != maxAwarenessClientsPerRoom {
		t.Fatalf("task A's room tracks %d of the frame's %d clients, want the bound's %d", tracked, flood, maxAwarenessClientsPerRoom)
	}

	deadline := time.After(roomWait(t))
	sends, relayed := 0, 0
	for relayed < maxAwarenessClientsPerRoom {
		select {
		case message := <-handledB:
			if message.Room != pair.artifactID || message.Query {
				continue
			}
			sends++
			update, err := base64.StdEncoding.DecodeString(message.Data)
			if err != nil {
				t.Fatalf("decode a relayed awareness update: %v", err)
			}
			entries, err := decodeAwarenessEntries(update)
			if err != nil {
				t.Fatalf("read a relayed awareness update: %v", err)
			}
			relayed += len(entries)
		case <-deadline:
			t.Fatalf("task B heard %d awareness clients in %d notifications within %v, want the bound's %d",
				relayed, sends, roomWait(t), maxAwarenessClientsPerRoom)
		}
	}
	if relayed > maxAwarenessClientsPerRoom || sends > maxAwarenessClientsPerRoom {
		t.Fatalf("a frame of %d clients relayed %d of them in %d notifications, past the %d-client bound",
			flood, relayed, sends, maxAwarenessClientsPerRoom)
	}
	t.Logf("a frame of %d awareness clients relayed %d of them in %d notifications", flood, relayed, sends)
}

// The clients of a flood that leaves give their entries back as they go: a room's bound counts
// the clients it holds now, not every client id it has ever heard of, so a cursor announced just
// after a frame of thousands has gone is taken by the room and relayed at once rather than
// refused until the frame's clients have expired and been purged.
func TestAwarenessClientsThatLeaveFreeTheRoomsBoundAtOnce(t *testing.T) {
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	bob := pair.openReader(t, pair.serverB, "bob")
	aw, resident := pair.taskA.srv.GetAwareness(pair.artifactID)
	if !resident {
		t.Fatal("task A's room is not resident, so it has no awareness to flood")
	}

	const flood = 8 * maxAwarenessClientsPerRoom
	if err := aw.ApplyUpdate(awarenessStates(1, flood, 1, floodState), "a peer of task A"); err != nil {
		t.Fatalf("apply a frame of %d awareness clients: %v", flood, err)
	}
	waitFor(t, roomWait(t), "task A's relay to be sending the bound's clients of the frame", func() bool {
		return relayedClients(pair.taskA, pair.artifactID) == maxAwarenessClientsPerRoom
	})
	if err := aw.ApplyUpdate(awarenessStates(1, flood, 1, "null"), "a peer of task A"); err != nil {
		t.Fatalf("take the frame's %d awareness clients away: %v", flood, err)
	}
	waitFor(t, roomWait(t), "task A's relay to be sending none of the frame's clients once they have left", func() bool {
		return relayedClients(pair.taskA, pair.artifactID) == 0
	})

	// A client id of the joining reader's own: the frame claims 1 through flood, and a client the
	// frame has left a tombstone of stands at a clock of its own.
	const joined = flood + aliceClient
	setAwareness(t, alice, joined, cursorOf("alice", 1))
	requireAwareness(t, bob, holdsAwareness(joined, cursorOf("alice", 1)),
		fmt.Sprintf("alice's cursor, announced on task A just after a frame of %d clients had come and gone, reaching bob on task B", flood))
}

// A batch too large for one notification is split across several, each within the limit and
// holding whole clients, and a client too large for any is dropped and counted.
func TestAnAwarenessBatchIsSplitAcrossNotificationsWithinTheLimit(t *testing.T) {
	relay := newAwarenessRelay(&Service{processID: "00000000-0000-0000-0000-000000000001"})
	room := "00000000-0000-0000-0000-000000000002"
	batch := make(map[uint64][]byte)
	for client := uint64(1); client <= 5; client++ {
		state := `{"name":"` + strings.Repeat("x", 2_500) + `"}`
		entries, err := decodeAwarenessEntries(awarenessStates(client, 1, client, state))
		if err != nil {
			t.Fatal(err)
		}
		batch[client] = entries[0].raw
	}
	huge, err := decodeAwarenessEntries(awarenessStates(99, 1, 1, `{"name":"`+strings.Repeat("y", roomSyncPayloadLimit)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	batch[99] = huge[0].raw

	payloads := relay.awarenessNotifications(room, batch)
	if len(payloads) < 2 {
		t.Fatalf("five clients of about 2.5 kB each went out in %d notifications, want them split", len(payloads))
	}
	var sent []uint64
	for _, payload := range payloads {
		if len(payload) > roomSyncPayloadLimit {
			t.Errorf("a notification of %d bytes passes the %d-byte limit", len(payload), roomSyncPayloadLimit)
		}
		var message awarenessMessage
		if err := json.Unmarshal([]byte(payload), &message); err != nil {
			t.Fatal(err)
		}
		if message.Room != room || message.Sender != relay.service.processID {
			t.Fatalf("a notification names room %q and sender %q", message.Room, message.Sender)
		}
		update, err := base64.StdEncoding.DecodeString(message.Data)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := decodeAwarenessEntries(update)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			sent = append(sent, entry.client)
		}
	}
	if want := []uint64{1, 2, 3, 4, 5}; !slices.Equal(sent, want) {
		t.Fatalf("the notifications carry clients %v, want %v, each once", sent, want)
	}
	if dropped := relay.oversized.Load(); dropped != 1 {
		t.Fatalf("the relay counted %d oversized states, want 1", dropped)
	}
}

// Every document socket's upgrade names the process serving it.
func TestTheDocumentWebsocketUpgradeNamesItsProcess(t *testing.T) {
	pair := newProcessPair(t)
	reader := pair.openReader(t, pair.serverA, "alice")
	if pair.taskA.hostname == "" {
		t.Fatal("the service read no host name")
	}
	if served := reader.UpgradeHeader.Get(ProcessHeader); served != pair.taskA.hostname {
		t.Fatalf("the upgrade names %q as its process, want %q; header %v", served, pair.taskA.hostname, reader.UpgradeHeader)
	}
}
