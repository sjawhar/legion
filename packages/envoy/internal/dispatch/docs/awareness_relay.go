package docs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/reearth/ygo/awareness"
	"github.com/reearth/ygo/cluster"
	"github.com/reearth/ygo/encoding"
)

// Awareness - each reader's cursor and presence - crosses processes through Postgres, as every
// other part of room sync does (plan D4). The service attaches an awarenessRelay to its ygo server
// (Server.AttachRelay) when it is built, and the relay watches each room's awareness itself: it
// registers an observer on the awareness of every room that becomes resident here
// (RoomActivated, watchRoomAwareness), which ygo fires with the clients each change touched - a
// local peer's update or heartbeat, the removal of a peer that left, an expiry.
//
// It watches the room rather than taking what ygo's own relay path hands it (Publish), which is
// dropped: ygo queues an outbound awareness change on a lane that keeps one blob per room, the
// latest only (internal/relaylane/lane.go), and each blob holds only the clients its own change
// touched, so five readers moving in the same instant reach Publish as one of them and the other
// four would be lost until their next heartbeat - 15 s in a browser. The document updates ygo
// hands Publish are dropped too: edits cross processes as stored rows, through the catch-up
// (roomCatchUps), never through the relay.
//
// What the observer records it coalesces per room and per awareness client over
// awarenessCoalesceWindow; the flush then reads each pending client's current state from the
// room's awareness - the removals it recorded stand at the clock the departing client held - and
// sends it on documentAwarenessChannel through the process's NOTIFY sender (roomSyncSender).
// Each process's room sync hears the others' on its awareness lane (roomSync.handlers) and applies
// each to its own room of the document (Server.Inject), which hands it to that room's peers. A
// change the relay applied itself is not relayed back (inbound, received).
//
// What a room relays is bounded by the sockets it holds: a socket may announce no more than
// maxAwarenessClientsPerSocket awareness clients, and the clients a frame names past that are
// taken out of it before ygo applies it (awareness_limit.go), so a peer's frame inventing client
// ids grows neither a room's awareness nor the relay's fan-out. A client's entry is freed as the
// client leaves, rather than two expiries later (watchRoomAwareness, reclaimAwarenessSlots), so
// what a room tracks is what it holds now.
//
// Wire format, one JSON object per notification:
//
//	{"s": sender process id, "r": room, "d": base64 of a y-protocols awareness update}
//	{"s": sender process id, "r": room, "q": true}
//
// The first carries awareness states: a count, then each client's id, clock and state JSON (null
// for a client that left), as y-protocols encodes them. A batch whose notification would pass
// roomSyncPayloadLimit is split across several, whole clients to each; one client whose state
// alone passes it is dropped, logged at WARN and counted (awarenessRelay.oversized), and the other
// processes keep that client's last state until it expires (awarenessExpiry).
//
// The second is a query: a room became resident on its sender, or a reader joined one resident
// there (Service.authorize), and every process holding that room answers with the states of its
// own local clients, so a reader who opens a document second sees the first reader's cursor at
// once rather than at that reader's next heartbeat (15 s in y-protocols). It rides the awareness
// channel, not documentQueryChannel, because its answer is an ordinary awareness notification
// that waits for nobody: documentQueryChannel's queries are the ones a requester waits on.

const (
	// awarenessCoalesceWindow is how long a room's awareness changes gather before they are sent:
	// a client moving its cursor at typing speed costs one notification per window, not one per
	// keystroke, each notification being a commit Postgres serialises with every other.
	awarenessCoalesceWindow = 50 * time.Millisecond
	// awarenessExpiry is how long a room keeps an awareness client it hears nothing from
	// (Server.AwarenessExpiry). A browser re-announces itself every 15 s, so a live client never
	// expires, and one relayed from a process that died without saying so goes after this. An
	// expiry is an awareness change like any other and is relayed, so a client this process
	// stopped hearing from - its own process's relay was down for longer than this - is removed
	// on its home process too; the y-protocols client then sees its own state removed and
	// announces itself again, which is how it comes back everywhere.
	awarenessExpiry = 30 * time.Second
	// maxAwarenessClientsPerRoom is the last-resort cap on the client entries a room of this
	// process holds - the ones present and the tombstones of the ones that left - which ygo
	// enforces on every room's awareness (Server.MaxAwarenessClientsPerRoom in docs/service.go,
	// which it forwards to awareness.Awareness.SetMaxClients): past it a client id the room has
	// not seen before is refused, whether it arrives in a browser's frame or in another
	// process's notification (awareness/awareness.go:608-623). A refused client is a reader
	// nobody sees - neither the room's own readers nor, since the relay sends what the room
	// took, the readers of any other process - so the bound has to stand above every client
	// real readers can put in a room, and it is derived from the two caps that bound them:
	// this process's sockets, every one it admits (maxRoomConnections) holding the clients a
	// socket may hold (maxAwarenessClientsPerSocket, awareness_limit.go), and the clients
	// another process relays, which that process's own sockets are held to in the same way
	// before they cross (received). They share this bound: four thousand entries is every
	// connection this process admits, all of them on one document, each holding the four clients
	// a socket may hold, of which only one is a client of that tab's own (awareness_limit.go),
	// and a process refuses the socket after that whatever its rooms hold
	// (Service.canAddConnection). Relayed clients reach it no sooner:
	// a document would have to hold four thousand tabs at once - every socket of four
	// processes at their connection caps - against the handful any document of this service
	// has. What stops one tab from spending the room's bound on invented ids is the socket's
	// own bound, applied before ygo reads its frame; this one is what is left if any other
	// path ever reaches a room's awareness. A client that leaves gives its entry back at once
	// rather than two expiries later (reclaimAwarenessSlots), so it counts the clients a room
	// holds now, and it is what bounds a room's entries where the byte bound does not - a
	// client id costs an entry whether its state is cursor-sized or null.
	maxAwarenessClientsPerRoom = maxRoomConnections * maxAwarenessClientsPerSocket
	// awarenessTombstoneAge is the age reclaimAwarenessSlots frees a room's tombstones from. ygo
	// takes an age and refuses a non-positive one (awareness/awareness.go:883-886), so the
	// shortest positive one stands for "every tombstone the room holds now".
	awarenessTombstoneAge = time.Nanosecond
	// maxAwarenessBytesPerRoom is how many bytes of its clients' states, as they arrived on the
	// wire, a room holds at once (Server.MaxAwarenessBytesPerRoom, which ygo forwards to
	// awareness.Awareness.SetMaxBytes). It counts what a room holds now and nothing else: a
	// client's bytes are released as it leaves or expires, so the entry it leaves behind costs
	// nothing against this bound, which is why the room's client bound is the one that holds a
	// frame of invented ids. It is the room's bound of clients each holding a state as large as
	// one notification carries (roomSyncPayloadLimit) - about 31 MB - so no state small enough
	// for the relay to send is refused for the room's total, and presence costs a room no more
	// than this. Reaching it takes every socket this process admits, in one document, each
	// holding four states the size of a whole notification where a reader's cursor is a few
	// hundred bytes; those sockets cost more than their states do. Past it ygo takes the state
	// as null, which removes that client's presence; the client's next announcement, under a
	// room back below the bound, brings it back.
	maxAwarenessBytesPerRoom = maxAwarenessClientsPerRoom * roomSyncPayloadLimit
)

// awarenessMessage is one documentAwarenessChannel notification (see the wire format above).
type awarenessMessage struct {
	Sender string `json:"s"`
	Room   string `json:"r"`
	Data   string `json:"d,omitempty"`
	Query  bool   `json:"q,omitempty"`
}

// awarenessEntry is one client's entry in a y-protocols awareness update.
type awarenessEntry struct {
	client uint64
	// clock is the clock the entry stands at: the one a receiver compares what it holds of that
	// client against (awareness/awareness.go:566-581).
	clock uint64
	// raw is the entry as the update encodes it: the client's id, its clock and its state JSON.
	// It is nil for an entry the flush is to read from the room's awareness (resolve).
	raw []byte
	// removed is whether the state is null: the client left.
	removed bool
}

// decodeAwarenessEntries splits a y-protocols awareness update into its clients' entries, each
// a slice of update.
func decodeAwarenessEntries(update []byte) ([]awarenessEntry, error) {
	decoder := encoding.NewDecoder(update)
	count, err := decoder.ReadVarUint()
	if err != nil {
		return nil, err
	}
	entries := make([]awarenessEntry, 0, min(count, 1024))
	for range count {
		start := len(update) - decoder.Remaining()
		client, err := decoder.ReadVarUint()
		if err != nil {
			return nil, err
		}
		clock, err := decoder.ReadVarUint()
		if err != nil {
			return nil, err
		}
		state, err := decoder.ReadVarString()
		if err != nil {
			return nil, err
		}
		end := len(update) - decoder.Remaining()
		entries = append(entries, awarenessEntry{client: client, clock: clock, raw: update[start:end], removed: state == "null"})
	}
	return entries, nil
}

// encodeAwarenessEntries joins raw entries into one y-protocols awareness update.
func encodeAwarenessEntries(entries [][]byte) []byte {
	return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(uint64(len(entries)))
		for _, entry := range entries {
			encoder.WriteRaw(entry)
		}
	})
}

// varUintLen is how many bytes encoding.Encoder.WriteVarUint writes for v.
func varUintLen(v uint64) int {
	n := 1
	for ; v >= 0x80; v >>= 7 {
		n++
	}
	return n
}

// awarenessHost is the process the relay relays for: the rooms whose awareness it watches, the
// other processes it sends to, and the windows tests open in its paths. *Service is the host in
// production, where each of these is the service's own (Service.roomAwareness and the methods
// below it); the test that drives the relay through every interleaving of a room's comings and
// goings is a host of its own, which needs neither a database nor a ygo server
// (awareness_relay_schedule_test.go).
type awarenessHost interface {
	// roomAwareness is room's awareness, while room is resident on this process
	// (Server.GetAwareness).
	roomAwareness(room string) (*awareness.Awareness, bool)
	// injectAwareness applies another process's awareness update to this process's room of the
	// document (Server.Inject).
	injectAwareness(ctx context.Context, room string, update []byte) error
	// notifyAwareness sends one notification on documentAwarenessChannel.
	notifyAwareness(ctx context.Context, payload string) error
	// relaySender names this process in what it sends, which is what the other processes drop
	// their own notifications by (received).
	relaySender() string
	// hasLocalConnection reports whether a reader of this process is connected to room, or is
	// being admitted to it.
	hasLocalConnection(room string) bool
	// holdsRoomForReaders reports whether room is one another process's awareness is to be
	// applied to here.
	holdsRoomForReaders(room string) bool
	// coalesceAwareness runs a room's flush once the coalescing window has passed
	// (awarenessCoalesceWindow).
	coalesceAwareness(flush func())
	// awarenessWindows are the windows a test opens in the relay's paths.
	awarenessWindows() awarenessWindows
}

// awarenessWindows are the moments of the relay's own paths a test runs its work in, each one a
// moment another goroutine runs in production: the two within a change's own path, the one
// between the read an answer stands on and the record of it, and the two a flush is split at.
// Each is nil outside tests.
type awarenessWindows struct {
	// beforeWatch is given a room's awareness before the relay registers its own observer on it
	// (watchRoomAwareness). Tests use it to put an observer of their own ahead of the relay's,
	// since ygo fires a room's update observers in the order they were registered.
	beforeWatch func(room string, aw *awareness.Awareness)
	// beforeSnapshot runs between what the relay's observer read of its own lists under mu and
	// the read of the room that gives each of its entries a clock (touched): the window where
	// the list a removal is decided from and the room it is read against are two moments.
	beforeSnapshot func(room string, aw *awareness.Awareness, entries []awarenessEntry)
	// beforeRecord runs with what the relay's observer is to send of a change (touched) and
	// before it records it (record). Tests use it to announce a departing client again in that
	// window, so the return's own record runs first.
	beforeRecord func(room string, aw *awareness.Awareness, entries []awarenessEntry)
	// beforeAnswerRecord runs with the states the relay read of a room's local clients to answer
	// another process's query (answer) and before it records them (record). Tests use it to hold
	// that read open while the room moves on under it.
	beforeAnswerRecord func(room string, aw *awareness.Awareness, entries []awarenessEntry)
	// beforeResolve runs with the batch a flush has taken, before it reads the room for the
	// clients that batch holds a nil for (resolve): the window a change recorded after the batch
	// was taken, which the next batch carries, lands in.
	beforeResolve func(room string, batch map[uint64]awarenessEntry)
	// beforeRaise runs within that read, after the states are read and before the list is raised
	// to them under mu (resolve): the window the room's own paths move the list in.
	beforeRaise func(room string, batch map[uint64]awarenessEntry)
}

// awarenessRelay is the service's cluster.Relay (see the top of this file). It is per process:
// what it tracks is this process's rooms and the awareness clients connected to them here.
type awarenessRelay struct {
	host awarenessHost
	// mu guards every map below. It is taken ahead of a room's awareness lock, never behind it:
	// record reads the room under it (awarenessClientStanding), and nothing of this package can
	// hold ygo's awareness lock while it takes mu, since that lock is private to ygo and every
	// path that fires a room's observers - ApplyUpdate, SetLocalState, Heartbeat, RemoveExpired
	// - releases it first (awareness/awareness.go:726-732, 268-274, 309-312, 853-857), while
	// PurgeTombstones and Destroy fire none (awareness/awareness.go:883-889, 804-808). A ygo
	// that fired an observer under its own lock would invert the two; the test that catches it
	// is TestARoomsAwarenessFiresItsObserversWithItsLockReleased.
	mu sync.Mutex
	// pending is, per room, what the next flush is to carry for each client; a room is in it
	// from its first change until the batch the window then sends takes it, or until the last
	// entry left in it is dropped (resolve). A client's entry is the bytes to send - the
	// removal the room's observer encoded at the clock the client held - with a nil raw for a
	// client whose current state the flush is to read from the room's awareness (resolve), so
	// a reader that moves twice within the window is sent where it is now. Each entry carries
	// the clock it stands at, which is what keeps an entry recorded late from writing over a
	// later one of the same client (record).
	pending map[string]map[uint64]awarenessEntry
	// flushing is the rooms a flush is sending the batches of now, and whether another flush of
	// the same room stepped aside while it sent: a room's batches are taken and sent one flush
	// at a time, so what the other processes receive of a room stands in the order its own paths
	// recorded it (flush, takeBatch, nextBatch). A room has an entry here while a flush of it is
	// sending, and none at any other moment.
	flushing map[string]bool
	// inbound is, per room, the clients another process's notification is being applied to now,
	// and how many such injections name each: the room's observer, which ygo fires inside that
	// apply (received), leaves them to the process they came from rather than relaying them
	// back, as ygo's own relay observer leaves a change stamped with its injection sentinel
	// (provider/websocket/cluster.go:456-458). A client belongs to one process's socket, so a
	// mark never covers a change of this process's own.
	inbound map[string]map[uint64]int
	// local is, per room, the awareness clients of this process's own peers that the relay is
	// sending to the other processes, each with the clock its last state stood at: every client
	// the room's observer has seen a state of and not since a removal it relayed - a client the
	// snapshot a removal is decided from finds in the room again is relayed none and stays on
	// the list (touched) - less the ones another process's notification brought (inbound). The
	// room's sockets hold the list to maxAwarenessClientsPerSocket clients each
	// (awareness_limit.go).
	// It is what a query is answered with, what a close of a room removes (localRemoval), the
	// clock a removal the room's own observer relays stands at when the room's awareness holds
	// no later clock - a purge may have freed the departing client's tombstone by then
	// (touched) - and what every entry is settled against as it is recorded: a removal older
	// than the clock held here, and a state of a client this list does not hold or holds at a
	// later clock, are a reader's first state, a client that is in the room again, or an entry
	// of that client that arrived late, which the clocks alone cannot tell apart and record
	// settles by reading the room (record).
	// A flush raises it too: a client still listed stands here at the clock of the entry that
	// flush sends for it (resolve), which is where the room holds the client now and can be a
	// state no record of this list's has seen - a heartbeat applied while the batch waited. A
	// client it finds in the room again, with a removal of an earlier life of it still waiting
	// to be sent, goes back on the list at that same clock and the removal is dropped: the
	// state is what the other processes are to hold, and the list is what the client's next
	// departure builds its removal from (resolve, touched).
	local map[string]map[uint64]uint64
	// closed refuses every later send; Close sets it.
	closed bool
	// sends counts the batches and queries on their way to the sender, which Close waits for.
	sends sync.WaitGroup
	// oversized counts the client states dropped for being too large for a notification.
	oversized atomic.Uint64
}

func newAwarenessRelay(host awarenessHost) *awarenessRelay {
	return &awarenessRelay{
		host:     host,
		pending:  make(map[string]map[uint64]awarenessEntry),
		flushing: make(map[string]bool),
		inbound:  make(map[string]map[uint64]int),
		local:    make(map[string]map[uint64]uint64),
	}
}

var _ cluster.Relay = (*awarenessRelay)(nil)

// Publish is ygo's hand-off of a change a room of this process made itself, and nothing of it is
// sent: an edit crosses processes as a stored row, through the catch-up, and an awareness change
// reaches the relay from the room's own observer instead (watchRoomAwareness), which names every
// client the change touched where the lane behind Publish keeps one blob per room, the latest
// only (see the top of this file).
func (r *awarenessRelay) Publish(context.Context, cluster.Outbound) error {
	return nil
}

// Start is called once by Server.AttachRelay. What other processes send arrives through room
// sync's awareness lane (received), not through a subscription of the relay's own, so there is
// nothing to start.
func (r *awarenessRelay) Start(context.Context, cluster.Sink) error {
	return nil
}

// RoomActivated is ygo's word that room became resident here, once its load has finished. The
// room's awareness is given the relay's observer, which records what each change touched and
// frees a departed client's entry as the client leaves (watchRoomAwareness). A room a reader of
// this process is connecting to then queries the other processes for their readers' states; one
// only a write or a settlement loaded does not, since nothing here would show them.
func (r *awarenessRelay) RoomActivated(room string) {
	if aw, resident := r.host.roomAwareness(room); resident {
		r.watchRoomAwareness(room, aw)
	}
	if r.host.hasLocalConnection(room) {
		r.query(room)
	}
}

// RoomDeactivated is ygo's word that a room instance retired. Its clients' list goes with it,
// unless a successor instance is already resident under the name. Its awareness, the watcher
// registered on it included, goes with the instance itself.
func (r *awarenessRelay) RoomDeactivated(room string) {
	if _, resident := r.host.roomAwareness(room); resident {
		return
	}
	r.mu.Lock()
	delete(r.local, room)
	r.mu.Unlock()
}

// Close sends every batch still pending and waits for every send under way, then refuses any
// more. Service.Shutdown calls it after ygo's server has shut down, and before it closes the
// sender these sends go through.
func (r *awarenessRelay) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	rooms := slices.Collect(maps.Keys(r.pending))
	r.mu.Unlock()
	for _, room := range rooms {
		r.flush(room)
	}
	r.sends.Wait()
	return nil
}

// record puts each of entries into room's pending batch - a client's entry replacing any earlier
// one of its own, and a nil entry leaving the flush to read that client's current state
// (resolve) - and arms the batch's send when it is the room's first. It keeps the room's list of
// the clients it relays (local) as it goes: a client's state puts it on the list at its own
// clock, or at the clock the room holds that client at where the list cannot place the entry
// (below); a removal takes it off and is relayed like any other entry.
// What one batch can hold is what the room's sockets may announce: a change touches the clients
// of the frame that made it, and a socket's frame touches no more than
// maxAwarenessClientsPerSocket of them (awareness_limit.go), so a tab inventing thousands
// reaches the relay as its own handful and the readers who join after it are relayed at once.
//
// Entries reach it by two paths: a change of the room's own (touched) and the read behind an
// answer to another process's query (answer). An answer carries states and nothing else - every
// departure is the room's own observer's to record, and an answer leaves the nulls its read
// found out of what it hands here (answer) - so what settles an entry is what the entry is, not
// the path it came on.
//
// Deciding what to send and recording it are two moments on both paths, and what the room's
// other paths did in between is recorded first. An entry the list does not place - a removal
// older than the clock the list holds for its client, a state of a client the list does not
// hold or holds at a later clock - is one of three things, and its own clock does not say
// which: a reader's first state, which is the commonest of the three and the one a room where
// nothing races meets; the client left and is in the room again at a later clock; or this entry
// is a read of that client that arrived late. So such an entry is settled against the room
// itself - aw, the room's awareness, read here under mu (awarenessClientStanding), one read to
// an entry:
//
//   - a removal of a client the room holds live is dropped whole, neither the list nor the
//     batch touched for it: the client's own later state is already recorded, and a removal
//     over it would take the client off the list this process relays and answers queries from
//     and replace the client's own entry in the batch, so every other process would drop the
//     cursor of a reader who is in the room.
//   - a removal of a client the room does not hold stands at the clock the list holds, the
//     latest state this process relayed of that client. At its own older clock it would be
//     dropped by every process that took the later state, and the departed reader's cursor
//     would sit in their rooms until it expired (awarenessExpiry, 30 s); at the list's clock
//     every process takes it, since none holds a later state of a client this one has relayed
//     nothing later of (awareness/awareness.go:566-581).
//   - a state of a client the room holds live goes on the list at the clock the room holds it
//     at, never below the clock the list already holds, and puts a nil in the batch for the
//     flush to read (resolve). The client is in the room, so a removal pending for it must not
//     be sent, for the reason a removal settled live is dropped.
//   - a state of a client the room does not hold is dropped whole, neither the list nor the
//     batch touched: the client left between the entry being decided and this read, and the
//     removal the room's own observer makes for it - which takes it off the list, and stands
//     in the batch at the clock that path settled it at - is what the other processes are to
//     be sent. Recorded as it stands, the state would put a client that is gone back on the
//     list this process answers queries from and leave a nil over that removal in the batch,
//     and the flush drops a nil of a client the room no longer holds (resolve): the removal
//     would reach no other process at all, and the departed reader's cursor would sit in their
//     rooms until it expired.
//
// A client the list has never held - a reader whose first state this is - is settled by that
// same read: listed at the clock the room holds it at, or dropped whole when the client has
// already left.
//
// A removal at the very clock the list holds is applied as it stands: it is that state's own
// removal, since a client's return always stands at a strictly higher clock than the tombstone
// it replaces - ygo drops a state whose clock equals what it holds and takes a null at that
// clock as the removal of it (awareness/awareness.go:566-581) - and it is the clock every other
// process takes the removal at for the same reason. A removal of a client the list does not
// hold has no clock of this process's own to stand at and stands as the room's own observer
// built it, at the clock that path's snapshot of the client gave (touched) - unless the batch
// holds a later entry of that client, which is a removal the room's own path settled after this
// one and recorded before it, and which stands. Two of a client's removals are recorded out of
// their order where the client left, came back and left again within one window, each
// departure's snapshot taken before the record of the one ahead of it (touched); the older of
// them, written over the later, would reach the other processes as the client's last word, and
// every one of them holding the client at the later clock would drop it and keep a cursor whose
// reader has left until it expired (awarenessExpiry, 30 s). A state's own entry needs no such
// guard: it is recorded where the room holds the client live, and a client live again stands at
// a strictly higher clock than any removal of it, its clock being its own counter.
// A state at the clock the list holds, or past it, is where the client stands now: it goes on
// the list at its own clock and into the batch as it came - a nil from the room's own change,
// for the flush to read, and the state itself from an answer's read.
func (r *awarenessRelay) record(room string, aw *awareness.Awareness, entries []awarenessEntry) {
	if len(entries) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	batch, armed := r.pending[room]
	if !armed {
		batch = make(map[uint64]awarenessEntry, min(len(entries), maxAwarenessClientsPerRoom))
	}
	clients := r.local[room]
	for _, entry := range entries {
		held, listed := clients[entry.client]
		if entry.removed {
			if listed && entry.clock < held {
				if _, live := awarenessClientStanding(aw, entry.client); live {
					continue
				}
				entry.clock, entry.raw = held, awarenessRemoval(entry.client, held)
			}
			delete(clients, entry.client)
			if carried, pending := batch[entry.client]; pending && carried.clock > entry.clock {
				continue
			}
			batch[entry.client] = entry
			continue
		}
		clock, state := entry.clock, entry.raw
		if !listed || clock < held {
			standing, live := awarenessClientStanding(aw, entry.client)
			if !live {
				continue
			}
			clock, state = max(standing, held), nil
		}
		if clients == nil {
			clients = make(map[uint64]uint64)
			r.local[room] = clients
		}
		clients[entry.client] = clock
		batch[entry.client] = awarenessEntry{client: entry.client, clock: clock, raw: state}
	}
	if len(clients) == 0 {
		delete(r.local, room)
	}
	if len(batch) == 0 || armed {
		return
	}
	r.pending[room] = batch
	r.sends.Add(1)
	r.host.coalesceAwareness(func() {
		defer r.sends.Done()
		r.flush(room)
	})
}

// awarenessClientStanding is what aw, a room's awareness, holds of client now: the clock of the
// client's state and whether it holds a state of it at all - the one read that settles an entry
// record cannot place by its clock alone. It reads the client's state and that state's clock
// together, under the awareness's own lock, as every read of a client's standing does
// (EncodeUpdate, awareness/awareness.go:458-480) - GetStates and Meta are two locks and so two
// moments. A snapshot that does not decode - ygo's own encoding of what it holds, so nothing
// short of a bug - reads as a client the room does not hold, which sends the removal being
// settled rather than dropping it, and drops a state the list cannot place rather than listing its
// client: a cursor the relay takes away a moment early is put back by that client's own next
// frame, where one it never takes away stays until it expires (awarenessExpiry, 30 s).
func awarenessClientStanding(aw *awareness.Awareness, client uint64) (uint64, bool) {
	entries, err := decodeAwarenessEntries(aw.EncodeUpdate([]uint64{client}))
	if err != nil || len(entries) != 1 {
		slog.Debug("dispatch: read what a room's awareness holds of one client",
			"client", client, "entries", len(entries), "error", err)
		return 0, false
	}
	return entries[0].clock, !entries[0].removed
}

// flush sends room's pending batch, if any is still pending, in as many notifications as its size
// takes (awarenessNotifications), each client at the state the room holds now (resolve).
//
// One flush of a room sends at a time, and each takes the room's batch as its turn comes
// (takeBatch), so what the other processes receive of a room stands in the order the room's own
// paths recorded it. Two of a room's flushes are under way together where the first is still
// sending as the second's window ends - a notification waiting its turn on the process's sender
// (roomSyncSender), or on a connection it had to open again - and the second, left to take the
// batch the first did not, would send a reader's removal ahead of the state the first read
// before that reader left. Every other process would take the removal, then take the older
// state as the first word of a client it has forgotten (reclaimAwarenessSlots), and the cursor
// of a reader who has left would sit in their rooms until it expired (awarenessExpiry, 30 s).
// So the second steps aside and leaves its batch to the first, which takes it as soon as it has
// sent its own (nextBatch): the batch waits for the flush ahead of it and for nothing else,
// since its own window has already passed.
func (r *awarenessRelay) flush(room string) {
	for batch := r.takeBatch(room); batch != nil; batch = r.nextBatch(room) {
		if ahead := r.host.awarenessWindows().beforeResolve; ahead != nil {
			ahead(room, batch)
		}
		r.resolve(room, batch)
		if len(batch) == 0 {
			continue
		}
		r.send(room, batch)
	}
}

// takeBatch is the batch a flush of room is to send: what is pending, or nil where nothing is -
// and nil where another flush of that room is sending, which is left to take this flush's batch
// too (nextBatch).
func (r *awarenessRelay) takeBatch(room string) map[uint64]awarenessEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, sending := r.flushing[room]; sending {
		r.flushing[room] = true
		return nil
	}
	batch := r.pending[room]
	if len(batch) == 0 {
		return nil
	}
	delete(r.pending, room)
	r.flushing[room] = false
	return batch
}

// nextBatch is what a flush is to send after the batch it has just sent: what is pending, where
// another flush of the room stepped aside while this one sent, and nil otherwise - which gives
// the room's turn back. A room whose next change is recorded after that arms a window of its own
// (record), as the first change of any room does.
func (r *awarenessRelay) nextBatch(room string) map[uint64]awarenessEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	batch := r.pending[room]
	if !r.flushing[room] || len(batch) == 0 {
		delete(r.flushing, room)
		return nil
	}
	delete(r.pending, room)
	r.flushing[room] = false
	return batch
}

// send relays batch, room's resolved client entries, to the other processes: as many
// notifications as its size takes, each within the payload limit (awarenessNotifications). A
// send that fails takes the ones behind it with it, since the batch they share is one moment of
// the room.
func (r *awarenessRelay) send(room string, batch map[uint64]awarenessEntry) {
	payloads := r.awarenessNotifications(room, batch)
	ctx, cancel := context.WithTimeout(context.Background(), roomSyncSendTimeout)
	defer cancel()
	for _, payload := range payloads {
		if err := r.host.notifyAwareness(ctx, payload); err != nil {
			slog.Warn("dispatch: relay a document room's awareness to the other processes", "room", room, "error", err)
			return
		}
	}
}

// resolve fills in what batch left for the flush to read: the entry of every client it holds a
// nil for, as room's awareness holds that client now, so a reader that moved several times
// within the window is sent where it is rather than where it was when the change was recorded.
// A client the room no longer holds is dropped - encoding one ygo has forgotten would name it at
// a clock of 0, which every other process drops as older than the state it holds - and dropping
// it stands in no removal's way: a nil is left for a client the room held as the entry was
// recorded - read live where the list could not place the entry, and listed where it could,
// which is a client that is in the room or one whose removal, not yet recorded, follows it
// (record) - and the removal the room's own observer makes for a client that leaves after that
// replaces the nil in this batch when it is recorded before the flush takes it, and stands in
// the next batch when it is recorded after.
//
// What it sends it raises the list to (local): a client still listed stands there at the clock
// of the entry sent for it, never below the clock the list already holds, and one the list no
// longer holds is put back only where the room holds it live and a removal of it is waiting
// (below). Without the raise the list would stand below the state the other processes hold -
// the entry sent here is read from the room, which a heartbeat recorded after this read has
// already moved on - and the removal that client's departure builds from the list, when a
// purge has freed its tombstone before the relay reads it (touched), would be older than that
// state: every other process drops it (awareness/awareness.go:566-581) and the departed
// reader's cursor sits in their rooms until it expires (awarenessExpiry, 30 s).
//
// The read and the raise are two moments, and the room's own paths move the list in between: a
// removal recorded there takes a client off it, a later state puts it on at a later clock, and
// neither is undone here. The states are read first and mu taken after, which is the order
// every path that takes both takes them in (record).
//
// A client the list no longer holds, with a removal waiting in the room's next batch below the
// state being sent here, is one of two, and the room says which - read here under mu, one read
// to a client (awarenessClientStanding), as record reads it:
//
//   - the room holds nothing of the client: it left, in that window or before this flush took
//     its batch, and the removal waiting for it stands at the clock the list held when it was
//     recorded, which is below the state being sent. It is raised to the clock of the entry
//     sent here, where it stands as the removal of that very state: every other process takes
//     a null at the clock it holds the client at (awareness/awareness.go:566-581), and at the
//     lower clock it would take none of them and keep the cursor of a reader who has left.
//   - the room holds the client live: it is in the room again, and the waiting removal is of
//     the life that ended before the state being sent - the return stands at a strictly higher
//     clock than the removal it replaces, and the state read here is no lower than the return.
//     Raised, it would reach every other process as the removal of the very state sent just
//     ahead of it and take a reader who is in the room out of their rooms: theirs recover, since
//     each frees a departed client's entry after every apply (reclaimAwarenessSlots) and takes
//     the client's next state whatever its clock, but the browsers reading those rooms hold the
//     null until that reader moves again (y-protocols), and the cursor of a reader who is in
//     the room is gone from them until then. So it is dropped whole: at its own clock no
//     process would take it, every one of them holding the client at the state sent here or
//     later, and the client's own next departure is what removes it.
//
// The client whose removal is dropped goes back on the list at the clock sent, as it does
// where the list placed the entry: the removal that client's next departure builds stands at
// the clock the list holds (touched), and off the list it would stand at the clock the room's
// tombstone gives, which is 0 where a purge has freed it - older than the state sent here, so
// every other process would drop it, and a removal recorded over the later entry this one
// left in the batch stands aside (record).
//
// The next batch is where such a removal is, since one flush of a room sends at a time (flush)
// and a record puts it nowhere else. The room's pending batch goes with the last entry dropped
// from it: a batch left empty would take the window already armed for it and leave the entries
// recorded after that one with none (record, takeBatch).
func (r *awarenessRelay) resolve(room string, batch map[uint64]awarenessEntry) {
	unread := make([]uint64, 0, len(batch))
	for client, entry := range batch {
		if entry.raw == nil {
			unread = append(unread, client)
		}
	}
	if len(unread) == 0 {
		return
	}
	aw, resident := r.host.roomAwareness(room)
	if !resident {
		for _, client := range unread {
			delete(batch, client)
		}
		return
	}
	states := aw.GetStates()
	held := unread[:0]
	for _, client := range unread {
		if _, holds := states[client]; holds {
			held = append(held, client)
			continue
		}
		delete(batch, client)
	}
	if len(held) == 0 {
		return
	}
	entries, err := decodeAwarenessEntries(aw.EncodeUpdate(held))
	if err != nil {
		slog.Debug("dispatch: drop the awareness states of a room the relay cannot read", "room", room, "error", err)
		for _, client := range held {
			delete(batch, client)
		}
		return
	}
	if ahead := r.host.awarenessWindows().beforeRaise; ahead != nil {
		ahead(room, batch)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	clients := r.local[room]
	waiting := r.pending[room]
	for _, entry := range entries {
		batch[entry.client] = entry
		if stood, listed := clients[entry.client]; listed {
			if entry.clock > stood {
				clients[entry.client] = entry.clock
			}
			continue
		}
		gone, recorded := waiting[entry.client]
		if !recorded || !gone.removed || gone.clock >= entry.clock {
			continue
		}
		if _, live := awarenessClientStanding(aw, entry.client); live {
			delete(waiting, entry.client)
			if len(waiting) == 0 {
				delete(r.pending, room)
			}
			if clients == nil {
				clients = make(map[uint64]uint64)
				r.local[room] = clients
			}
			clients[entry.client] = entry.clock
			continue
		}
		waiting[entry.client] = awarenessEntry{
			client:  entry.client,
			clock:   entry.clock,
			raw:     awarenessRemoval(entry.client, entry.clock),
			removed: true,
		}
	}
}

// awarenessNotifications packs batch, room's client entries, into the fewest notification payloads
// each within roomSyncPayloadLimit, whole clients to each, in client order. A client whose entry
// alone does not fit is dropped and counted.
func (r *awarenessRelay) awarenessNotifications(room string, batch map[uint64]awarenessEntry) []string {
	framing, err := json.Marshal(awarenessMessage{Sender: r.host.relaySender(), Room: room, Data: "-"})
	if err != nil {
		slog.Error("dispatch: frame a document room's awareness notification", "room", room, "error", err)
		return nil
	}
	// budget is what the base64 of one update may take: the payload less its framing, whose
	// placeholder stands for that base64. Base64's alphabet needs no JSON escape.
	budget := roomSyncPayloadLimit - (len(framing) - 1)
	fits := func(entries, size int) bool {
		return base64.StdEncoding.EncodedLen(varUintLen(uint64(entries))+size) <= budget
	}
	clients := slices.Sorted(maps.Keys(batch))
	var (
		payloads []string
		group    [][]byte
		size     int
	)
	emit := func() {
		if len(group) == 0 {
			return
		}
		update := encodeAwarenessEntries(group)
		payload, err := json.Marshal(awarenessMessage{Sender: r.host.relaySender(), Room: room, Data: base64.StdEncoding.EncodeToString(update)})
		if err != nil {
			slog.Error("dispatch: encode a document room's awareness notification", "room", room, "error", err)
		} else {
			payloads = append(payloads, string(payload))
		}
		group, size = nil, 0
	}
	for _, client := range clients {
		entry := batch[client].raw
		if !fits(1, len(entry)) {
			dropped := r.oversized.Add(1)
			slog.Warn("dispatch: drop an awareness state too large to relay to the other processes",
				"room", room, "client", client, "bytes", len(entry), "limit", roomSyncPayloadLimit, "dropped_total", dropped)
			continue
		}
		if !fits(len(group)+1, size+len(entry)) {
			emit()
		}
		group = append(group, entry)
		size += len(entry)
	}
	emit()
	return payloads
}

// query asks every other process holding room for its local clients' awareness states, on a
// goroutine of its own: its callers run inside ygo's room load and a socket's admission.
func (r *awarenessRelay) query(room string) {
	payload, err := json.Marshal(awarenessMessage{Sender: r.host.relaySender(), Room: room, Query: true})
	if err != nil {
		slog.Error("dispatch: encode a document room's awareness query", "room", room, "error", err)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.sends.Add(1)
	go func() {
		defer r.sends.Done()
		ctx, cancel := context.WithTimeout(context.Background(), roomSyncSendTimeout)
		defer cancel()
		if err := r.host.notifyAwareness(ctx, string(payload)); err != nil {
			slog.Warn("dispatch: ask the other processes for a document room's awareness", "room", room, "error", err)
		}
	}()
}

// localClients is which of room's local clients aw, the room's awareness, holds a state of now,
// with that state's clock.
func (r *awarenessRelay) localClients(room string, aw *awareness.Awareness) map[uint64]uint64 {
	r.mu.Lock()
	listed := slices.Collect(maps.Keys(r.local[room]))
	r.mu.Unlock()
	if len(listed) == 0 {
		return nil
	}
	states := aw.GetStates()
	clients := make(map[uint64]uint64, len(listed))
	for _, client := range listed {
		if state, ok := states[client]; ok {
			clients[client] = state.Clock
		}
	}
	return clients
}

// awarenessRemoval is the entry of an awareness update that takes client away at clock: a null
// state, which every receiver holding that client at clock or below takes and the client's own
// next announcement, one clock later, undoes (awareness/awareness.go:566-581). The clock is the
// client's own, unbumped, as ygo's removal for a peer that disconnects stands at
// (provider/websocket/peer.go, encodeAwarenessRemoval): bumped, it would be the clock the
// rejoining client's next announcement carries, and one of the two would be dropped.
func awarenessRemoval(client, clock uint64) []byte {
	return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(client)
		encoder.WriteVarUint(clock)
		encoder.WriteVarString("null")
	})
}

// localRemoval is the awareness update that removes room's local clients from aw, the room's
// awareness, or nil when it holds none. Each removal stands at its client's current clock
// (awarenessRemoval).
func (r *awarenessRelay) localRemoval(room string, aw *awareness.Awareness) []byte {
	clients := r.localClients(room, aw)
	if len(clients) == 0 {
		return nil
	}
	entries := make([][]byte, 0, len(clients))
	for _, client := range slices.Sorted(maps.Keys(clients)) {
		entries = append(entries, awarenessRemoval(client, clients[client]))
	}
	return encodeAwarenessEntries(entries)
}

// received is documentAwarenessChannel's handler, on room sync's awareness lane. This process's own
// notifications are dropped. A query is answered (answer). A state update is applied to this
// process's room of the document only while that room is resident or loading and a reader of
// this process is connected to it (Service.holdsRoomForReaders): ygo's Inject loads an absent room
// and restamps the idle clock of a room no peer is in, so applying every relayed heartbeat would
// load the document on every process and keep a room nobody here reads resident for good.
//
// What a notification carries is injected as it stands: the inbound side counts no clients of
// its own. What the other processes together cost a room here is their own sockets' bound, since
// each cut a peer's frame to maxAwarenessClientsPerSocket before relaying it
// (awareness_limit.go). The clients it names are marked as the room's for the apply (mark), so
// the room's observer, which ygo fires inside it, leaves them to the process they came from
// rather than relaying them back. The entries of the clients that left are freed once the update
// has landed (reclaimAwarenessSlots), because a removal of a client this process never held
// leaves a tombstone ygo tells no observer about (awareness/awareness.go:669-686), so the
// observer a room's own departures run through never sees it, and a run of them - every reader
// of a process that shut down, say - would otherwise sit in the room until a purge two expiries
// later.
func (r *awarenessRelay) received(ctx context.Context, payload string) {
	var message awarenessMessage
	if err := json.Unmarshal([]byte(payload), &message); err != nil || message.Room == "" {
		slog.Debug("dispatch: drop an awareness notification that does not parse", "error", err)
		return
	}
	if message.Sender == r.host.relaySender() {
		return
	}
	if message.Query {
		r.answer(message.Room)
		return
	}
	update, err := base64.StdEncoding.DecodeString(message.Data)
	if err != nil {
		slog.Debug("dispatch: drop an awareness notification that does not decode", "room", message.Room, "error", err)
		return
	}
	if !r.host.holdsRoomForReaders(message.Room) {
		return
	}
	entries, err := decodeAwarenessEntries(update)
	if err != nil {
		slog.Debug("dispatch: drop an awareness notification the relay cannot read", "room", message.Room, "error", err)
		return
	}
	defer r.mark(message.Room, entries, -1)
	r.mark(message.Room, entries, 1)
	// The room can retire between that check and Inject - its last reader left and the idle sweep
	// took it, or a close - and then Inject loads it again for this one update. That room has no
	// peer, so it idles out after roomIdleTimeout like any room a write loads; the window is the
	// few microseconds between the two calls, against a sweep that waits a minute.
	err = r.host.injectAwareness(ctx, message.Room, update)
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Debug("dispatch: apply another process's awareness to a document room", "room", message.Room, "error", err)
	}
	if aw, resident := r.host.roomAwareness(message.Room); resident {
		reclaimAwarenessSlots(message.Room, aw)
	}
}

// answer sends, for another process's query, the states of room's local clients, when room is
// resident here and has any: each entry as it stands, which puts it in the room's pending batch
// and refreshes the clock the relay holds for that client (record). It answers with states and
// nothing else - a client its read finds a tombstone of, or nothing at all
// (awareness/awareness.go:477-480), is left out of what it records: an answer relays no
// departure of its own, since every departure is the room's own observer's to record (touched),
// and a null of this read would stand in that removal's place in the batch at the clock this
// read took it at rather than the one the room's own path settles it at.
//
// Reading the room and recording what it read are two moments, and an answer answers for the
// clients the list held when it read them and for nothing else: a client the room moved on from
// in between - taken off the list by a removal recorded there, or on it at a later clock - is
// settled against the room itself rather than recorded at the clock this read stands at, so a
// reader who has left is neither relisted nor left in the other processes' rooms (record).
func (r *awarenessRelay) answer(room string) {
	aw, resident := r.host.roomAwareness(room)
	if !resident {
		return
	}
	clients := r.localClients(room, aw)
	if len(clients) == 0 {
		return
	}
	entries, err := decodeAwarenessEntries(aw.EncodeUpdate(slices.Collect(maps.Keys(clients))))
	if err != nil {
		slog.Debug("dispatch: drop an awareness update the relay cannot read", "room", room, "error", err)
		return
	}
	entries = slices.DeleteFunc(entries, func(entry awarenessEntry) bool { return entry.removed })
	if len(entries) == 0 {
		return
	}
	if ahead := r.host.awarenessWindows().beforeAnswerRecord; ahead != nil {
		ahead(room, aw, entries)
	}
	r.record(room, aw, entries)
}

// watchRoomAwareness has aw, a room's awareness, tell the relay what each of the room's changes
// touched, and free a departed client's entry as the client leaves. ygo fires a room's awareness
// observers with the clients each change added, updated and removed - a peer's frame, a
// heartbeat, a peer's disconnect, an expiry, another process's relayed update - so the relay
// hears of every client a change touched where the lane behind Publish would hand it only the
// latest change's (see the top of this file). The entry a removal leaves behind is reclaimed
// there and then (reclaimAwarenessSlots), so a room of readers that come and go costs no more
// than the readers in it now; what the removal is relayed at is the clock the relay recorded for
// the client (touched), which no purge can take away.
//
// It watches updates rather than changes, so a heartbeat - the same state at a later clock, which
// fires no change observer - is relayed as the presence it is. The observer is dropped with the
// room instance's awareness itself; a successor instance under the same name is a new awareness,
// and RoomActivated gives it its own.
func (r *awarenessRelay) watchRoomAwareness(room string, aw *awareness.Awareness) {
	if ahead := r.host.awarenessWindows().beforeWatch; ahead != nil {
		ahead(room, aw)
	}
	aw.OnUpdate(func(event awareness.UpdateEvent) {
		entries := r.touched(room, aw, event)
		if ahead := r.host.awarenessWindows().beforeRecord; ahead != nil {
			ahead(room, aw, entries)
		}
		r.record(room, aw, entries)
		if len(event.Removed) > 0 {
			reclaimAwarenessSlots(room, aw)
		}
	})
}

// touched is what the relay is to send of event, the change room's awareness has just made: an
// entry each of the clients it added or updated, left for the flush to read their state as it
// stands then (resolve), and the removal of each client it took away and the room does not hold
// again, encoded here. A client another process's notification is being applied for is left to
// the process it came from (inbound, received).
//
// Each removal is decided from one snapshot of its client - the client's state and its clock
// read together, under the awareness's own lock - and never from one read of which clients are
// live followed by another of the clock. ygo keeps that lock private (awareness/awareness.go:135)
// and GetStates (awareness/awareness.go:344) and Meta (awareness/awareness.go:361) each take it
// for themselves, so two reads are two moments: a client returning between them is gone to the
// first read, which builds a removal, and live to the second, which gives that removal the clock
// of the state the client is live at - the one clock every other process takes it at. EncodeUpdate
// takes the lock once for the clients it is asked for (awareness/awareness.go:458), and what it
// writes for one of them says which of three that client is:
//
//   - a state that is not null: the client is in the room again - its own tab's frame having
//     landed between the change and this read - and no removal is sent at all. Its own entry
//     carries its state, and it stays on the list of clients this process relays and answers
//     queries from (local, record), which only a relayed removal takes it off. A removal here
//     would stand at the clock of the state the client is live at; every other process would
//     take it, since it is no older than what it holds (awareness/awareness.go:566-581), and
//     the cursor of a reader who is in the room would go from all of them.
//   - a null state at the tombstone's clock: the client is gone, and its removal stands at that
//     clock or at the one the relay holds for it (local), whichever is the later.
//   - a null state at a clock of 0, which is what EncodeUpdate writes for a client the room
//     holds nothing of (awareness/awareness.go:477-480): ygo fires its update observers with
//     the awareness unlocked, and a purge of a tombstone this one's age (awarenessTombstoneAge,
//     one nanosecond) runs after every inbound apply (received) and after every other removal
//     of the room's own, so one can land between the change and this read. The removal then
//     stands at the clock the relay holds for the client, which no purge takes away; sent at 0
//     it would be dropped by every other process as older than the state it holds, and the
//     departed reader's cursor would sit in their rooms until it expired, thirty seconds later
//     (awarenessExpiry).
//
// The last two are one rule, since a client the room holds nothing of reads as a clock of 0:
// a removal stands at the later of the snapshot's clock and the relay's own.
//
// The snapshot is still one moment, and a client can return after it: what this hands back is
// settled against the room once more as it is recorded (record).
func (r *awarenessRelay) touched(room string, aw *awareness.Awareness, event awareness.UpdateEvent) []awarenessEntry {
	entries := make([]awarenessEntry, 0, len(event.Added)+len(event.Updated)+len(event.Removed))
	left := make([]awarenessEntry, 0, len(event.Removed))
	r.mu.Lock()
	inbound := r.inbound[room]
	relayed := r.local[room]
	for _, changed := range [][]uint64{event.Added, event.Updated} {
		for _, client := range changed {
			if inbound[client] == 0 {
				entries = append(entries, awarenessEntry{client: client})
			}
		}
	}
	for _, client := range event.Removed {
		if inbound[client] == 0 {
			left = append(left, awarenessEntry{client: client, clock: relayed[client], removed: true})
		}
	}
	r.mu.Unlock()
	if ahead := r.host.awarenessWindows().beforeSnapshot; ahead != nil {
		ahead(room, aw, entries)
	}
	for i, entry := range entries {
		// The clock the room holds now, which is the one the flush will read its state at.
		if meta, known := aw.Meta(entry.client); known {
			entries[i].clock = meta.Clock
		}
	}
	if len(left) == 0 {
		return entries
	}
	departed := make([]uint64, len(left))
	for i, entry := range left {
		departed[i] = entry.client
	}
	// The one snapshot every removal below is decided from. A snapshot that does not decode -
	// which is ygo's own encoding of what it holds, so nothing short of a bug - leaves each
	// removal at the clock the relay holds for its client, the clock the client last stood at
	// here.
	snapshot, err := decodeAwarenessEntries(aw.EncodeUpdate(departed))
	if err != nil {
		slog.Debug("dispatch: read the awareness of a room's departed clients", "room", room, "error", err)
	}
	read := make(map[uint64]awarenessEntry, len(snapshot))
	for _, entry := range snapshot {
		read[entry.client] = entry
	}
	for _, gone := range left {
		if entry, found := read[gone.client]; found {
			if !entry.removed {
				continue
			}
			if entry.clock > gone.clock {
				gone.clock = entry.clock
			}
		}
		gone.raw = awarenessRemoval(gone.client, gone.clock)
		entries = append(entries, gone)
	}
	return entries
}

// mark counts the clients entries names as ones another process's notification is applying to
// room, by delta: 1 while the apply runs and -1 once it has returned (received). The count is
// the injections naming that client at once, so one ending leaves another's mark standing.
func (r *awarenessRelay) mark(room string, entries []awarenessEntry, delta int) {
	if len(entries) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	clients := r.inbound[room]
	if clients == nil {
		if delta < 0 {
			return
		}
		clients = make(map[uint64]int, len(entries))
		r.inbound[room] = clients
	}
	for _, entry := range entries {
		if count := clients[entry.client] + delta; count > 0 {
			clients[entry.client] = count
		} else {
			delete(clients, entry.client)
		}
	}
	if len(clients) == 0 {
		delete(r.inbound, room)
	}
}

// reclaimAwarenessSlots frees the entries of room's departed clients - every tombstone its
// awareness holds. ygo keeps a removed client's entry for two expiries, a minute and a half
// here, so that a state of that client still in flight when it left is dropped rather than
// taken for a new one (awareness/awareness.go:861-882). A room of this service keeps it only
// until the room's next change: a tab that opens and closes a document costs its process an
// entry for a minute and a half otherwise, on every process its cursor reached, and the
// cursors of a room nobody is in are memory and nothing else.
// What the shorter life gives up is that state of a departed client arriving after its removal
// is taken for a new client, and shows that cursor until it expires (awarenessExpiry); the
// relay's own order makes it rare, since a room's changes are coalesced and sent in order and a
// removal takes its client off the list a query is answered from (localClients) before the send
// that carries it.
func reclaimAwarenessSlots(room string, aw *awareness.Awareness) {
	if freed := aw.PurgeTombstones(awarenessTombstoneAge); freed > 0 {
		slog.Debug("dispatch: free the entries of a document room's departed awareness clients",
			"room", room, "freed", freed)
	}
}

// hasLocalConnection reports whether a reader of this process is connected to room, or is being
// admitted to it (authorize registers a connection before ygo joins it to the room).
func (s *Service) hasLocalConnection(room string) bool {
	state := s.lockExistingState(room)
	if state == nil {
		return false
	}
	defer s.unlockState(room, state)
	return len(state.connected) > 0
}

// holdsRoomForReaders reports whether room is resident here, or loading, with a reader of this
// process connected to it or being admitted: the rooms another process's awareness is applied to
// (awarenessRelay.received). A loading room counts, because a reader opening a document cold is
// what loads it, and that load's query (RoomActivated) is answered while the load can still be
// finishing.
func (s *Service) holdsRoomForReaders(room string) bool {
	return s.hasLocalConnection(room) && s.roomHeld(room)
}

// The service is the relay's host in production (awarenessHost): the rooms the relay watches are
// the ones resident on this process, what it sends goes out on this process's NOTIFY connection
// under this process's id, and what the other processes send is applied to this process's rooms.
var _ awarenessHost = (*Service)(nil)

// roomAwareness is the awareness of this process's room of a document, while the room is
// resident.
func (s *Service) roomAwareness(room string) (*awareness.Awareness, bool) {
	return s.srv.GetAwareness(room)
}

// injectAwareness applies another process's awareness update to this process's room of a
// document, which ygo hands to that room's peers.
func (s *Service) injectAwareness(ctx context.Context, room string, update []byte) error {
	return s.srv.Inject(ctx, cluster.Inbound{Room: room, Kind: cluster.KindAwareness, Data: update})
}

// notifyAwareness sends one awareness notification to the other processes, on
// documentAwarenessChannel through this process's NOTIFY connection.
func (s *Service) notifyAwareness(ctx context.Context, payload string) error {
	return s.roomSyncSender.Notify(ctx, documentAwarenessChannel, payload)
}

// relaySender names this process in what its relay sends (Service.processID).
func (s *Service) relaySender() string {
	return s.processID
}

// coalesceAwareness runs a room's flush once the coalescing window has passed, on a timer of the
// runtime's: the window is what keeps a reader moving at typing speed to one notification per
// window (awarenessCoalesceWindow).
func (s *Service) coalesceAwareness(flush func()) {
	time.AfterFunc(awarenessCoalesceWindow, flush)
}

// awarenessWindows are the windows the service's own tests open in the relay's paths, each kept
// as a field of the service and nil outside tests. The two a flush is split at have no field
// here: the test that opens them is its own host (awareness_relay_schedule_test.go), since a
// flush of a whole process runs on a timer no test holds.
func (s *Service) awarenessWindows() awarenessWindows {
	return awarenessWindows{
		beforeWatch:        s.beforeAwarenessWatch,
		beforeRecord:       s.beforeAwarenessRecord,
		beforeAnswerRecord: s.beforeAwarenessAnswerRecord,
	}
}

// removeLocalAwareness removes the awareness states of room's local clients before this process
// closes the room itself - its stored history replaced, its issue closed, a failed room's
// eviction, a quiesce, a shutdown - through every close the service makes (roomServer). ygo
// removes a peer's states as it disconnects, but a close disconnects the room's peers and tears
// its relay down together, and a peer's removal that comes after the teardown would reach no
// other process, whose readers would keep the cursor until it expired (awarenessExpiry). So the
// removal is applied to the room here, while the room is still the server's: the room's own
// observer records it as the apply runs (watchRoomAwareness), and the relay sends it within its
// window whatever the close does next.
func (s *Service) removeLocalAwareness(room string) {
	aw, resident := s.srv.GetAwareness(room)
	if !resident {
		return
	}
	removal := s.awarenessRelay.localRemoval(room, aw)
	if removal == nil {
		return
	}
	if err := aw.ApplyUpdate(removal, nil); err != nil {
		slog.Warn("dispatch: remove a closing document room's awareness", "room", room, "error", err)
	}
}
