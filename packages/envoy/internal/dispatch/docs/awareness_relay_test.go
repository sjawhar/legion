package docs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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

// awarenessFlood is one y-protocols awareness update announcing clients 1 through clients, each
// with a cursor-sized state, as one peer's frame can.
func awarenessFlood(clients uint64) []byte {
	return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(clients)
		for client := uint64(1); client <= clients; client++ {
			encoder.WriteVarUint(client)
			encoder.WriteVarUint(1)
			encoder.WriteVarString(`{"user":{"name":"flood"}}`)
		}
	})
}

// awarenessFloodRemoval takes away the clients awarenessFlood announced, as the disconnect of the
// peer that sent them does: a null state at the clock they stand at.
func awarenessFloodRemoval(clients uint64) []byte {
	return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(clients)
		for client := uint64(1); client <= clients; client++ {
			encoder.WriteVarUint(client)
			encoder.WriteVarUint(1)
			encoder.WriteVarString("null")
		}
	})
}

// relayedClients is how many of room's clients service's relay is sending to the other processes.
func relayedClients(service *Service, room string) int {
	relay := service.awarenessRelay
	relay.mu.Lock()
	defer relay.mu.Unlock()
	return len(relay.local[room])
}

// A room whose awareness holds far more clients than any room has readers costs the other
// processes the notifications those clients fill and no more: the relay packs whole clients into
// each notification within its payload limit, so a frame of thousands is a handful of pg_notify
// sends and not one per client. What keeps such a frame from reaching a room at all is the bound
// each socket is held to before ygo applies it (awareness_limit.go); this one is applied to the
// room's awareness directly, as ygo's own expiry and removal paths do.
func TestAnAwarenessFrameOfManyClientsRelaysThemInFewNotifications(t *testing.T) {
	pair := newProcessPair(t)
	handledB := runAwareness(t, pair.taskB)
	if err := pair.taskA.warmLiveDocument(context.Background(), pair.artifactID); err != nil {
		t.Fatalf("load task A's room: %v", err)
	}
	aw, resident := pair.taskA.srv.GetAwareness(pair.artifactID)
	if !resident {
		t.Fatal("task A's room is not resident, so it has no awareness to flood")
	}

	const flood = 2_048
	if err := aw.ApplyUpdate(awarenessFlood(flood), "a peer of task A"); err != nil {
		t.Fatalf("apply a frame of %d awareness clients: %v", flood, err)
	}
	waitFor(t, roomWait(t), "task A's relay to be sending the frame's clients", func() bool {
		return relayedClients(pair.taskA, pair.artifactID) == flood
	})
	if tracked := trackedAwarenessClients(t, pair.taskA, pair.artifactID); tracked != flood {
		t.Fatalf("task A's room tracks %d of the frame's %d clients, want all of them", tracked, flood)
	}

	deadline := time.After(roomWait(t))
	sends, relayed := 0, 0
	for relayed < flood {
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
			t.Fatalf("task B heard %d awareness clients in %d notifications within %v, want the frame's %d",
				relayed, sends, roomWait(t), flood)
		}
	}
	// Each notification holds whole clients within roomSyncPayloadLimit, so the frame's clients
	// fill a dozen or so; one per client would be thousands.
	if sends >= flood/16 {
		t.Fatalf("a frame of %d clients relayed %d of them in %d notifications, want far fewer notifications than clients",
			flood, relayed, sends)
	}
	t.Logf("a frame of %d awareness clients relayed %d of them in %d notifications", flood, relayed, sends)
}

// The clients of a flood that leaves give their entries back as they go, rather than sitting in
// the room for two expiries: a room tracks the clients it holds now, not every client id it has
// ever heard of, and a cursor announced just after a frame of thousands has gone is relayed at
// once.
func TestAwarenessClientsThatLeaveFreeTheirEntriesAtOnce(t *testing.T) {
	pair := newProcessPair(t)
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	bob := pair.openReader(t, pair.serverB, "bob")
	aw, resident := pair.taskA.srv.GetAwareness(pair.artifactID)
	if !resident {
		t.Fatal("task A's room is not resident, so it has no awareness to flood")
	}

	const flood = 2_048
	if err := aw.ApplyUpdate(awarenessFlood(flood), "a peer of task A"); err != nil {
		t.Fatalf("apply a frame of %d awareness clients: %v", flood, err)
	}
	waitFor(t, roomWait(t), "task A's relay to be sending the frame's clients", func() bool {
		return relayedClients(pair.taskA, pair.artifactID) == flood
	})
	if err := aw.ApplyUpdate(awarenessFloodRemoval(flood), "a peer of task A"); err != nil {
		t.Fatalf("take the frame's %d awareness clients away: %v", flood, err)
	}
	waitFor(t, roomWait(t), "task A's relay to be sending none of the frame's clients once they have left", func() bool {
		return relayedClients(pair.taskA, pair.artifactID) == 0
	})

	// A client id of the joining reader's own: awarenessFlood claims 1 through flood, and a
	// client the frame has left a tombstone of stands at a clock of its own.
	const joined = flood + aliceClient
	setAwareness(t, alice, joined, cursorOf("alice", 1))
	requireAwareness(t, bob, holdsAwareness(joined, cursorOf("alice", 1)),
		"alice's cursor, announced on task A just after a frame of 2,048 clients had come and gone, reaching bob on task B")
}

// awarenessEntryUpdate is a one-client awareness update, as y-protocols encodes one.
func awarenessEntryUpdate(client, clock uint64, state string) []byte {
	return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(1)
		encoder.WriteVarUint(client)
		encoder.WriteVarUint(clock)
		encoder.WriteVarString(state)
	})
}

// A batch too large for one notification is split across several, each within the limit and
// holding whole clients, and a client too large for any is dropped and counted.
func TestAnAwarenessBatchIsSplitAcrossNotificationsWithinTheLimit(t *testing.T) {
	relay := newAwarenessRelay(&Service{processID: "00000000-0000-0000-0000-000000000001"})
	room := "00000000-0000-0000-0000-000000000002"
	batch := make(map[uint64][]byte)
	for client := uint64(1); client <= 5; client++ {
		state := `{"name":"` + strings.Repeat("x", 2_500) + `"}`
		entries, err := decodeAwarenessEntries(awarenessEntryUpdate(client, client, state))
		if err != nil {
			t.Fatal(err)
		}
		batch[client] = entries[0].raw
	}
	huge, err := decodeAwarenessEntries(awarenessEntryUpdate(99, 1, `{"name":"`+strings.Repeat("y", roomSyncPayloadLimit)+`"}`))
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
