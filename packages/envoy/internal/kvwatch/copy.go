package kvwatch

import (
	"iter"
	"maps"
)

// Copy is one cache's in-memory copy of its bucket: its entries, the revision fence that keeps a
// delayed result from undoing a newer one, the marks of the read under way and the epoch that read
// began in. It is the whole of the read protocol (Cache) and of the write sites, so a cache keeps
// none of that state itself: the hooks it hands its watcher call BeginRead, Delivered and
// CompleteRead, and each result of a KV call it makes reaches the copy through Put, Apply or
// Evict, which drop a result of an earlier epoch and mark the key of every other before they
// compare the fence. No write site can take a result without marking it, or mark one it drops.
//
// A Copy has no lock of its own: the cache calls every method under the mutex that guards
// whatever else it keeps per key (a session's last heartbeat, its expiry), and its hooks take that
// mutex too. The write sites read the watcher's epoch under it, which the lock order allows: the
// cache's mutex, then the watcher's (Cache).
type Copy[V any] struct {
	rows      Rows[V]
	entries   map[string]V
	revisions map[string]uint64
	marks     marks
	readEpoch uint64
}

// Rows is how one copy turns a record into its entry, and what the cache that keeps it records
// beside its entries when one is stored or removed. The copy calls each under the cache's mutex.
type Rows[V any] struct {
	// Decode turns a put record into the entry the copy keeps for its key. keep false with a nil
	// error removes the key as a delete marker does: the cache keeps no entry for that record. An
	// error, a value it cannot decode, removes the key as well and is returned for the caller to
	// log.
	Decode func(entry Entry) (value V, keep bool, err error)
	// Stored, when not nil, follows every entry the copy stores.
	Stored func(key string, value V)
	// Removed, when not nil, is handed every entry the copy removes for the bucket's sake: by a
	// record or an Evict that passed the fence, and by a completed read that found no evidence of
	// it. Remove, the cache's own removal, does not call it.
	Removed func(key string, value V)
}

// Outcome is what became of a KV call's result handed to a copy (Put, Apply, Evict).
type Outcome uint8

const (
	// Dropped is a result through a handle of an earlier epoch (Watcher.Handle), from a bucket
	// since recreated. It reaches nothing and marks nothing: its revision would fence out the new
	// bucket's, which number from 1 again, and the new bucket's read delivers what it holds for the
	// key.
	Dropped Outcome = iota
	// Fenced is a result of the current epoch, whose key it marked for the read under way, at a
	// revision older than the one the copy holds for that key: the copy is left as it was.
	Fenced
	// Applied is a result of the current epoch that marked its key and that the copy took.
	Applied
)

// NewCopy returns an empty copy that takes its records through rows.
func NewCopy[V any](rows Rows[V]) Copy[V] {
	return Copy[V]{rows: rows, entries: map[string]V{}, revisions: map[string]uint64{}}
}

// Get answers the entry the copy holds for key.
func (c *Copy[V]) Get(key string) (V, bool) {
	value, ok := c.entries[key]
	return value, ok
}

// Len is the number of entries the copy holds.
func (c *Copy[V]) Len() int {
	return len(c.entries)
}

// All yields every entry the copy holds, in no order. The loop may Remove the entry it is handed.
func (c *Copy[V]) All() iter.Seq2[string, V] {
	return maps.All(c.entries)
}

// Remove takes key's entry out of the copy for a reason of the cache's own, such as an expiry the
// bucket never delivers; the cache records beside it what it records for that reason, so Remove
// does not call Rows.Removed. The fence keeps key's revision: no record removed it, so a delayed
// one older than that revision is still fenced out.
func (c *Copy[V]) Remove(key string) {
	delete(c.entries, key)
}

// BeginRead starts a read of the bucket (Cache.BeginRead): a fresh set of marks, the read's epoch
// as the one the fence belongs to, and, for a recreated bucket whose revisions number from 1
// again, no fence. Every entry stays.
func (c *Copy[V]) BeginRead(read Read) {
	c.marks.begin()
	c.readEpoch = read.Epoch
	if read.Recreated {
		clear(c.revisions)
	}
}

// Delivered takes one record the current read delivered (Cache.Apply). Its key is marked before
// the fence is compared, so a delivered value older than a local write still keeps the key
// through the sweep. The error is a value Rows.Decode could not decode, for the caller to log.
func (c *Copy[V]) Delivered(entry Entry) error {
	c.marks.mark(entry.Key)
	_, err := c.apply(entry)
	return err
}

// CompleteRead removes every entry and fence entry the read found no evidence of, handing each
// removed entry to Rows.Removed, and ends the read's marks (Cache.CompleteRead): the bucket no
// longer holds those keys.
func (c *Copy[V]) CompleteRead() {
	sweep(&c.marks, c.entries, c.rows.Removed)
	sweep(&c.marks, c.revisions, nil)
	c.marks.end()
}

// Put takes value, written for key at revision through a handle of epoch (Watcher.Handle): a
// write-through, or a read-through the cache decoded itself.
func (c *Copy[V]) Put(w *Watcher, epoch uint64, key string, value V, revision uint64) Outcome {
	if !c.current(w, epoch, key) {
		return Dropped
	}
	if c.fenced(key, revision) {
		return Fenced
	}
	c.store(key, value, revision)
	return Applied
}

// Apply takes entry, a record of the bucket read through a handle of epoch, as a delivered record
// is taken: a read-through, or a key's latest record read back after a delete. The error is a
// value Rows.Decode could not decode, for the caller to log; a record the fence drops is not
// decoded.
func (c *Copy[V]) Apply(w *Watcher, epoch uint64, entry Entry) (Outcome, error) {
	if !c.current(w, epoch, entry.Key) {
		return Dropped, nil
	}
	return c.apply(entry)
}

// Evict removes key at revision for a delete made through a handle of epoch whose own record the
// cache could not read back.
func (c *Copy[V]) Evict(w *Watcher, epoch uint64, key string, revision uint64) Outcome {
	if !c.current(w, epoch, key) {
		return Dropped
	}
	if c.fenced(key, revision) {
		return Fenced
	}
	c.evict(key, revision)
	return Applied
}

// FenceFor is the revision the fence holds for key, for a delete about to be made through a handle
// of epoch, and 0 when the read under way did not begin in epoch: between a watch moving the epoch
// and that read beginning, the fence still holds the recreated bucket's revisions.
func (c *Copy[V]) FenceFor(epoch uint64, key string) uint64 {
	if c.readEpoch != epoch {
		return 0
	}
	return c.revisions[key]
}

// current reports whether a result through a handle of epoch may reach the copy, and marks key
// for the read under way when it may.
func (c *Copy[V]) current(w *Watcher, epoch uint64, key string) bool {
	if w.Epoch() != epoch {
		return false
	}
	c.marks.mark(key)
	return true
}

// fenced reports whether the fence drops a result for key at revision: the copy already took a
// higher revision of it in the current epoch.
func (c *Copy[V]) fenced(key string, revision uint64) bool {
	return revision < c.revisions[key]
}

// apply takes one record whose key is already marked: one the fence drops leaves the copy as it
// is, undecoded; a put the cache keeps is stored; and a delete or purge marker, a put the cache
// does not keep and one it cannot decode remove the key.
func (c *Copy[V]) apply(entry Entry) (Outcome, error) {
	if c.fenced(entry.Key, entry.Revision) {
		return Fenced, nil
	}
	if entry.Op != OpPut {
		c.evict(entry.Key, entry.Revision)
		return Applied, nil
	}
	value, keep, err := c.rows.Decode(entry)
	if err != nil || !keep {
		c.evict(entry.Key, entry.Revision)
		return Applied, err
	}
	c.store(entry.Key, value, entry.Revision)
	return Applied, nil
}

func (c *Copy[V]) store(key string, value V, revision uint64) {
	if c.entries == nil {
		c.entries = map[string]V{}
	}
	c.entries[key] = value
	c.setRevision(key, revision)
	if c.rows.Stored != nil {
		c.rows.Stored(key, value)
	}
}

func (c *Copy[V]) evict(key string, revision uint64) {
	if value, ok := c.entries[key]; ok {
		delete(c.entries, key)
		if c.rows.Removed != nil {
			c.rows.Removed(key, value)
		}
	}
	c.setRevision(key, revision)
}

func (c *Copy[V]) setRevision(key string, revision uint64) {
	if c.revisions == nil {
		c.revisions = map[string]uint64{}
	}
	c.revisions[key] = revision
}
