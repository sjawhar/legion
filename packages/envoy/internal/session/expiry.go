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
	// expireLastSeen is the deadline of the id's lastSeen record and of its revision fence, the
	// registry's retention (one TTL unless WithLastSeenRetention set longer) after the id left the
	// cache. At it the record leaves lastSeen and, when the id is still not cached, the fence leaves
	// cacheRevisions.
	expireLastSeen
)

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
// deadline later) is left alone: it fires early, finds nothing due, and re-arms. The timer is a
// time.AfterFunc, so no goroutine waits on it and nothing has to stop with the watcher. Callers must
// hold r.mu for writing.
func (r *SessionRegistry) armExpiryLocked() {
	if len(r.expiries) == 0 {
		return
	}
	earliest := r.expiries[0].deadline
	if !r.expiryArmedFor.IsZero() && !earliest.Before(r.expiryArmedFor) {
		return
	}
	wait := max(time.Until(earliest), 0)
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
// entries without waiting for the timer, are where the relay hears of one.
//
// A live entry's deadline removes the entry only when the cache still holds it with that same
// expiresAt; a write that replaced it since moved the deadline, and then nothing happens. Removing it
// records its final heartbeat in lastSeen until the retention after its deadline, so LastSeen
// answers the same before and after the removal, and schedules the id's lastSeen deadline.
//
// A lastSeen deadline removes the id's lastSeen record and, when the cache holds no entry for the
// id, its revision fence in cacheRevisions, so the fence map holds only live ids and ids that left
// within the retention. It changes no entry, so it reports nothing: a read already answered the id
// as gone when its entry left. Dropping the fence is safe because the retention is never less than
// one TTL: any revision older than the fence was written before the id left the cache, so its
// Created() is more than one TTL in the past, and expiryFor gives a late delivery of it a deadline
// that has already passed. cacheSessionLocked does not insert such a delivery for an id it does not
// hold, so it neither makes the id live nor brings back a LastSeen answer.
func (r *SessionRegistry) expireDue() {
	var lapsed []string
	r.mu.Lock()
	r.expiryArmedFor = time.Time{}
	now := time.Now()
	for len(r.expiries) > 0 && !r.expiries[0].deadline.After(now) {
		item := heap.Pop(&r.expiries).(*expiryItem)
		delete(r.expiryIndex, item.key)
		id := item.key.id
		switch item.key.kind {
		case expireLive:
			cs, ok := r.cache[id]
			if !ok || !cs.expiresAt.Equal(item.deadline) {
				continue
			}
			r.retainLastSeenLocked(id, cs.entry.UpdatedAt, cs.expiresAt)
			delete(r.cache, id)
			r.setExpiryLocked(id, expireLastSeen, cs.expiresAt.Add(r.retention()))
			lapsed = append(lapsed, id)
		case expireLastSeen:
			if seen, ok := r.lastSeen[id]; ok {
				if seen.expiresAt.After(now) {
					r.setExpiryLocked(id, expireLastSeen, seen.expiresAt)
					continue
				}
				delete(r.lastSeen, id)
			}
			if _, cached := r.cache[id]; !cached {
				delete(r.cacheRevisions, id)
			}
		}
	}
	r.armExpiryLocked()
	r.mu.Unlock()
	r.notifyChanged(lapsed...)
}
