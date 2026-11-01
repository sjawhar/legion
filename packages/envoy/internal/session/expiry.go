package session

import (
	"container/heap"
	"time"
)

// expiryKind names what an expiry deadline ends for a session id.
type expiryKind uint8

const (
	// expireLive is the deadline of the id's live cache entry. At it the entry leaves the cache and
	// its final heartbeat moves to lastSeen.
	expireLive expiryKind = iota
	// expireLastSeen is the deadline of the id's lastSeen record, the registry's retention (one TTL
	// unless WithLastSeenRetention set longer) after the id left the cache. At it the record leaves
	// lastSeen. The id's revision fence is not dropped here: the copy keeps one fence entry per key
	// and every completed read sweeps the entries the bucket no longer holds
	// (kvwatch.Copy.CompleteRead), which is the one place a fence may be dropped safely.
	expireLastSeen
)

// unfollowedRecheck is how long expireDue waits before it looks again while the copy does not
// follow its bucket. Nothing expires then: a row's TTL is the only word that its session ended,
// since JetStream's MaxAge expiry is never delivered, but a copy that cannot see the heartbeats
// that renewed a row must not read its age as the end of its session, and the read that restores
// following delivers them (kvwatch.Watcher.Following). The wait is short because that read's
// completion arms nothing here; the episode it covers is a reconnect or a re-read, and past the
// stale bound the copy is not served at all (kvwatch.Watcher.Servable).
const unfollowedRecheck = time.Second

// expiryKey names one deadline: at most one per id and kind, so the heap holds at most two items
// per id.
type expiryKey struct {
	id   string
	kind expiryKind
}

// expiryItem is one deadline in the registry's expiry heap. index is its position in the heap,
// which heap.Fix needs to move it when a write changes its deadline in place.
type expiryItem struct {
	key      expiryKey
	deadline time.Time
	index    int
}

// expiryHeap is a min-heap of expiry deadlines, earliest first (container/heap).
type expiryHeap []*expiryItem

func (h expiryHeap) Len() int           { return len(h) }
func (h expiryHeap) Less(i, j int) bool { return h[i].deadline.Before(h[j].deadline) }

func (h expiryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *expiryHeap) Push(x any) {
	item := x.(*expiryItem)
	item.index = len(*h)
	*h = append(*h, item)
}

func (h *expiryHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return item
}

// scheduleLocked sets the deadline of id's kind of expiry to deadline, in place when one is already
// scheduled, and re-arms the expiry timer when the earliest deadline moved earlier. A lastSeen
// deadline only ever moves later: the revision fence it ends must outlive every time the id left
// the cache by the retention. A non-positive ttl or a zero deadline schedules nothing: such an
// entry never expires. A registry built from a struct literal has no heap, index or timer until its
// first schedule. Callers must hold r.mu for writing.
func (r *SessionRegistry) scheduleLocked(id string, kind expiryKind, deadline time.Time) {
	if r.ttl <= 0 || deadline.IsZero() {
		return
	}
	r.setExpiryLocked(id, kind, deadline)
	r.armExpiryLocked()
}

// setExpiryLocked is scheduleLocked without re-arming the timer, for the expiry callback, which
// re-arms once after it has popped every due item. Callers must hold r.mu for writing.
func (r *SessionRegistry) setExpiryLocked(id string, kind expiryKind, deadline time.Time) {
	key := expiryKey{id: id, kind: kind}
	if item, ok := r.expiryIndex[key]; ok {
		if kind == expireLastSeen && !deadline.After(item.deadline) {
			return
		}
		item.deadline = deadline
		heap.Fix(&r.expiries, item.index)
		return
	}
	if r.expiryIndex == nil {
		r.expiryIndex = map[expiryKey]*expiryItem{}
	}
	item := &expiryItem{key: key, deadline: deadline}
	heap.Push(&r.expiries, item)
	r.expiryIndex[key] = item
}

// armExpiryLocked arms the expiry timer for the earliest deadline when it is not armed or is armed
// for a later one. A timer armed for an earlier deadline than the earliest (a write moved that
// deadline later) is left alone: it fires early, finds nothing due, and re-arms. While the copy
// does not follow its bucket the timer is armed no earlier than unfollowedRecheck from now, since
// nothing may expire until a read restores following and a deadline already past would otherwise
// re-arm it at once on every write. The timer is a time.AfterFunc, so no goroutine waits on it and
// nothing has to stop with the watcher. The wait is measured on the registry's own clock, so a
// test that moves that clock moves the deadlines with it. Callers must hold r.mu for writing.
func (r *SessionRegistry) armExpiryLocked() {
	if len(r.expiries) == 0 {
		return
	}
	now := r.now()
	earliest := r.expiries[0].deadline
	if recheck := now.Add(unfollowedRecheck); !r.following() && earliest.Before(recheck) {
		earliest = recheck
	}
	if !r.expiryArmedFor.IsZero() && !earliest.Before(r.expiryArmedFor) {
		return
	}
	wait := max(earliest.Sub(now), 0)
	if r.expiryTimer == nil {
		r.expiryTimer = time.AfterFunc(wait, r.expireDue)
	} else {
		r.expiryTimer.Reset(wait)
	}
	r.expiryArmedFor = earliest
}

// expireDue is the expiry timer's callback. It pops every deadline that has passed, re-arms the
// timer for the next one, and then calls OnChange's functions for each entry it removed from the
// cache: a lapse has no watch event, so this and PruneLapsed, which sweeps the cache for the same
// entries without waiting for the timer, are where the relay hears of one. While the copy does not
// follow its bucket it pops nothing and only looks again (unfollowedRecheck).
//
// A live entry's deadline removes the entry only when the cache still holds it with that same
// expiresAt; a write that replaced it since moved the deadline, and then nothing happens. It is
// removed through the copy's own removal (kvwatch.Copy.Remove), which keeps the key's revision in
// the fence -- no record removed it, so a delayed record older than that revision is still fenced
// out -- and leaves to this callback what the copy records beside a row it removes for the
// bucket's sake: its final heartbeat in lastSeen until the retention after the moment it left the
// live view (departedLocked), so LastSeen answers the same before and after the removal, and the
// id's lastSeen deadline.
//
// A lastSeen deadline removes the id's lastSeen record. It changes no entry, so it reports
// nothing: a read already answered the id as gone when its entry left. The fence entry outlives it
// and is dropped by the next completed read that finds the bucket no longer holds the key
// (kvwatch.Copy.CompleteRead).
func (r *SessionRegistry) expireDue() {
	r.mu.Lock()
	defer r.unlockAndNotify()
	r.expiryArmedFor = time.Time{}
	if !r.following() {
		r.armExpiryLocked()
		return
	}
	now := r.now()
	for len(r.expiries) > 0 && !r.expiries[0].deadline.After(now) {
		item := heap.Pop(&r.expiries).(*expiryItem)
		delete(r.expiryIndex, item.key)
		id := item.key.id
		switch item.key.kind {
		case expireLive:
			cs, ok := r.cache.Get(id)
			if !ok || !cs.expiresAt.Equal(item.deadline) {
				continue
			}
			r.setExpiryLocked(id, expireLastSeen, r.retireLocked(id, cs, now))
			r.cache.Remove(id)
			r.changedLocked(id)
		case expireLastSeen:
			if seen, ok := r.lastSeen[id]; ok {
				if seen.expiresAt.After(now) {
					r.setExpiryLocked(id, expireLastSeen, seen.expiresAt)
					continue
				}
				delete(r.lastSeen, id)
			}
		}
	}
	r.armExpiryLocked()
}
