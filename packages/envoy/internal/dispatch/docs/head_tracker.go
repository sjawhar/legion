package docs

import (
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// headTracker is what this process knows of each document's stored history without reading it:
// whether anything has written the document since a given moment, and the head its history stood
// at after the last write this process heard of. One per Service; the stale-room evictor's LISTEN
// feeds it, and the persistence layer reports this process's own writes to it.
//
// What it rests on is that every write that changes a document's stored content notifies, in the
// transaction that makes it: documentHeadChannel's notice with the head after the write, and
// documentChangedChannel's bare id beside it (appendUpdatesStatement, notifyDocumentChanged). A
// compaction changes no content and notifies nothing, so it changes nothing here either. While the
// listener's connection stands, a write of another process therefore reaches observe or
// invalidate within the notification's delivery, and a write of this process reaches it at its own
// end, before the call that committed it returns (beginOwnWrite, ownWrite.end), so a process reads
// its own writes at once and another's within that delivery. While the connection is down nothing
// would announce a write, so nothing is trusted (disconnected), and a reconnect starts a new epoch
// in which nothing heard before counts (connected).
//
// A connection can also stand and carry nothing - half-open, its socket up, no error, as a dropped
// NAT entry or a failover leaves it - which pglisten's own idle ping finds only after about 35 s
// (pglisten.DefaultListenIdle and DefaultPingTimeout). So the tracker trusts itself only while the
// connection has delivered something within headTrustWindow (delivered), and its liveness
// (headLiveness.pingWhileQuiet) keeps an idle healthy connection delivering with a ping of its own
// on documentPingChannel while this process holds something that trust serves - a resident room, a
// cached cold read or a preload - and the connection has delivered nothing for the process's quiet
// window, headPingAfter plus a jitter drawn afresh after each delivery and each ping (headLiveness).
// Notifications arrive in commit order, so a delivery says every write committed before what it
// carried has been heard; on a half-open connection a write is served stale for at most
// headTrustWindow past the last delivery plus one notification's delivery, about 3 s, after which
// every read falls back to the store. Every process's ping reaches every listener and draws each
// one's window afresh, so the first holder whose window ends pings for all of them: a deployment
// sends about one ping a second while some process holds a room, a cached read or a preload and no
// document write is announced, and none otherwise.
//
// Per document it keeps a generation, which every write heard of moves on - drawn from one
// process-wide counter, so an entry made again never repeats a generation an old token carries -
// the last head heard of, or none, and how many writes of this process are between their gate and
// their end. A token (headToken) is the epoch and the generation at one moment: unchanged says
// whether nothing has written the document since.
//
// It keeps an entry only for a document this process holds (hold, release): a resident room, a
// cached cold read, a preload, or a cold read between its token and the end of its read, cached or
// not. A notice of any other document is dropped and makes no entry, so the map is bounded by what
// this process holds, about a hundred bytes a document, and not by what the deployment writes.
// Dropping an entry is safe because nothing trusts what the tracker no longer holds. A token is
// taken only under a hold that lasts until what was read is cached under it or given up, and a
// cached read holds its document for as long as it is cached, so every write heard while a token
// is in use moves the generation it is compared with. Once an entry has gone, no token taken under
// it is unchanged again: unchanged finds no entry, or one made since, whose generation is drawn
// after the token's. And head is untrusted for a document with no entry, as for one made since that
// has heard of no write, so its caller reads the store. What a dropped entry forgets is only what
// the tracker could have answered without the store.
//
// This process's own writes keep their document's entry, held or not, from the gate to the end
// the tracker sees (beginOwnWrite, ownWrite.end), and a write whose end it cannot see keeps it
// until the document's next notice or the next LISTEN (ownWrite.untrack): while either lasts the
// document is untrusted, and the end moves the generation on, so a read that overlapped the write
// checks the store again. The entry goes with that end if nothing holds the document: a tracked
// write has committed or rolled back by then, so a read that starts after it finds in the store
// what the write left.
type headTracker struct {
	mu sync.Mutex
	// now is the clock trust is measured on: time.Now, and a unit test's own.
	now func() time.Time
	// listening is whether the evictor's LISTEN stands: false from the start, true from each
	// connected, false again from each disconnected.
	listening bool
	// heard is when the LISTEN's connection last delivered anything: the LISTEN itself
	// (connected) or a notification on any channel it listens on (delivered).
	heard time.Time
	// epoch is the number of LISTENs that have stood; a token of an earlier epoch is never
	// unchanged, since the notifications sent while no connection stood reached nobody.
	epoch uint64
	// generations is the process-wide counter every generation is drawn from.
	generations uint64
	entries     map[string]*headEntry
}

// headEntry is one document's record in the tracker, kept while dropIfUnheldLocked keeps it.
type headEntry struct {
	generation uint64
	// holds is how many holders of the document this process has (headTracker.hold): its resident
	// room, its cached cold read, its preload, and each cold read between its token and its end.
	holds int
	// head is the head the last write heard of left the document's history at, when known is set.
	head  int64
	known bool
	// writes are this process's writes of the document between their gate and their end. While
	// any is in flight nothing about the document is trusted: such a write may have committed
	// without its end having run yet.
	writes []*ownWrite
	// untracked is set by a write of this process whose end the tracker cannot see (a transaction
	// store.AfterCommit refused), and cleared by the next documentHeadChannel notice of the
	// document, which that write's commit sends if nothing sent one since.
	untracked bool
	// idle wakes, once writes empties, a decision waiting on this process's writes of the document
	// (ownWritesDone); nothing is made while nothing waits.
	idle wakeSignal
}

// headToken is the tracker's state for one document at one moment (headTracker.token): the epoch,
// the document's generation, and whether the tracker trusted itself then.
type headToken struct {
	epoch, generation uint64
	trusted           bool
}

// ownWrite is one write of this process to a document, from its gate to its end (beginOwnWrite).
// A nil ownWrite is a write no tracker follows, and its methods do nothing.
type ownWrite struct {
	tracker *headTracker
	room    string
	// seen is every head documentHeadChannel announced for the document while the write was in
	// flight, which end reads to tell whether the write's own notice has already been heard.
	seen  []int64
	ended atomic.Bool
}

// closedIdle is what ownWritesDone answers for a document with no write of this process in flight.
var closedIdle = func() chan struct{} {
	idle := make(chan struct{})
	close(idle)
	return idle
}()

func newHeadTracker() *headTracker {
	return &headTracker{now: time.Now, entries: map[string]*headEntry{}}
}

// entryLocked is room's entry, made with a fresh generation and no head when there is none, so it
// says only what the tracker hears from then on. Its caller holds t.mu.
func (t *headTracker) entryLocked(room string) *headEntry {
	entry := t.entries[room]
	if entry == nil {
		t.generations++
		entry = &headEntry{generation: t.generations}
		t.entries[room] = entry
	}
	return entry
}

// bumpLocked moves entry's generation on, so no token taken before it is unchanged. Its caller
// holds t.mu.
func (t *headTracker) bumpLocked(entry *headEntry) {
	t.generations++
	entry.generation = t.generations
}

// dropIfUnheldLocked drops room's entry once nothing keeps it: no holder (hold), no write of this
// process in flight, and none ended unseen since the document's last notice (untracked). Its caller
// holds t.mu.
func (t *headTracker) dropIfUnheldLocked(room string, entry *headEntry) {
	if entry.holds == 0 && len(entry.writes) == 0 && !entry.untracked {
		delete(t.entries, room)
	}
}

// hold is a holder of room in this process beginning: a resident room's load (Service.roomLoaded),
// a cached cold read (documentReads.put), a preload (Service.offerPreload), or a cold read about to
// take its token (Service.coldRead). From here every write the tracker hears of room moves its
// entry on, until the matching release.
func (t *headTracker) hold(room string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entryLocked(room).holds++
}

// release ends one hold of room, and with the last one the entry goes unless a write of this
// process keeps it (dropIfUnheldLocked). A release with no hold to end does nothing.
func (t *headTracker) release(room string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[room]
	if entry == nil || entry.holds == 0 {
		return
	}
	entry.holds--
	t.dropIfUnheldLocked(room, entry)
}

// trustedLocked is whether entry's answers hold now: the tracker is listening (listeningLocked), no
// write of this process to the document is in flight, and none ended unseen since the last notice.
// Its caller holds t.mu.
func (t *headTracker) trustedLocked(entry *headEntry) bool {
	return t.listeningLocked() && len(entry.writes) == 0 && !entry.untracked
}

// listeningLocked is whether the LISTEN stands and its connection has delivered something within
// headTrustWindow of now, so trust lapses on a silent connection with no event to end it. Its
// caller holds t.mu.
func (t *headTracker) listeningLocked() bool {
	return t.listening && t.now().Sub(t.heard) < headTrustWindow
}

// connected is the evictor's LISTEN standing: a new epoch, in which every token taken before is
// stale, and every entry forgets its head and any write that ended unseen - the notifications sent
// while no connection stood reached nobody, so nothing heard before says what the store holds now.
// An entry something holds, or with a write of this process in flight, stays, with a fresh
// generation and no head, so that write's end still finds it and a decision waiting on it still
// wakes; any other goes. The LISTEN is a round trip the connection made, so it counts as the
// connection's first delivery (listeningLocked).
func (t *headTracker) connected() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.listening = true
	t.heard = t.now()
	t.epoch++
	for room, entry := range t.entries {
		t.bumpLocked(entry)
		entry.head, entry.known, entry.untracked = 0, false, false
		t.dropIfUnheldLocked(room, entry)
	}
}

// disconnected is the evictor's LISTEN lost: nothing is trusted until the next connected, since a
// write committed meanwhile notifies nobody here.
func (t *headTracker) disconnected() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.listening = false
}

// delivered is a notification the LISTEN's connection carried, on any of its channels, the
// evictor's ping included: the connection is still delivering, so the tracker trusts itself for
// headTrustWindow from now (listeningLocked).
func (t *headTracker) delivered() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.heard = t.now()
}

// quiet is how long the LISTEN's connection has delivered nothing, and whether the LISTEN stands:
// what the liveness's ping asks before it sends one (headLiveness.pingWhileQuiet).
func (t *headTracker) quiet() (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.now().Sub(t.heard), t.listening
}

// figures is whether the tracker trusts itself (listeningLocked) and how many documents it holds an
// entry for, for the room figures.
func (t *headTracker) figures() HeadTrackerFigures {
	if t == nil {
		return HeadTrackerFigures{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return HeadTrackerFigures{Trusted: t.listeningLocked(), Entries: len(t.entries)}
}

// observe is a documentHeadChannel notice: room's history stands at head after a write that has
// committed, this process's own or another's. The generation moves on and the head is recorded;
// a write of this process whose end the tracker could not see is accounted for by it, since
// notices arrive in commit order and that write's own notice is this one or before it, and the
// entry goes if that write was all that kept it. A document with no entry is one this process
// neither holds nor writes, which nothing here asks about: the notice is dropped, and makes no
// entry.
func (t *headTracker) observe(room string, head int64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[room]
	if entry == nil {
		return
	}
	t.bumpLocked(entry)
	entry.head, entry.known, entry.untracked = head, true, false
	for _, write := range entry.writes {
		write.seen = append(write.seen, head)
	}
	t.dropIfUnheldLocked(room, entry)
}

// invalidate is a documentChangedChannel notification: room's history changed, at a head the bare
// id does not say. Every write of this build sends a documentHeadChannel notice beside it, which
// observe takes; the bare id alone is a write of an older build, during a roll, and what it
// changes here is the generation, so a cold read cached before it checks the store again. The head
// stays as it was, which is how far this build's rooms follow an older build's writes: not at all,
// until the gate refuses one of their writes and they reload (refuseThroughBehindHead). A document
// with no entry is one nothing of this process holds, so it gains none.
func (t *headTracker) invalidate(room string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if entry := t.entries[room]; entry != nil {
		t.bumpLocked(entry)
	}
}

// beginOwnWrite is a write of this process to room passing its gate: from here until its end
// (ownWrite.end, or ownWrite.untrack for a write whose end the tracker cannot see) nothing about
// room is trusted, since the write can commit before its end runs, and the write keeps room's
// entry whether or not anything holds the document, so a token taken meanwhile is untrusted.
// Every content-changing writer of this process calls it: a room's batch of browser updates
// (AppendUpdatesWithCredit), an append in a caller's transaction (appendUpdateTxClass), RebuildTx,
// PruneAfter and Delete. Nil for a store no Service was built on.
func (t *headTracker) beginOwnWrite(room string) *ownWrite {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entryLocked(room)
	write := &ownWrite{tracker: t, room: room}
	entry.writes = append(entry.writes, write)
	return write
}

// end is the write's end, once its transaction has ended: committed, and head the head it left
// the history at, or rolled back. Either way the generation moves on, so a cold read that read the
// store while the write was in flight checks the store again. A committed write records its head,
// unless its own notice has already been heard (seen holds that head): notices arrive in commit
// order, so a head heard after the write's own notice is of a later write and stands. The entry
// goes if this write was all that kept it: a read from here on finds the write's end in the store.
// It runs once; a second call does nothing.
func (w *ownWrite) end(committed bool, head int64) {
	if w == nil || w.ended.Swap(true) {
		return
	}
	t := w.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.dropWriteLocked(w)
	t.bumpLocked(entry)
	if committed && !slices.Contains(w.seen, head) {
		entry.head, entry.known = head, true
	}
	t.dropIfUnheldLocked(w.room, entry)
}

// untrack ends the write as one whose end the tracker cannot see: room is untrusted, and its entry
// kept, from here until its next documentHeadChannel notice (observe), which the write's commit
// sends if nothing sent one since, or the next LISTEN (connected). It runs once, like end.
func (w *ownWrite) untrack() {
	if w == nil || w.ended.Swap(true) {
		return
	}
	t := w.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.dropWriteLocked(w)
	t.bumpLocked(entry)
	entry.untracked = true
}

// dropWriteLocked takes w out of its document's writes and wakes whatever waits on the document's
// writes once none is left (ownWritesDone). The entry is there: a write in flight keeps it
// (dropIfUnheldLocked, connected). Its caller holds t.mu.
func (t *headTracker) dropWriteLocked(w *ownWrite) *headEntry {
	entry := t.entryLocked(w.room)
	if at := slices.Index(entry.writes, w); at >= 0 {
		entry.writes = slices.Delete(entry.writes, at, at+1)
	}
	if len(entry.writes) == 0 {
		entry.idle.wake()
	}
	return entry
}

// ownWriteIn begins room's own write in tx (beginOwnWrite) and ends it when tx ends, through
// store.AfterCommit, so the end runs inside the Commit that ends tx, before the caller that
// committed returns. The write reports the head it left with the returned function once it has
// written; a transaction that ends without that report - the write failed, and its caller rolled
// back or committed the rest - ends the write as rolled back. A transaction store.AfterCommit
// refuses, one the shared pool did not open, ends the write untracked at once (ownWrite.untrack).
func (t *headTracker) ownWriteIn(tx pgx.Tx, room string) (wrote func(head int64)) {
	write := t.beginOwnWrite(room)
	if write == nil {
		return func(int64) {}
	}
	var written atomic.Int64
	written.Store(-1)
	if !store.AfterCommit(tx, func(committed bool) {
		head := written.Load()
		write.end(committed && head >= 0, head)
	}) {
		write.untrack()
		return func(int64) {}
	}
	return func(head int64) { written.Store(head) }
}

// token is room's state now, to be compared later with unchanged: taken before a read of the
// store, it says after the read whether anything has written the document since. Its caller holds
// room (hold) from before it takes the token until what it read is cached under the token or given
// up, so every write heard of in between moves the generation on. A document with no entry gives
// an untrusted token. Its trusted field is whether the tracker trusts itself now; an untrusted token
// is never unchanged.
func (t *headTracker) token(room string) headToken {
	if t == nil {
		return headToken{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[room]
	if entry == nil {
		return headToken{epoch: t.epoch}
	}
	return headToken{epoch: t.epoch, generation: entry.generation, trusted: t.trustedLocked(entry)}
}

// unchanged is whether nothing has written room since token was taken: the token was trusted, the
// epoch and the generation are still its own, and the tracker trusts itself now. A token whose
// entry has gone since is never unchanged again: there is no entry, or one made since, whose
// generation is drawn after the token's.
func (t *headTracker) unchanged(room string, token headToken) bool {
	if t == nil || !token.trusted {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[room]
	return entry != nil && t.epoch == token.epoch && entry.generation == token.generation && t.trustedLocked(entry)
}

// head is the head room's history stood at after the last write heard of, and whether it holds
// now: the tracker trusts itself and has heard of a write of the document since its entry was made
// this epoch. A document with no entry is untrusted, so its caller reads the store.
func (t *headTracker) head(room string) (int64, bool) {
	if t == nil {
		return 0, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[room]
	if entry == nil || !entry.known || !t.trustedLocked(entry) {
		return 0, false
	}
	return entry.head, true
}

// ownWritesDone is closed once no write of this process to room is in flight: at once when none
// is, else at the end of the last one in flight.
func (t *headTracker) ownWritesDone(room string) <-chan struct{} {
	if t == nil {
		return closedIdle
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[room]
	if entry == nil || len(entry.writes) == 0 {
		return closedIdle
	}
	return entry.idle.wait()
}
