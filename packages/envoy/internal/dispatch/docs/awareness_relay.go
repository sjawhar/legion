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
	// connection this process admits, all of them on one document, each announcing four
	// clients where a browser tab announces one, and a process refuses the socket after that
	// whatever its rooms hold (Service.canAddConnection). Relayed clients reach it no sooner:
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
	// raw is the entry as the update encodes it: the client's id, its clock and its state JSON.
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
		if _, err := decoder.ReadVarUint(); err != nil {
			return nil, err
		}
		state, err := decoder.ReadVarString()
		if err != nil {
			return nil, err
		}
		end := len(update) - decoder.Remaining()
		entries = append(entries, awarenessEntry{client: client, raw: update[start:end], removed: state == "null"})
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

// awarenessRelay is the service's cluster.Relay (see the top of this file). It is per process:
// what it tracks is this process's rooms and the awareness clients connected to them here.
type awarenessRelay struct {
	service *Service
	mu      sync.Mutex
	// pending is, per room, what the next flush is to carry for each client; a room is in it
	// from its first change until the batch the window then sends takes it. A client's entry is
	// the bytes to send - the removal the room's observer encoded at the clock the client held -
	// or nil for a client whose current state the flush is to read from the room's awareness
	// (resolve), so a reader that moves twice within the window is sent where it is now.
	pending map[string]map[uint64][]byte
	// inbound is, per room, the clients another process's notification is being applied to now,
	// and how many such injections name each: the room's observer, which ygo fires inside that
	// apply (received), leaves them to the process they came from rather than relaying them
	// back, as ygo's own relay observer leaves a change stamped with its injection sentinel
	// (provider/websocket/cluster.go:456-458). A client belongs to one process's socket, so a
	// mark never covers a change of this process's own.
	inbound map[string]map[uint64]int
	// local is, per room, the awareness clients of this process's own peers that the relay is
	// sending to the other processes: every client the room's observer has seen a state of and
	// not since a removal, less the ones another process's notification brought (inbound). The
	// room's sockets hold the list to maxAwarenessClientsPerSocket clients each
	// (awareness_limit.go). It is what a query is answered with and what a close of a room
	// removes (localRemoval).
	local map[string]map[uint64]struct{}
	// closed refuses every later send; Close sets it.
	closed bool
	// sends counts the batches and queries on their way to the sender, which Close waits for.
	sends sync.WaitGroup
	// oversized counts the client states dropped for being too large for a notification.
	oversized atomic.Uint64
}

func newAwarenessRelay(service *Service) *awarenessRelay {
	return &awarenessRelay{
		service: service,
		pending: make(map[string]map[uint64][]byte),
		inbound: make(map[string]map[uint64]int),
		local:   make(map[string]map[uint64]struct{}),
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
	if aw, resident := r.service.srv.GetAwareness(room); resident {
		r.watchRoomAwareness(room, aw)
	}
	if r.service.hasLocalConnection(room) {
		r.query(room)
	}
}

// RoomDeactivated is ygo's word that a room instance retired. Its clients' list goes with it,
// unless a successor instance is already resident under the name. Its awareness, the watcher
// registered on it included, goes with the instance itself.
func (r *awarenessRelay) RoomDeactivated(room string) {
	if _, resident := r.service.srv.GetAwareness(room); resident {
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

// enqueue adds an awareness update of room's own clients to the room's pending batch, each entry
// as it stands (record): a snapshot a query is answered with (answer) or the removal a closing
// room makes of its own clients (removeLocalAwareness). A room's own changes reach the batch
// through its observer instead (watchRoomAwareness).
func (r *awarenessRelay) enqueue(room string, update []byte) {
	entries, err := decodeAwarenessEntries(update)
	if err != nil {
		slog.Debug("dispatch: drop an awareness update the relay cannot read", "room", room, "error", err)
		return
	}
	r.record(room, entries)
}

// record puts each of entries into room's pending batch - a client's entry replacing any earlier
// one of its own, and a nil entry leaving the flush to read that client's current state
// (resolve) - and arms the batch's send when it is the room's first. It keeps the room's list of
// the clients it relays (local) as it goes: a client's state puts it on the list, a removal takes
// it off and is relayed like any other entry. What one batch can hold is what the room's sockets
// may announce: a change touches the clients of the frame that made it, and a socket's frame
// touches no more than maxAwarenessClientsPerSocket of them (awareness_limit.go), so a tab
// inventing thousands reaches the relay as its own handful and the readers who join after it are
// relayed at once.
func (r *awarenessRelay) record(room string, entries []awarenessEntry) {
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
		batch = make(map[uint64][]byte, min(len(entries), maxAwarenessClientsPerRoom))
	}
	clients := r.local[room]
	for _, entry := range entries {
		if entry.removed {
			delete(clients, entry.client)
		} else {
			if clients == nil {
				clients = make(map[uint64]struct{})
				r.local[room] = clients
			}
			clients[entry.client] = struct{}{}
		}
		batch[entry.client] = entry.raw
	}
	if len(clients) == 0 {
		delete(r.local, room)
	}
	if len(batch) == 0 || armed {
		return
	}
	r.pending[room] = batch
	r.sends.Add(1)
	time.AfterFunc(awarenessCoalesceWindow, func() {
		defer r.sends.Done()
		r.flush(room)
	})
}

// flush sends room's pending batch, if any is still pending, in as many notifications as its size
// takes (awarenessNotifications), each client at the state the room holds now (resolve).
func (r *awarenessRelay) flush(room string) {
	r.mu.Lock()
	batch := r.pending[room]
	delete(r.pending, room)
	r.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	r.resolve(room, batch)
	if len(batch) == 0 {
		return
	}
	payloads := r.awarenessNotifications(room, batch)
	ctx, cancel := context.WithTimeout(context.Background(), roomSyncSendTimeout)
	defer cancel()
	for _, payload := range payloads {
		if err := r.service.roomSyncSender.Notify(ctx, documentAwarenessChannel, payload); err != nil {
			slog.Warn("dispatch: relay a document room's awareness to the other processes", "room", room, "error", err)
			return
		}
	}
}

// resolve fills in what batch left for the flush to read: the entry of every client it holds a
// nil for, as room's awareness holds that client now, so a reader that moved several times
// within the window is sent where it is rather than where it was when the change was recorded.
// A client the room no longer holds is dropped - encoding one ygo has forgotten would name it at
// a clock of 0, which every other process drops as older than the state it holds - and its
// removal, if the room made one, stands in the batch at its own clock already.
func (r *awarenessRelay) resolve(room string, batch map[uint64][]byte) {
	unread := make([]uint64, 0, len(batch))
	for client, entry := range batch {
		if entry == nil {
			unread = append(unread, client)
		}
	}
	if len(unread) == 0 {
		return
	}
	aw, resident := r.service.srv.GetAwareness(room)
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
	for _, entry := range entries {
		batch[entry.client] = entry.raw
	}
}

// awarenessNotifications packs batch, room's client entries, into the fewest notification payloads
// each within roomSyncPayloadLimit, whole clients to each, in client order. A client whose entry
// alone does not fit is dropped and counted.
func (r *awarenessRelay) awarenessNotifications(room string, batch map[uint64][]byte) []string {
	framing, err := json.Marshal(awarenessMessage{Sender: r.service.processID, Room: room, Data: "-"})
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
		payload, err := json.Marshal(awarenessMessage{Sender: r.service.processID, Room: room, Data: base64.StdEncoding.EncodeToString(update)})
		if err != nil {
			slog.Error("dispatch: encode a document room's awareness notification", "room", room, "error", err)
		} else {
			payloads = append(payloads, string(payload))
		}
		group, size = nil, 0
	}
	for _, client := range clients {
		entry := batch[client]
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
	payload, err := json.Marshal(awarenessMessage{Sender: r.service.processID, Room: room, Query: true})
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
		if err := r.service.roomSyncSender.Notify(ctx, documentAwarenessChannel, string(payload)); err != nil {
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

// localRemoval is the awareness update that removes room's local clients from aw, the room's
// awareness, or nil when it holds none. Each removal stands at its client's current clock, as
// ygo's removal for a peer that disconnects does: a null state at an equal clock is taken by
// every receiver, and the client's own next announcement, one clock later, brings it back.
func (r *awarenessRelay) localRemoval(room string, aw *awareness.Awareness) []byte {
	clients := r.localClients(room, aw)
	if len(clients) == 0 {
		return nil
	}
	ids := slices.Sorted(maps.Keys(clients))
	return encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarUint(uint64(len(ids)))
		for _, client := range ids {
			encoder.WriteVarUint(client)
			encoder.WriteVarUint(clients[client])
			encoder.WriteVarString("null")
		}
	})
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
	if message.Sender == r.service.processID {
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
	if !r.service.holdsRoomForReaders(message.Room) {
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
	err = r.service.srv.Inject(ctx, cluster.Inbound{Room: message.Room, Kind: cluster.KindAwareness, Data: update})
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Debug("dispatch: apply another process's awareness to a document room", "room", message.Room, "error", err)
	}
	if aw, resident := r.service.srv.GetAwareness(message.Room); resident {
		reclaimAwarenessSlots(message.Room, aw)
	}
}

// answer sends, for another process's query, the states of room's local clients, when room is
// resident here and has any.
func (r *awarenessRelay) answer(room string) {
	aw, resident := r.service.srv.GetAwareness(room)
	if !resident {
		return
	}
	clients := r.localClients(room, aw)
	if len(clients) == 0 {
		return
	}
	ids := slices.Collect(maps.Keys(clients))
	r.enqueue(room, aw.EncodeUpdate(ids))
}

// watchRoomAwareness has aw, a room's awareness, tell the relay what each of the room's changes
// touched, and free a departed client's entry as the client leaves. ygo fires a room's awareness
// observers with the clients each change added, updated and removed - a peer's frame, a
// heartbeat, a peer's disconnect, an expiry, another process's relayed update - so the relay
// hears of every client a change touched where the lane behind Publish would hand it only the
// latest change's (see the top of this file). The entry a removal leaves behind is reclaimed
// there and then (reclaimAwarenessSlots), so a room of readers that come and go costs no more
// than the readers in it now, and the removal is encoded first, while the tombstone the purge
// frees still holds the departing client's clock.
//
// It watches updates rather than changes, so a heartbeat - the same state at a later clock, which
// fires no change observer - is relayed as the presence it is. The observer is dropped with the
// room instance's awareness itself; a successor instance under the same name is a new awareness,
// and RoomActivated gives it its own.
func (r *awarenessRelay) watchRoomAwareness(room string, aw *awareness.Awareness) {
	aw.OnUpdate(func(event awareness.UpdateEvent) {
		r.record(room, r.touched(room, aw, event))
		if len(event.Removed) > 0 {
			reclaimAwarenessSlots(room, aw)
		}
	})
}

// touched is what the relay is to send of event, the change room's awareness has just made: an
// entry each of the clients it added or updated, left for the flush to read their state as it
// stands then (resolve), and the removal of each client it took away, encoded here at the clock
// that client held. A client another process's notification is being applied for is left to the
// process it came from (inbound, received).
func (r *awarenessRelay) touched(room string, aw *awareness.Awareness, event awareness.UpdateEvent) []awarenessEntry {
	entries := make([]awarenessEntry, 0, len(event.Added)+len(event.Updated)+len(event.Removed))
	var left []uint64
	r.mu.Lock()
	inbound := r.inbound[room]
	for _, changed := range [][]uint64{event.Added, event.Updated} {
		for _, client := range changed {
			if inbound[client] == 0 {
				entries = append(entries, awarenessEntry{client: client})
			}
		}
	}
	for _, client := range event.Removed {
		if inbound[client] == 0 {
			left = append(left, client)
		}
	}
	r.mu.Unlock()
	if len(left) == 0 {
		return entries
	}
	removals, err := decodeAwarenessEntries(aw.EncodeUpdate(left))
	if err != nil {
		slog.Debug("dispatch: drop the removals of a document room the relay cannot read", "room", room, "error", err)
		return entries
	}
	return append(entries, removals...)
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

// removeLocalAwareness removes the awareness states of room's local clients before this process
// closes the room itself - its stored history replaced, its issue closed, a failed room's
// eviction, a quiesce, a shutdown - through every close the service makes (roomServer). ygo
// removes a peer's states as it disconnects, but a close disconnects the room's peers and tears
// its relay down together, and a peer's removal that comes after the teardown would reach no
// other process, whose readers would keep the cursor until it expired (awarenessExpiry). So the
// removal is applied to the room and handed to the relay here, while the room is still the
// server's: the relay sends it within its window whatever the close does next.
func (s *Service) removeLocalAwareness(room string) {
	aw, resident := s.srv.GetAwareness(room)
	if !resident {
		return
	}
	removal := s.awarenessRelay.localRemoval(room, aw)
	if removal == nil {
		return
	}
	s.awarenessRelay.enqueue(room, removal)
	if err := aw.ApplyUpdate(removal, nil); err != nil {
		slog.Warn("dispatch: remove a closing document room's awareness", "room", room, "error", err)
	}
}
