package kvwatch

// Marks is the set of keys a cache has evidence of from the current read: the keys that read
// delivered, and those a write-through, read-through or delete of the current incarnation touched.
// A cache begins it in BeginRead, marks a key on each piece of such evidence, and once the read
// completes removes every entry the read left unmarked (Sweep) and ends it: the bucket no longer
// holds those entries. A key is marked before any revision fence is compared, so a delivered value
// older than a local write still keeps that key through the sweep.
//
// Marks are collected only while a read is under way: between a completed read and the next
// BeginRead nothing reads them, so Mark records nothing then, and a cache that follows its bucket
// keeps no set of every key it has seen. A read that times out or ends before its end marker never
// ends its marks, which stay collected until the next read begins.
//
// The zero value is an empty set that collects nothing. Marks has no lock of its own: the cache
// uses it under the mutex that guards its entries, which every cache hook takes.
type Marks struct {
	keys   map[string]struct{}
	active bool
}

// Begin forgets every mark and starts collecting them, for a read that begins.
func (m *Marks) Begin() {
	clear(m.keys)
	m.active = true
}

// End stops collecting marks and drops the set, once a completed read has swept against it.
func (m *Marks) End() {
	m.keys = nil
	m.active = false
}

// Mark records that the current read has evidence of key. With no read under way it records
// nothing.
func (m *Marks) Mark(key string) {
	if !m.active {
		return
	}
	if m.keys == nil {
		m.keys = map[string]struct{}{}
	}
	m.keys[key] = struct{}{}
}

// MarkCurrent reports whether the result of a KV call made through a handle of epoch
// (Watcher.Handle) may reach the cache, and marks key for the read under way when it may. A result
// from a bucket since recreated is dropped and marks nothing: its revision would fence out the new
// bucket's, which number from 1 again, and the new bucket's read delivers what it holds for key. A
// cache calls it under its own mutex, which the lock order allows: the cache's mutex, then the
// watcher's (Cache).
func (m *Marks) MarkCurrent(w *Watcher, epoch uint64, key string) bool {
	if w.Epoch() != epoch {
		return false
	}
	m.Mark(key)
	return true
}

// Marked reports whether the current read has evidence of key.
func (m *Marks) Marked(key string) bool {
	_, ok := m.keys[key]
	return ok
}

// Sweep deletes from entries every key marks does not hold, handing each removed key and value to
// removed when it is not nil (a cache records there what it records for an expired entry). It
// leaves marks as they are, so a cache sweeps each map it keeps per key (its entries and its
// revision fence) against the same read before it ends them.
func Sweep[V any](marks *Marks, entries map[string]V, removed func(key string, value V)) {
	for key, value := range entries {
		if marks.Marked(key) {
			continue
		}
		delete(entries, key)
		if removed != nil {
			removed(key, value)
		}
	}
}
