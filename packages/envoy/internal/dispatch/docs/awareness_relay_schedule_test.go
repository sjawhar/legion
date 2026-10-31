package docs

// One test drives the awareness relay of two processes through randomized interleavings of what
// a room's readers do: a reader's frame, a reader leaving, a reader back in the room at a later
// clock, an answer to another process's query, a purge of the room's tombstones, the coalescing
// window ending, and the other process's notification arriving. The races the relay settles were
// each found one at a time and have a test of their own (awareness_relay_race_test.go); this one
// looks for the ones nobody has thought of yet, and the seeds it has already found are run first,
// every run (relayScheduleRegressions).
//
// It drives the relay itself - touched, record, flush, resolve, awarenessNotifications, answer,
// received, localRemoval, reclaimAwarenessSlots - over two ygo awareness instances, one room to a
// process, with what one process sends handed to the other's received. Nothing else of the
// service is here: a schedule is its own awarenessHost, so it needs neither a database nor a ygo
// server nor a timer.
//
// Every schedule is one goroutine at a time. An operation runs on a goroutine of its own and
// parks in the windows real goroutines interleave in (awarenessWindows); the scheduler hands one
// thread the baton and waits for it to park or finish before it picks the next, so a seed replays
// the same schedule. What a schedule is not is a model of the relay: the invariants it checks are
// about the two rooms, not about what the relay did to them.
//
// What a schedule holds still, because what it would break is a decision of the design rather
// than a fault of the relay's:
//
//   - both rooms stay resident, with a reader of their own process connected, for every schedule.
//     A room that retires keeps nothing of the other process's readers, and one no reader of this
//     process is in is applied nothing (received).
//   - a client's own clock only ever goes up, as a y-protocols client's counter does, and each
//     client belongs to one process's socket, which sends one frame at a time.
//   - only a room's own readers are removed. An expiry takes away the clients another process
//     relayed too, and the relay sends that removal back to the process those readers are on,
//     which is how a reader of a process that stopped relaying is dropped everywhere
//     (awarenessExpiry); the reader's own next announcement, 15 s later, brings it back.
//   - no relay is closed: Close refuses every send after it, so a schedule that closed one would
//     be checking what a process does after its last word.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/encoding"
)

const (
	// scheduleRoom is the document both processes hold a room of.
	scheduleRoom = "schedule-doc"
	// scheduleFirstClient is the first awareness client id a schedule's readers announce. The
	// awareness of each room is its own client (scheduleRoomClient), far from these, since ygo
	// refuses a removal of the client an awareness was built for (awareness/awareness.go:596).
	scheduleFirstClient uint64 = 101
	// scheduleRoomClient is the client id the room's own awareness stands under on process A; B's
	// is the next one.
	scheduleRoomClient uint64 = 1 << 32
	// scheduleCount is how many schedules the test runs when scheduleCountEnv names no other
	// number. It is what fits in a few seconds of CI; a local run raises it.
	scheduleCount = 10_000
	// scheduleCountEnv raises that count: DISPATCH_RELAY_SCHEDULES=1000000 go test ...
	scheduleCountEnv = "DISPATCH_RELAY_SCHEDULES"
	// scheduleDrainRounds is how many rounds a schedule's drain takes before the test calls it
	// stuck. A drain finishes in as many rounds as it has threads, flushes and notifications
	// left, which is a handful; this is far above any of them and catches a relay that answers
	// its own sends.
	scheduleDrainRounds = 10_000
)

// relayScheduleRegressions are the seeds whose schedules once broke an invariant below, each
// with the fault it found. Every one of them runs first, on every run, whatever count the
// randomized seeds are run at: what a fix is worth is that the schedule which found the fault
// passes. A seed is kept because it fails with its fix removed, and which schedule a seed
// names is the generator's to decide: a change to the generator moves every seed, so after one,
// re-derive these by taking each fix out in turn and running the schedules again.
//
//   - 21: a reader left, was back in the room at a higher clock and left again within one
//     window. The second departure reached the relay after the first removal had taken the
//     client off the list and a purge had freed its tombstone, so it stood at a clock of 0
//     (touched), and recorded last it was written over the removal already in the batch at the
//     clock the list had held. The other process, holding that reader at a clock of its own,
//     dropped the clock-0 removal and kept its cursor. The batch keeps each entry's clock now,
//     and a removal recorded over a later entry of the same client stands aside (record).
//   - 3569: a reader's socket closed and the reader was back at a higher clock while a flush of
//     its room was in flight - the removal recorded into the next batch, after that flush had
//     taken its own, and the return landing before the flush's read of the room. The flush read
//     the reader live and sent that state, and the removal waiting behind it stood at the clock
//     the list had held, below the state. The other process took the state and dropped the
//     removal. A flush raises the removal a client taken off the list in that window left
//     behind, to the clock of the state it sends (resolve).
//   - 14360: two flushes of one room under way together sent a reader's removal ahead of the
//     state an earlier batch's read had taken of it, both at the clock the reader's last frame
//     stood at, and the other process, which frees a room's tombstones after every apply
//     (reclaimAwarenessSlots), took that state for a reader who had left. A room's batches are
//     taken and sent one flush at a time now, so the state goes first and the removal at its
//     clock takes it away (takeBatch, flush).
var relayScheduleRegressions = []uint64{21, 3569, 14360}

// relayClient is one reader's awareness client: the process whose room it is a client of, the
// clock its own frames stand at - its own counter, which never goes back, as a y-protocols
// client's does - and where its cursor is.
type relayClient struct {
	id    uint64
	home  int
	clock uint64
	at    int
}

// relayProcess is one of the two processes a schedule runs on, and the relay's host there
// (awarenessHost): its room's awareness, the relay watching it, the flushes its records armed,
// and the notifications the other process sent it. Its one awareness lane handles those in the
// order they arrived, one at a time, as room sync's lane does (roomSyncHandler).
type relayProcess struct {
	name    string
	aw      *awareness.Awareness
	relay   *awarenessRelay
	peer    *relayProcess
	windows awarenessWindows
	// flushes are the coalescing windows record armed and no operation has ended yet, oldest
	// first. The relay arms one per room and arms none while a batch is pending, so this holds
	// one at a time.
	flushes []func()
	// inbox is what the other process sent and this one has not handled yet, oldest first.
	inbox []string
	// delivering is whether a notification is being handled now: the lane takes the next one
	// only once the one before it has returned.
	delivering bool
}

func (p *relayProcess) roomAwareness(room string) (*awareness.Awareness, bool) {
	if room != scheduleRoom {
		return nil, false
	}
	return p.aw, true
}

func (p *relayProcess) injectAwareness(_ context.Context, room string, update []byte) error {
	if room != scheduleRoom {
		return nil
	}
	return p.aw.ApplyUpdate(update, "the other process's relay")
}

func (p *relayProcess) notifyAwareness(_ context.Context, payload string) error {
	p.peer.inbox = append(p.peer.inbox, payload)
	return nil
}

func (p *relayProcess) relaySender() string { return p.name }

func (p *relayProcess) hasLocalConnection(room string) bool { return room == scheduleRoom }

func (p *relayProcess) holdsRoomForReaders(room string) bool { return room == scheduleRoom }

func (p *relayProcess) coalesceAwareness(flush func()) { p.flushes = append(p.flushes, flush) }

func (p *relayProcess) awarenessWindows() awarenessWindows { return p.windows }

// holds is what p's room holds of client now, and whether it holds a state of it at all.
func (p *relayProcess) holds(client uint64) (awareness.ClientState, bool) {
	state, held := p.aw.GetStates()[client]
	return state, held
}

// lists is the clock p's relay holds client at among the clients it relays (local), and whether
// it lists the client at all.
func (p *relayProcess) lists(client uint64) (uint64, bool) {
	p.relay.mu.Lock()
	defer p.relay.mu.Unlock()
	clock, listed := p.relay.local[scheduleRoom][client]
	return clock, listed
}

// relayThread is one operation's goroutine. One thread of a schedule runs at a time: the
// scheduler hands this one the baton (relaySchedule.step) and waits for it to park (park) or
// finish.
type relayThread struct {
	what    string
	where   string
	resume  chan struct{}
	stopped chan struct{}
	done    bool
	// onDone runs on the scheduler's side once the thread has finished.
	onDone func()
}

// relayOpKind is what one operation of a schedule does.
type relayOpKind int

const (
	// opAnnounce is a reader's own frame: its cursor where it has moved to, or its presence
	// again at a later clock, which is the heartbeat a browser sends every 15 s. A client that
	// left the room is back in it by the same path, at the clock after the one it stood at, as a
	// y-protocols client's own counter gives.
	opAnnounce relayOpKind = iota
	// opDepart is the removal a reader's socket closing applies to the room, at the clock the
	// client stands at (awarenessRemoval). ygo's own expiry removes a client the same way.
	opDepart
	// opEcho is one socket's frame carrying another reader's client as well as its own: its own
	// announced at its next clock, the other's taken away at the clock it stands at, which is
	// what a disconnecting tab's frame does to the clients it echoed. ygo fires one event for
	// the two, so the relay decides a state and a removal together (touched).
	opEcho
	// opClose is what a close of the room applies: every local client of the room removed at
	// once, read from the relay's own list (Service.removeLocalAwareness, localRemoval).
	opClose
	// opQuery is a query the process sends the other, which the other answers with its local
	// clients' states (answer).
	opQuery
	// opPurge frees the room's tombstones (reclaimAwarenessSlots), which every inbound apply and
	// every removal of the room's own does.
	opPurge
	// opFlush is a coalescing window ending: the batch is taken, resolved against the room and
	// sent (flush).
	opFlush
	// opDeliver is one notification the other process sent, handled on the awareness lane
	// (received).
	opDeliver
)

// relayOp is one operation of a schedule: what it does, on which process, and - where the kind
// names one - to which client.
type relayOp struct {
	kind   relayOpKind
	proc   int
	client int
	// echoed is the client an opEcho's frame takes away, which reads on the same process as
	// client.
	echoed int
	// move is whether an announcement carries a cursor that has moved, rather than the same
	// cursor at a later clock.
	move bool
}

// relayOpDraw is the bag each of a schedule's operations is drawn from. A reader's own frames are
// the commonest, as they are in a room; flushes and deliveries are next, since every other
// operation's work reaches the other process through them.
var relayOpDraw = [...]relayOpKind{
	opAnnounce, opAnnounce, opAnnounce, opAnnounce, opAnnounce, opAnnounce, opAnnounce, opAnnounce,
	opDepart, opDepart, opDepart, opDepart,
	opEcho, opEcho,
	opClose,
	opQuery, opQuery,
	opPurge, opPurge, opPurge,
	opFlush, opFlush, opFlush, opFlush, opFlush, opFlush,
	opDeliver, opDeliver, opDeliver, opDeliver, opDeliver, opDeliver, opDeliver, opDeliver,
}

// relaySchedule is one seed's run: the two processes, the readers of their rooms, the operations
// left to start, and the threads of the ones under way.
type relaySchedule struct {
	seed    uint64
	rand    *rand.Rand
	procs   [2]*relayProcess
	clients []*relayClient
	ops     []relayOp
	threads []*relayThread
	running *relayThread
	// started counts the threads the schedule has started, which numbers each in its steps: a
	// room's flushes, say, all read alike without one.
	started int
	// steps is the schedule as it ran, one line to a step, which a failure prints.
	steps []string
	// failures are what the schedule broke, which is nothing in a passing run.
	failures []string
}

// newRelaySchedule builds the schedule seed names: two to four readers over two processes, and
// ten to forty operations.
func newRelaySchedule(seed uint64) *relaySchedule {
	s := &relaySchedule{seed: seed, rand: rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15))}
	for i := range s.procs {
		process := &relayProcess{
			name: string(rune('A' + i)),
			aw:   awareness.New(scheduleRoomClient + uint64(i)),
		}
		process.windows = awarenessWindows{
			beforeWatch: func(_ string, aw *awareness.Awareness) {
				// An observer of the schedule's own, ahead of the relay's, which ygo fires
				// first: the window between a change of the room and the relay hearing of it,
				// where a purge, a reader's return and another path's removal each land.
				aw.OnUpdate(func(awareness.UpdateEvent) {
					s.park("between a change of the room and the relay hearing of it")
				})
			},
			beforeSnapshot: func(string, *awareness.Awareness, []awarenessEntry) {
				s.park("between what a change read of the relay's lists and its read of the room")
			},
			beforeRecord: func(string, *awareness.Awareness, []awarenessEntry) {
				s.park("between what a change touched and the record of it")
			},
			beforeAnswerRecord: func(string, *awareness.Awareness, []awarenessEntry) {
				s.park("between the read an answer stands on and the record of it")
			},
			beforeResolve: func(string, map[uint64]awarenessEntry) {
				s.park("between a flush taking its batch and the read that resolves it")
			},
			beforeRaise: func(string, map[uint64]awarenessEntry) {
				s.park("between a flush's read of the room and the raise of the list to it")
			},
		}
		process.relay = newAwarenessRelay(process)
		s.procs[i] = process
	}
	s.procs[0].peer, s.procs[1].peer = s.procs[1], s.procs[0]
	for _, process := range s.procs {
		// What RoomActivated does for a room that is resident. The query it also sends runs on a
		// goroutine of its own, which a strictly stepped schedule cannot hold; a schedule sends
		// its queries as operations of its own (opQuery), which is the same payload on the same
		// channel.
		process.relay.watchRoomAwareness(scheduleRoom, process.aw)
	}
	for i := range 2 + s.rand.IntN(3) {
		home := i
		if i > 1 {
			home = s.rand.IntN(2)
		}
		s.clients = append(s.clients, &relayClient{id: scheduleFirstClient + uint64(i), home: home})
	}
	for range 10 + s.rand.IntN(31) {
		op := relayOp{kind: relayOpDraw[s.rand.IntN(len(relayOpDraw))], move: s.rand.IntN(2) == 0}
		switch op.kind {
		case opAnnounce, opDepart:
			op.client = s.rand.IntN(len(s.clients))
			op.proc = s.clients[op.client].home
		case opEcho:
			op.client = s.rand.IntN(len(s.clients))
			op.proc = s.clients[op.client].home
			op.echoed = s.echoed(op.client)
			if op.echoed == op.client {
				// The process has this one reader, so the frame carries its own client alone.
				op.kind = opAnnounce
			}
		default:
			op.proc = s.rand.IntN(2)
		}
		s.ops = append(s.ops, op)
	}
	return s
}

// echoed is another reader of the same process as client, for a frame of that reader's socket to
// carry away (opEcho), or client itself where the process has no other reader.
func (s *relaySchedule) echoed(client int) int {
	others := make([]int, 0, len(s.clients))
	for other, reader := range s.clients {
		if other != client && reader.home == s.clients[client].home {
			others = append(others, other)
		}
	}
	if len(others) == 0 {
		return client
	}
	return others[s.rand.IntN(len(others))]
}

// play runs the schedule: at each step it either starts the next operation or carries one that is
// parked a step further, picked at random, until every operation has run and no thread is left.
// It then drains and checks what the two rooms hold.
func (s *relaySchedule) play() {
	for len(s.ops) > 0 || len(s.threads) > 0 {
		pick := s.rand.IntN(len(s.threads) + min(len(s.ops), 1))
		if pick == len(s.threads) {
			op := s.ops[0]
			s.ops = s.ops[1:]
			s.begin(op)
			continue
		}
		s.resume(s.threads[pick], "")
	}
	s.drain()
	s.check()
}

// drain finishes what the schedule left in flight: every thread still parked, every coalescing
// window still armed, and every notification still waiting on a lane - and what each of those
// starts in turn, until the two processes owe each other nothing.
func (s *relaySchedule) drain() {
	for round := 0; ; round++ {
		if round > scheduleDrainRounds {
			s.fail(fmt.Sprintf("the schedule never came to rest: %d threads, %d flushes and %d notifications after %d rounds of draining",
				len(s.threads), len(s.procs[0].flushes)+len(s.procs[1].flushes), len(s.procs[0].inbox)+len(s.procs[1].inbox), round))
			return
		}
		switch {
		case len(s.threads) > 0:
			s.resume(s.threads[0], "drain: ")
		case len(s.procs[0].flushes) > 0 || len(s.procs[1].flushes) > 0:
			s.begin(relayOp{kind: opFlush, proc: s.withFlush()})
		case len(s.procs[0].inbox) > 0 || len(s.procs[1].inbox) > 0:
			s.begin(relayOp{kind: opDeliver, proc: s.withNotification()})
		default:
			return
		}
	}
}

// withFlush is the process a drain flushes next: the one with a window still armed, process A
// first where both have one.
func (s *relaySchedule) withFlush() int {
	if len(s.procs[0].flushes) > 0 {
		return 0
	}
	return 1
}

// withNotification is the process a drain delivers to next: the one with a notification still
// waiting on its lane, process A first where both have one.
func (s *relaySchedule) withNotification() int {
	if len(s.procs[0].inbox) > 0 {
		return 0
	}
	return 1
}

// begin starts op on a thread of its own, as far as its first window. An operation whose room has
// nothing for it - a flush with no window armed, a delivery with nothing on the lane or one
// already being handled, a close of a room with no local client - is a step that does nothing,
// which the schedule records as it ran.
func (s *relaySchedule) begin(op relayOp) {
	process := s.procs[op.proc]
	switch op.kind {
	case opAnnounce:
		client := s.clients[op.client]
		client.clock++
		if op.move {
			client.at++
		}
		state := fmt.Sprintf(`{"cursor":%d}`, client.at)
		update := awarenessStates(client.id, 1, client.clock, state)
		s.start(fmt.Sprintf("%s: client %d announces %s at clock %d", process.name, client.id, state, client.clock), nil, func() {
			s.applied(process, update, "a reader's frame")
		})
	case opDepart:
		client := s.clients[op.client]
		update := awarenessStates(client.id, 1, client.clock, "null")
		s.start(fmt.Sprintf("%s: client %d's socket closes, removing it at clock %d", process.name, client.id, client.clock), nil, func() {
			s.applied(process, update, "a reader's socket closing")
		})
	case opEcho:
		client, echoed := s.clients[op.client], s.clients[op.echoed]
		client.clock++
		if op.move {
			client.at++
		}
		state := fmt.Sprintf(`{"cursor":%d}`, client.at)
		update := encodeAwarenessEntries([][]byte{
			relayStateEntry(client.id, client.clock, state),
			awarenessRemoval(echoed.id, echoed.clock),
		})
		s.start(fmt.Sprintf("%s: client %d's socket announces %s at clock %d and takes client %d away at clock %d",
			process.name, client.id, state, client.clock, echoed.id, echoed.clock), nil, func() {
			s.applied(process, update, "a reader's frame carrying another reader's client")
		})
	case opClose:
		s.start(fmt.Sprintf("%s: the room closes, removing its local clients", process.name), nil, func() {
			removal := process.relay.localRemoval(scheduleRoom, process.aw)
			if removal == nil {
				return
			}
			s.park("between a closing room's read of its local clients and the removal of them")
			s.applied(process, removal, "a closing room")
		})
	case opQuery:
		payload, err := json.Marshal(awarenessMessage{Sender: process.name, Room: scheduleRoom, Query: true})
		if err != nil {
			s.fail(fmt.Sprintf("encode %s's query: %v", process.name, err))
			return
		}
		process.peer.inbox = append(process.peer.inbox, string(payload))
		s.note(fmt.Sprintf("%s: asks %s for its local clients' states", process.name, process.peer.name))
	case opPurge:
		s.start(fmt.Sprintf("%s: the room's tombstones are freed", process.name), nil, func() {
			reclaimAwarenessSlots(scheduleRoom, process.aw)
		})
	case opFlush:
		if len(process.flushes) == 0 {
			s.note(fmt.Sprintf("%s: a coalescing window ends with no batch pending", process.name))
			return
		}
		flush := process.flushes[0]
		process.flushes = process.flushes[1:]
		s.start(fmt.Sprintf("%s: the coalescing window ends and its batch is sent", process.name), nil, flush)
	case opDeliver:
		if process.delivering {
			s.note(fmt.Sprintf("%s: its awareness lane is still handling the notification before this one", process.name))
			return
		}
		if len(process.inbox) == 0 {
			s.note(fmt.Sprintf("%s: nothing has arrived on its awareness lane", process.name))
			return
		}
		payload := process.inbox[0]
		process.inbox = process.inbox[1:]
		process.delivering = true
		s.start(fmt.Sprintf("%s: handles %s", process.name, describeNotification(payload)), func() { process.delivering = false }, func() {
			process.relay.received(context.Background(), payload)
		})
	}
}

// relayStateEntry is one client's entry of an awareness update: its id, its clock and its state,
// as awarenessRemoval writes the entry that takes a client away.
func relayStateEntry(client, clock uint64, state string) []byte {
	return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(client)
		encoder.WriteVarUint(clock)
		encoder.WriteVarString(state)
	})
}

// start runs body on a thread of its own, as far as its first window, and records what it is
// doing. onDone runs once the thread has finished, where the operation holds something until
// then.
func (s *relaySchedule) start(what string, onDone func(), body func()) {
	s.started++
	what = fmt.Sprintf("#%d %s", s.started, what)
	thread := &relayThread{
		what:    what,
		resume:  make(chan struct{}),
		stopped: make(chan struct{}),
		onDone:  onDone,
	}
	s.threads = append(s.threads, thread)
	go func() {
		defer func() {
			if panicked := recover(); panicked != nil {
				s.fail(fmt.Sprintf("%s panicked: %v", what, panicked))
			}
			thread.done = true
			thread.stopped <- struct{}{}
		}()
		<-thread.resume
		body()
	}()
	s.step(thread, what)
}

// resume carries a parked thread a step further.
func (s *relaySchedule) resume(thread *relayThread, prefix string) {
	s.step(thread, fmt.Sprintf("%sresume %s, parked %s", prefix, thread.what, thread.where))
}

// step hands thread the baton and waits for it to park or finish. Nothing else of the schedule
// runs while it holds it, which is what makes a seed replay its schedule.
func (s *relaySchedule) step(thread *relayThread, note string) {
	s.note(note)
	s.running = thread
	thread.resume <- struct{}{}
	<-thread.stopped
	s.running = nil
	if !thread.done {
		return
	}
	s.threads = slices.DeleteFunc(s.threads, func(other *relayThread) bool { return other == thread })
	if thread.onDone != nil {
		thread.onDone()
	}
}

// park hands the baton back to the scheduler from inside the running thread's own code: where is
// the window it is parked in (awarenessWindows).
func (s *relaySchedule) park(where string) {
	thread := s.running
	thread.where = where
	thread.stopped <- struct{}{}
	<-thread.resume
}

// applied applies one update to a process's room, as a socket's frame, a disconnect and a close
// each reach it.
func (s *relaySchedule) applied(process *relayProcess, update []byte, what string) {
	if err := process.aw.ApplyUpdate(update, what); err != nil {
		s.fail(fmt.Sprintf("%s: apply %s: %v", process.name, what, err))
	}
}

func (s *relaySchedule) note(step string) { s.steps = append(s.steps, step) }

func (s *relaySchedule) fail(what string) { s.failures = append(s.failures, what) }

// check is what must hold once the schedule has come to rest, of every reader of either room:
//
//   - a reader that is not in its own process's room is in no room of the other's (I1): a cursor
//     the other process keeps sits in its readers' rooms until it expires, thirty seconds later
//     (awarenessExpiry).
//   - a reader that is in its own process's room is in the other's, at the same state and the
//     same clock (I2): a cursor the other process lost shows nothing of a reader who is there,
//     until that reader's own next frame.
//   - the relay's list of the clients it relays (local) holds that room's readers and no others
//     (I3): what a query is answered from (localClients), what a close removes (localRemoval)
//     and what a removal stands at (touched) all come off that list, so a client listed that has
//     left, or one in the room that is not listed, is what the next of those three gets wrong.
//     It is the relay's to keep, not a window into the room, and it holds once a schedule has
//     come to rest because the room's own observer hears every change the room makes and every
//     entry it hands record is settled against the room where its own clock cannot place it.
//     The list holds no client of the other process's (I4): the clients an inbound apply names
//     are marked as that process's for the apply, so the observer ygo fires inside it leaves
//     them where they came from (received, mark, touched).
//   - neither relay counts an inbound apply that has ended (I5), which is that same mark.
func (s *relaySchedule) check() {
	for _, client := range s.clients {
		home, away := s.procs[client.home], s.procs[1-client.home]
		here, live := home.holds(client.id)
		there, relayed := away.holds(client.id)
		switch {
		case !live && relayed:
			s.fail(fmt.Sprintf("client %d left %s's room and is still in %s's, at clock %d (I1)",
				client.id, home.name, away.name, there.Clock))
		case live && !relayed:
			s.fail(fmt.Sprintf("client %d is in %s's room at clock %d and in no room of %s's (I2)",
				client.id, home.name, here.Clock, away.name))
		case live && relayed && !reflect.DeepEqual(here.State, there.State):
			s.fail(fmt.Sprintf("client %d is %v on %s and %v on %s (I2)",
				client.id, here.State, home.name, there.State, away.name))
		case live && relayed && here.Clock != there.Clock:
			s.fail(fmt.Sprintf("client %d stands at clock %d on %s and %d on %s (I2)",
				client.id, here.Clock, home.name, there.Clock, away.name))
		}
		if clock, listed := home.lists(client.id); listed != live {
			if listed {
				s.fail(fmt.Sprintf("%s relays client %d, at clock %d, though the client left its room (I3)",
					home.name, client.id, clock))
			} else {
				s.fail(fmt.Sprintf("%s relays client %d none, though the client is in its room at clock %d (I3)",
					home.name, client.id, here.Clock))
			}
		}
		if clock, listed := away.lists(client.id); listed {
			s.fail(fmt.Sprintf("%s relays client %d, at clock %d, though the client is a reader of %s's (I4)",
				away.name, client.id, clock, home.name))
		}
	}
	for _, process := range s.procs {
		process.relay.mu.Lock()
		counted := len(process.relay.inbound)
		process.relay.mu.Unlock()
		if counted > 0 {
			s.fail(fmt.Sprintf("%s still counts an inbound apply of %d rooms (I5)", process.name, counted))
		}
	}
}

// report is the failure and the schedule that produced it: the seed to replay it with, the
// readers, what each room ended up holding, and every step in the order it ran.
func (s *relaySchedule) report() string {
	var out strings.Builder
	fmt.Fprintf(&out, "seed %d:\n", s.seed)
	for _, failure := range s.failures {
		fmt.Fprintf(&out, "  %s\n", failure)
	}
	for _, client := range s.clients {
		fmt.Fprintf(&out, "  client %d reads on %s\n", client.id, s.procs[client.home].name)
	}
	for _, process := range s.procs {
		states := process.aw.GetStates()
		held := make([]string, 0, len(states))
		for _, client := range slices.Sorted(maps.Keys(states)) {
			held = append(held, fmt.Sprintf("%d at clock %d, %v", client, states[client].Clock, states[client].State))
		}
		process.relay.mu.Lock()
		listed := fmt.Sprint(process.relay.local[scheduleRoom])
		process.relay.mu.Unlock()
		fmt.Fprintf(&out, "  %s's room holds [%s]; its relay lists %s\n", process.name, strings.Join(held, "; "), listed)
	}
	for i, step := range s.steps {
		fmt.Fprintf(&out, "  %3d. %s\n", i+1, step)
	}
	return out.String()
}

// describeNotification is what one awareness notification carries, as a schedule's steps name it.
func describeNotification(payload string) string {
	var message awarenessMessage
	if err := json.Unmarshal([]byte(payload), &message); err != nil {
		return fmt.Sprintf("a notification that does not parse: %v", err)
	}
	if message.Query {
		return fmt.Sprintf("%s's query", message.Sender)
	}
	update, err := relayNotificationUpdate(message)
	if err != nil {
		return fmt.Sprintf("a notification of %s's that does not decode: %v", message.Sender, err)
	}
	entries := make([]string, 0, len(update))
	for _, entry := range update {
		if entry.removed {
			entries = append(entries, fmt.Sprintf("%d removed at clock %d", entry.client, entry.clock))
			continue
		}
		entries = append(entries, fmt.Sprintf("%d at clock %d", entry.client, entry.clock))
	}
	return fmt.Sprintf("%s's notification of [%s]", message.Sender, strings.Join(entries, "; "))
}

// relayNotificationUpdate is the client entries one state notification carries.
func relayNotificationUpdate(message awarenessMessage) ([]awarenessEntry, error) {
	update, err := base64.StdEncoding.DecodeString(message.Data)
	if err != nil {
		return nil, err
	}
	return decodeAwarenessEntries(update)
}

// playRelaySchedule runs the schedule seed builds and returns what it broke, or the empty string
// when every invariant held.
func playRelaySchedule(seed uint64) string {
	schedule := newRelaySchedule(seed)
	schedule.play()
	if len(schedule.failures) == 0 {
		return ""
	}
	return schedule.report()
}

// Every interleaving of a room's comings and goings leaves the two processes holding the same
// readers: what a reader's own process's room holds of it, the other process's room holds too,
// and what it no longer holds is gone from both (check). The seeds that once broke that run
// first, then as many randomized ones as scheduleCountEnv asks for.
func TestEveryInterleavingOfARoomsAwarenessSettlesOnBothProcesses(t *testing.T) {
	for _, seed := range relayScheduleRegressions {
		if report := playRelaySchedule(seed); report != "" {
			t.Fatalf("a schedule that once broke an invariant breaks it again:\n%s", report)
		}
	}
	count := relaySchedules(t)
	// The seeds are shared out among as many workers as the machine has processors, each
	// schedule being its own two processes; the test reports the lowest seed any of them broke,
	// which is the one a run of this test names whatever the machine is.
	workers := min(runtime.GOMAXPROCS(0), count)
	var (
		group   sync.WaitGroup
		broke   = make([]uint64, workers)
		reports = make([]string, workers)
	)
	for worker := range workers {
		group.Go(func() {
			for seed := uint64(worker) + 1; seed <= uint64(count); seed += uint64(workers) {
				report := playRelaySchedule(seed)
				if report == "" {
					continue
				}
				broke[worker], reports[worker] = seed, report
				return
			}
		})
	}
	group.Wait()
	lowest, report := uint64(0), ""
	for worker := range workers {
		if reports[worker] != "" && (lowest == 0 || broke[worker] < lowest) {
			lowest, report = broke[worker], reports[worker]
		}
	}
	if report != "" {
		t.Fatalf("a schedule breaks an invariant; add %d to relayScheduleRegressions once it is fixed:\n%s", lowest, report)
	}
	t.Logf("%d schedules on %d workers, and the %d seeds that once failed", count, workers, len(relayScheduleRegressions))
}

// relaySchedules is how many randomized schedules to run: scheduleCount, or what scheduleCountEnv
// names.
func relaySchedules(t *testing.T) int {
	t.Helper()
	raw := os.Getenv(scheduleCountEnv)
	if raw == "" {
		return scheduleCount
	}
	count, err := strconv.Atoi(raw)
	if err != nil || count < 1 {
		t.Fatalf("%s is %q, want a count of one or more", scheduleCountEnv, raw)
	}
	return count
}
