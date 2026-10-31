package docs

// The races the awareness relay settles, each driven from a hook the service keeps for them
// (beforeAwarenessWatch, beforeAwarenessRecord, beforeAwarenessAnswerRecord): a reader back in
// the room before its removal is sent, a removal recorded over the read an answer to another
// process's query stands on, a removal and that client's own heartbeat recorded in either
// order, and the one lock order the relay's record and a room's observers share. What crosses
// between two processes when nothing races is in awareness_relay_test.go, whose readers,
// conditions and process pair the tests here are built from.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/awareness"
)

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
// the room's own snapshot of that client stands at (touched) and settled against the room once
// more as it is recorded (record). That is right for the tombstone a departure leaves and wrong
// for a client that is in the room: what ygo holds of a live client is its state, at the clock
// it is live at. A client can be both in one moment - removed at clock c by one path (a
// disconnecting tab whose frame echoed another reader's clients, an expiry) and announcing
// itself again at c+1 before the relay has sent the removal. Sent at c+1, the removal stands at
// the clock of the state the client is live at: every other process takes it, since it is no
// older than what it holds, and drops the cursor of a reader who is in the room, while this
// process takes that client off the list it relays and answers queries from (local), so nothing
// but the reader's own next announcement brings it back. Its own entry, recorded by the
// announcement, is overwritten by the removal in the same batch.
//
// Each test below puts alice's return in one of the windows between her removal's apply and its
// send, and aReturnRacingARemoval is their one body: her removal must be dropped and her own
// entry must carry her state.

// returnRace is the wiring of one such test: where alice's return is put, and what the test
// holds open across it.
type returnRace struct {
	// wire installs the hook that puts alice's return in the test's window, on task A, before
	// its room sync runs. back announces her again - once, at the clock after the one she was
	// taken away at - and the body fails the test when nothing ever called it.
	wire func(service *Service, back func(aw *awareness.Awareness))
	// ready runs once alice's cursor has reached both processes and before her removal is
	// applied: a read a test holds open across the removal starts here. Nil when the test needs
	// nothing there.
	ready func(t *testing.T, pair processPair)
	// window is the window alice's return lands in, as the test's failures name it.
	window string
}

// aReturnRacingARemoval runs race: alice announces a cursor on task A, which reaches bob on task
// B; a path of task A's own removes her client at the clock it stands at; and her own next frame
// lands in the window race names. Her removal must reach neither process: task A's relay must
// still list her, at the clock of her return, and bob must still hold her cursor.
func aReturnRacingARemoval(t *testing.T, race returnRace) {
	t.Helper()
	pair := newSeededProcessPair(t, "First.\n\nSecond.\n")
	returned := cursorOf("alice", 2)
	state, err := json.Marshal(returned)
	if err != nil {
		t.Fatalf("encode alice's next state: %v", err)
	}
	var announced atomic.Int64
	race.wire(pair.taskA, func(aw *awareness.Awareness) {
		if announced.Add(1) > 1 {
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
	if race.ready != nil {
		race.ready(t, pair)
	}

	// The removal another of the room's paths makes while alice's own tab is still open, as a
	// disconnecting tab's echoed clients and an expiry do: applied to the room's awareness
	// directly, the way those paths reach it.
	if err := aw.ApplyUpdate(awarenessStates(aliceClient, 1, clock, "null"), "a removal of task A's own"); err != nil {
		t.Fatalf("remove alice's client at clock %d: %v", clock, err)
	}
	if announced.Load() == 0 {
		t.Fatalf("alice was never announced again %s, so the race this test is about never happened", race.window)
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
		fmt.Sprintf("alice, back in task A's room %s, keeping her cursor on bob's reader on task B", race.window))
}

// removesAlice reports whether entries carry a removal of alice's awareness client.
func removesAlice(entries []awarenessEntry) bool {
	return slices.ContainsFunc(entries, func(entry awarenessEntry) bool {
		return entry.client == aliceClient && entry.removed
	})
}

// announcesAlice reports whether entries carry a state of alice's awareness client at clock or
// later; a clock of 0 takes any of them.
func announcesAlice(entries []awarenessEntry, clock uint64) bool {
	return slices.ContainsFunc(entries, func(entry awarenessEntry) bool {
		return entry.client == aliceClient && !entry.removed && entry.clock >= clock
	})
}

// Alice is announced again from an observer ahead of the relay's own, so her return lands
// between her removal's apply and the relay hearing of it every time.
func TestAReaderInTheRoomAgainKeepsItsStateThoughARemovalRacesItsReturn(t *testing.T) {
	aReturnRacingARemoval(t, returnRace{
		window: "before the relay's own observer heard of her removal",
		wire: func(service *Service, back func(aw *awareness.Awareness)) {
			service.beforeAwarenessWatch = func(room string, aw *awareness.Awareness) {
				aw.OnUpdate(func(event awareness.UpdateEvent) {
					if slices.Contains(event.Removed, aliceClient) {
						back(aw)
					}
				})
			}
		},
	})
}

// A removal the relay built is settled against the room again as it is recorded (record): what
// the observer decided and what the relay records are two moments, and a return of that client
// recorded in between is the later state. Recorded over it, the removal takes the client off the
// list this process relays and answers queries from and replaces the client's own entry in the
// batch, so every other process drops the cursor of a reader who is in the room.
//
// Here alice is announced again from the window between the relay's observer deciding her
// removal and recording it, so her return - and the record of it, which the announcement runs
// through - lands in it every time.
func TestAReaderBackBeforeItsRemovalIsRecordedKeepsItsState(t *testing.T) {
	aReturnRacingARemoval(t, returnRace{
		window: "between her removal being decided and being recorded",
		wire: func(service *Service, back func(aw *awareness.Awareness)) {
			service.beforeAwarenessRecord = func(room string, aw *awareness.Awareness, entries []awarenessEntry) {
				if removesAlice(entries) {
					back(aw)
				}
			}
		},
	})
}

// The answer to another process's query reads the states of this process's clients and records
// them, and those are two moments too (answer): a client the room moved on from in between is
// read at the clock it stood at then. Recorded as that clock, it would lower the clock the relay
// holds for a client that is in the room at a later one - and then the removal that took her
// away at the older clock, recorded after it, is no longer older than the list and is taken for
// that state's own removal. Every other process accepts a null at the clock it holds a state at
// (awareness/awareness.go:566-581), so the cursor of a reader who is in the room goes from all
// of them.
//
// Here task A answers a query while alice is live at c, and the answer's record is held until
// her return at c+1 and its record have run, with her removal at c recorded after both: the
// clock the relay holds for her must stay c+1, so the removal is settled against the room and
// dropped.
func TestAReaderBackWhileAnOlderStateOfHersIsAnsweredToAQueryKeepsItsState(t *testing.T) {
	// The bound every wait below runs on, taken here: roomWait reads the binary's deadline
	// through t, and the hooks run on the goroutines of the room's own changes.
	wait := roomWait(t)
	var answering, holding, settling atomic.Bool
	read := make(chan struct{})     // the answer has read alice at the clock she stood at
	record := make(chan struct{})   // the answer may record what it read
	recorded := make(chan struct{}) // the answer has recorded it
	aReturnRacingARemoval(t, returnRace{
		window: "with an answer to another process's query recorded over her older state before her removal",
		wire: func(service *Service, back func(aw *awareness.Awareness)) {
			service.beforeAwarenessAnswerRecord = func(room string, aw *awareness.Awareness, entries []awarenessEntry) {
				if !answering.Load() || !announcesAlice(entries, 0) || !holding.CompareAndSwap(false, true) {
					return
				}
				close(read)
				<-record
			}
			service.beforeAwarenessRecord = func(room string, aw *awareness.Awareness, entries []awarenessEntry) {
				// Her own socket's close removes her client again as the test ends; only the
				// removal this test is about is held.
				if !removesAlice(entries) || !settling.CompareAndSwap(false, true) {
					return
				}
				// Alice's return, recorded first, and then the answer's record of the state
				// she stood at before it. Her removal's own record follows both.
				back(aw)
				close(record)
				select {
				case <-recorded:
				case <-time.After(wait):
					t.Error("the answer to another process's query never recorded what it read")
				}
			}
		},
		ready: func(t *testing.T, pair processPair) {
			answering.Store(true)
			go func() {
				defer close(recorded)
				// The query another process sends when a reader of its own opens the document
				// (query, received), answered from here while alice is still live at c.
				pair.taskA.awarenessRelay.answer(pair.artifactID)
			}()
			select {
			case <-read:
			case <-time.After(wait):
				t.Fatal("the answer to another process's query never read alice's state")
			}
		},
	})
}

// The answer to another process's query is a read of the clients the relay's list held when it
// read them, and a removal recorded between that read and its record moves the list past it:
// alice is off the list, and her removal, at the clock she stood at, is in the room's pending
// batch. Recorded as it stands, the answer's older state would put her back on the list this
// process relays and answers queries from and replace her removal in the batch - and every
// other process drops a state at the clock it already holds
// (awareness/awareness.go:566-581), so the removal would reach none of them and alice's cursor
// would sit in their rooms until it expired (awarenessExpiry, 30 s). Flushed before the answer
// records, the removal does reach them - and the stale state is no safer behind it: every
// process purges its room's tombstones as it applies what it is sent (received,
// reclaimAwarenessSlots, a grace of one nanosecond), so her state at the older clock reaches a
// room that holds nothing of her at all and is taken for a reader who has just joined, which
// puts the cursor of a reader who is gone back until it expires. The list, too, would be left
// holding her - the reader the next query would be answered with.
//
// anAnswerRecordedOverARemoval runs both: alice announces a cursor on task A, which reaches bob
// on task B; an answer to another process's query reads her at the clock she stands at and is
// held there; her removal is recorded, and sent before the answer records when flushed is true;
// and the answer then records what it read. Her removal must stand: task A's relay must no
// longer list her, and bob must lose her cursor well before it expires.
func anAnswerRecordedOverARemoval(t *testing.T, flushed bool) {
	t.Helper()
	pair := newSeededProcessPair(t, "First.\n\nSecond.\n")
	cursor := cursorOf("alice", 1)
	// The bound the hook's wait runs on, taken here: roomWait reads the binary's deadline
	// through t, and the hook runs on the goroutine that answers.
	wait := roomWait(t)
	var answering, holding atomic.Bool
	read := make(chan struct{})     // the answer has read alice at the clock she stands at
	record := make(chan struct{})   // her removal is recorded, so the answer may record its read
	recorded := make(chan struct{}) // the answer has recorded it
	pair.taskA.beforeAwarenessAnswerRecord = func(room string, aw *awareness.Awareness, entries []awarenessEntry) {
		// Task B's own query is answered as bob opens the document; only the answer this test
		// drives is held.
		if !answering.Load() || !announcesAlice(entries, 0) || !holding.CompareAndSwap(false, true) {
			return
		}
		close(read)
		select {
		case <-record:
		case <-time.After(wait):
			t.Error("alice's removal was never recorded while the answer's read was held")
		}
	}
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	bob := pair.openReader(t, pair.serverB, "bob")
	setAwareness(t, alice, aliceClient, cursor)
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursor), "alice's cursor on task A reaching bob on task B")
	clock := awarenessClockOf(t, pair.taskA, pair.artifactID, aliceClient)
	aw, resident := pair.taskA.srv.GetAwareness(pair.artifactID)
	if !resident {
		t.Fatal("task A's room is not resident, so nothing holds alice's client")
	}

	// The query another process sends when a reader of its own opens the document (query,
	// received), answered from here while alice is still live at the clock she announced.
	answering.Store(true)
	go func() {
		defer close(recorded)
		pair.taskA.awarenessRelay.answer(pair.artifactID)
	}()
	select {
	case <-read:
	case <-time.After(wait):
		t.Fatal("the answer to another process's query never read alice's state")
	}

	// Alice leaves, at the clock she stands at: the room's own removal, which is recorded while
	// the answer's read waits.
	if err := aw.ApplyUpdate(awarenessStates(aliceClient, 1, clock, "null"), "a removal of task A's own"); err != nil {
		t.Fatalf("remove alice's client at clock %d: %v", clock, err)
	}
	if roomHoldsClient(pair.taskA, pair.artifactID, aliceClient) {
		t.Fatal("task A's room still holds alice's client after her removal, so nothing here is tested")
	}
	if flushed {
		requireAwareness(t, bob, lacksAwareness(aliceClient),
			"alice leaving task A reaching bob on task B before the answer records the older state it read of her")
	}
	close(record)
	<-recorded

	if relayed, listed := relayedClientClock(pair.taskA, pair.artifactID, aliceClient); listed {
		t.Errorf("task A's relay lists alice at clock %d among the clients it relays, though she left its room: its next answer to another process's query puts her cursor back",
			relayed)
	}
	requireAwareness(t, bob, lacksAwareness(aliceClient),
		"alice leaving task A, her removal recorded while an answer to another process's query held an older state of her, removing her from bob's reader on task B")
}

// The answer records the state it read after the removal, and before the batch carrying the
// removal is sent: the removal is what that batch must carry.
func TestAReaderLeavingWhileAnOlderStateOfHersIsAnsweredToAQueryStillLeaves(t *testing.T) {
	anAnswerRecordedOverARemoval(t, false)
}

// The same with her removal already sent, which the other processes have taken and purged her
// tombstone behind (received, reclaimAwarenessSlots): the stale state the answer records then
// would reach rooms that hold nothing of her and be taken for a reader who has just joined, so
// it must not be sent at all, and task A's own list must be left without a reader who is gone.
func TestAReaderLeavingBeforeAnOlderStateOfHersIsAnsweredToAQueryIsNotRelisted(t *testing.T) {
	anAnswerRecordedOverARemoval(t, true)
}

// A removal the relay recorded late still reaches the other processes, at the clock the relay
// holds for its client. Deciding a removal and recording it are two moments, and a heartbeat of
// the same client - its own entry, one clock later - can be recorded in between. The removal is
// then older than the clock the list holds, which is what a client that came back looks like,
// and the clocks alone do not tell the two apart. Dropped as a return, the departure is never
// sent at all: the heartbeat left a nil in the batch, and the flush reads a room that holds
// nothing of the client and drops it (resolve), so the departed reader's cursor sits in the
// other processes' rooms until it expires (awarenessExpiry, 30 s). Settled against the room as
// it is recorded, the removal is sent at the clock the list holds, which every process takes.
//
// A purge is what leaves the removal older than the heartbeat: it frees the departing client's
// tombstone before the relay's snapshot reads it (reclaimAwarenessSlots runs after every inbound
// apply and after every other removal of the room's own), so the snapshot finds nothing and the
// removal stands at the clock the relay last stood the client at - the one before the heartbeat.
// Here the purge is put ahead of the relay's own observer, so it lands in that window every time.
func TestAReaderLeavingOverItsOwnHeartbeatStillLeavesTheOtherProcess(t *testing.T) {
	pair := newSeededProcessPair(t, "First.\n\nSecond.\n")
	cursor := cursorOf("alice", 1)
	state, err := json.Marshal(cursor)
	if err != nil {
		t.Fatalf("encode alice's state: %v", err)
	}
	// The bound the hooks' waits run on, taken here: roomWait reads the binary's deadline
	// through t, and the hooks run on the goroutines of the room's own changes.
	wait := roomWait(t)
	var purged atomic.Int64
	var leaving, beating atomic.Bool
	var heartbeatClock atomic.Uint64
	held := make(chan struct{})     // the heartbeat has been decided and its record waits
	decided := make(chan struct{})  // the removal has been decided, at the clock before it
	recorded := make(chan struct{}) // the heartbeat is on the relay's list, at the later clock
	pair.taskA.beforeAwarenessWatch = func(room string, aw *awareness.Awareness) {
		aw.OnUpdate(func(event awareness.UpdateEvent) {
			if !slices.Contains(event.Removed, aliceClient) {
				return
			}
			purged.Add(1)
			reclaimAwarenessSlots(room, aw)
		})
	}
	// Her own socket's close removes her client again as the test ends, and a frame of the room
	// can name her once more; only the heartbeat and the removal this test is about are held.
	pair.taskA.beforeAwarenessRecord = func(room string, aw *awareness.Awareness, entries []awarenessEntry) {
		switch beat := heartbeatClock.Load(); {
		case removesAlice(entries) && leaving.CompareAndSwap(false, true):
			close(decided)
			select {
			case <-recorded:
			case <-time.After(wait):
				t.Error("alice's heartbeat was never recorded, so her removal was never recorded late")
			}
		case beat != 0 && announcesAlice(entries, beat) && beating.CompareAndSwap(false, true):
			close(held)
			select {
			case <-decided:
			case <-time.After(wait):
				t.Error("alice's removal was never decided while her heartbeat's record waited")
			}
		}
	}
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	bob := pair.openReader(t, pair.serverB, "bob")
	setAwareness(t, alice, aliceClient, cursor)
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursor), "alice's cursor on task A reaching bob on task B")
	clock := awarenessClockOf(t, pair.taskA, pair.artifactID, aliceClient)
	aw, resident := pair.taskA.srv.GetAwareness(pair.artifactID)
	if !resident {
		t.Fatal("task A's room is not resident, so nothing holds alice's client")
	}

	// Alice's heartbeat: the state she already stands at, one clock later, as a browser's
	// provider re-announces every 15 s. Its record waits, so the removal below is decided while
	// the relay still holds her at the clock before it.
	heartbeatClock.Store(clock + 1)
	go func() {
		defer close(recorded)
		if err := aw.ApplyUpdate(awarenessStates(aliceClient, 1, clock+1, string(state)), "alice's heartbeat"); err != nil {
			t.Errorf("announce alice's heartbeat at clock %d: %v", clock+1, err)
		}
	}()
	select {
	case <-held:
	case <-time.After(roomWait(t)):
		t.Fatal("alice's heartbeat never reached the relay's record")
	}

	// Alice leaves, at the clock her heartbeat stands at: the room's own removal, with the
	// tombstone it leaves purged before the relay reads the room for it.
	if err := aw.ApplyUpdate(awarenessStates(aliceClient, 1, clock+1, "null"), "a removal of task A's own"); err != nil {
		t.Fatalf("remove alice's client at clock %d: %v", clock+1, err)
	}
	<-recorded
	if purged.Load() == 0 {
		t.Fatal("no purge ran ahead of the relay's observer on task A's room, so the race this test is about never happened")
	}
	if roomHoldsClient(pair.taskA, pair.artifactID, aliceClient) {
		t.Fatal("task A's room still holds alice's client after her removal, so nothing here is tested")
	}
	requireAwareness(t, bob, lacksAwareness(aliceClient),
		"alice leaving task A over her own heartbeat, her tombstone purged before the relay read it, removing her from bob's reader on task B")
}

// The mirror of the test above, the two orders of one pair of changes: there the removal is
// recorded last, here the heartbeat is. Alice stands on the relay's list at c; her heartbeat at
// c+1 is decided - the relay's observer reads the clock the room holds her at - and its record
// is held there; her removal at c+1 is applied on another goroutine and recorded first, which
// takes her off the list this process relays and answers queries from and puts her removal in
// the room's pending batch; and the heartbeat's record follows it.
//
// Recorded as it stands, that heartbeat is a state of a client the list no longer holds, which
// puts her back on it and leaves a nil in the batch over her removal - and the flush reads a
// room that holds nothing of her and drops the nil (resolve), so the removal reaches no other
// process at all and alice's cursor sits in bob's room until it expires (awarenessExpiry, 30 s),
// while task A answers the next query with a reader who is gone. Settled against the room as it
// is recorded, the heartbeat is dropped whole and her removal stands (record).
func TestAReadersHeartbeatRecordedOverItsOwnRemovalStillLeavesTheOtherProcess(t *testing.T) {
	pair := newSeededProcessPair(t, "First.\n\nSecond.\n")
	cursor := cursorOf("alice", 1)
	state, err := json.Marshal(cursor)
	if err != nil {
		t.Fatalf("encode alice's state: %v", err)
	}
	// The bound the hook's wait runs on, taken here: roomWait reads the binary's deadline
	// through t, and the hook runs on the goroutine of alice's heartbeat.
	wait := roomWait(t)
	var beating atomic.Bool
	var removals atomic.Int64
	var heartbeatClock atomic.Uint64
	held := make(chan struct{})     // the heartbeat has been decided and its record waits
	gone := make(chan struct{})     // her removal has been recorded, so the heartbeat may record
	recorded := make(chan struct{}) // the heartbeat's apply has returned, its record run
	// Her own socket's close removes her client again as the test ends, and a frame of the room
	// can name her once more; only the heartbeat this test is about is held.
	pair.taskA.beforeAwarenessRecord = func(room string, aw *awareness.Awareness, entries []awarenessEntry) {
		switch beat := heartbeatClock.Load(); {
		case removesAlice(entries):
			removals.Add(1)
		case beat != 0 && announcesAlice(entries, beat) && beating.CompareAndSwap(false, true):
			close(held)
			select {
			case <-gone:
			case <-time.After(wait):
				t.Error("alice's removal was never recorded while her heartbeat's record waited")
			}
		}
	}
	runAwareness(t, pair.taskA)
	runAwareness(t, pair.taskB)
	alice := pair.openReader(t, pair.serverA, "alice")
	bob := pair.openReader(t, pair.serverB, "bob")
	setAwareness(t, alice, aliceClient, cursor)
	requireAwareness(t, bob, holdsAwareness(aliceClient, cursor), "alice's cursor on task A reaching bob on task B")
	clock := awarenessClockOf(t, pair.taskA, pair.artifactID, aliceClient)
	aw, resident := pair.taskA.srv.GetAwareness(pair.artifactID)
	if !resident {
		t.Fatal("task A's room is not resident, so nothing holds alice's client")
	}

	// Alice's heartbeat: the state she already stands at, one clock later, as a browser's
	// provider re-announces every 15 s. Its record waits where the relay has read the clock the
	// room holds her at and not yet recorded it.
	heartbeatClock.Store(clock + 1)
	go func() {
		defer close(recorded)
		if err := aw.ApplyUpdate(awarenessStates(aliceClient, 1, clock+1, string(state)), "alice's heartbeat"); err != nil {
			t.Errorf("announce alice's heartbeat at clock %d: %v", clock+1, err)
		}
	}()
	select {
	case <-held:
	case <-time.After(wait):
		t.Fatal("alice's heartbeat never reached the relay's record")
	}

	// Alice leaves at the clock her heartbeat stands at, on this goroutine: a change's record
	// runs inside the apply that made it, so her removal is recorded - and her heartbeat's
	// record released - before this returns.
	if err := aw.ApplyUpdate(awarenessStates(aliceClient, 1, clock+1, "null"), "a removal of task A's own"); err != nil {
		t.Fatalf("remove alice's client at clock %d: %v", clock+1, err)
	}
	if removals.Load() == 0 {
		t.Fatal("alice's removal never reached the relay's record, so the race this test is about never happened")
	}
	close(gone)
	<-recorded
	if roomHoldsClient(pair.taskA, pair.artifactID, aliceClient) {
		t.Fatal("task A's room still holds alice's client after her removal, so nothing here is tested")
	}

	if relayed, listed := relayedClientClock(pair.taskA, pair.artifactID, aliceClient); listed {
		t.Errorf("task A's relay lists alice at clock %d among the clients it relays, though she left its room: its next answer to another process's query puts her cursor back",
			relayed)
	}
	// Her removal, sent, reaches bob within a coalesce window; one the heartbeat replaced in the
	// batch reaches him never, and his room holds her cursor until it expires (awarenessExpiry,
	// 30 s), which is the failure this test is about - so the wait ends well short of that
	// expiry, whatever roomWait allows.
	prompt := min(wait, awarenessExpiry/3)
	if err := bob.WaitForAwareness(prompt, lacksAwareness(aliceClient)); err != nil {
		t.Fatalf("alice leaving task A under her own heartbeat, her heartbeat recorded after her removal, did not remove her from bob's reader on task B within %v: %v",
			prompt, err)
	}
}

// The relay reads a room's awareness while it holds its own lock (record,
// awarenessClientStanding), and a room's awareness tells the relay what each change touched -
// which takes that same lock - from inside ygo's own write paths. The one order is safe only
// while ygo fires its observers with its awareness released: fired under it, an observer would
// take the relay's lock behind ygo's where the relay takes ygo's behind its own, and a removal
// recorded while another goroutine wrote the room would deadlock it. ygo keeps that lock
// private, so nothing of this package can hold it other than by being inside one of these
// calls, and an observer is the one place the order can invert.
//
// Each write below runs with an observer that reads the awareness every way the relay does and
// waits for a write of another goroutine's. An awareness still locked when its observer runs
// never returns from those reads, which the deadline catches; one fired under the read side of
// its lock alone (sync.RWMutex, awareness/awareness.go:135) would return from the reads - a read
// lock taken twice is taken - and never from the wait, since the writer queued behind the
// observer's own read lock is what a room's record would deadlock against.
func TestARoomsAwarenessFiresItsObserversWithItsLockReleased(t *testing.T) {
	const cursor = `{"user":{"name":"alice"}}`
	live := func(aw *awareness.Awareness) {
		if err := aw.ApplyUpdate(awarenessStates(aliceClient, 1, 1, cursor), "a peer of this process"); err != nil {
			t.Fatalf("announce alice's client: %v", err)
		}
	}
	writes := []struct {
		what  string
		seed  func(aw *awareness.Awareness)
		write func(aw *awareness.Awareness) error
	}{
		{what: "a client's state", write: func(aw *awareness.Awareness) error {
			return aw.ApplyUpdate(awarenessStates(aliceClient, 1, 1, cursor), "a peer of this process")
		}},
		{what: "a client's heartbeat", seed: live, write: func(aw *awareness.Awareness) error {
			return aw.ApplyUpdate(awarenessStates(aliceClient, 1, 2, cursor), "a peer of this process")
		}},
		{what: "a client's removal", seed: live, write: func(aw *awareness.Awareness) error {
			return aw.ApplyUpdate(awarenessStates(aliceClient, 1, 2, "null"), "a peer of this process")
		}},
		{what: "the local client's own state", write: func(aw *awareness.Awareness) error {
			aw.SetLocalState(cursorOf("bob", 1))
			return nil
		}},
		{what: "the local client's heartbeat", seed: func(aw *awareness.Awareness) { aw.SetLocalState(cursorOf("bob", 1)) },
			write: func(aw *awareness.Awareness) error {
				aw.Heartbeat()
				return nil
			}},
		{what: "an expiry", seed: live, write: func(aw *awareness.Awareness) error {
			aw.RemoveExpired(time.Nanosecond)
			return nil
		}},
	}
	for _, write := range writes {
		t.Run(write.what, func(t *testing.T) {
			aw := awareness.New(bobClient)
			if write.seed != nil {
				write.seed(aw)
			}
			var fired atomic.Int64
			// The one write the observer below queues, which its own firing - on the goroutine
			// that write runs on - must not queue again.
			var queueing atomic.Bool
			aw.OnUpdate(func(event awareness.UpdateEvent) {
				fired.Add(1)
				var queued chan struct{}
				if queueing.CompareAndSwap(false, true) {
					queued = make(chan struct{})
					// A write of this same awareness, from another goroutine: it takes the
					// write side of the awareness's lock, which neither side held by this
					// observer ever lets go of. It is the local client's own state, the one
					// write that touches no client a subtest reads.
					go func() {
						defer close(queued)
						aw.SetLocalState(cursorOf("bob", 2))
					}()
				}
				// Every read the relay makes from inside an observer: the clock a change
				// stands at (touched), what the room holds of one client (record,
				// awarenessClientStanding), and the states a query is answered from
				// (localClients).
				aw.Meta(aliceClient)
				aw.EncodeUpdate([]uint64{aliceClient})
				aw.GetStates()
				if queued == nil {
					return
				}
				select {
				case <-queued:
				case <-time.After(roomWait(t)):
					t.Errorf("a room's awareness was still locked while its observer ran: a write of another goroutine's, queued from inside the observer %s fires, did not finish within %v",
						write.what, roomWait(t))
				}
			})
			done := make(chan error, 1)
			go func() { done <- write.write(aw) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("apply %s: %v", write.what, err)
				}
			case <-time.After(roomWait(t)):
				t.Fatalf("a room's awareness was still locked while its observer ran: reading it from inside the observer %s fires did not return within %v",
					write.what, roomWait(t))
			}
			if fired.Load() == 0 {
				t.Fatalf("%s fired no update observer, so nothing here is tested", write.what)
			}
			if !queueing.Load() {
				t.Fatalf("no write of another goroutine's was queued from inside the observer %s fires, so the lock it would wait on is untested", write.what)
			}
		})
	}
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
