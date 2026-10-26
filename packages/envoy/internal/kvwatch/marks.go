package kvwatch

// marks is the set of keys a copy has evidence of from the current read: the keys that read
// delivered, and those a write-through, read-through or delete of the current incarnation touched.
// A copy begins it in BeginRead, marks a key on each piece of such evidence, and once the read
// completes removes every entry the read left unmarked (sweep) and ends it: the bucket no longer
// holds those entries. A key is marked before any revision fence is compared, so a delivered value
// older than a local write still keeps that key through the sweep.
//
// Marks are collected only while a read is under way: between a completed read and the next
// BeginRead nothing reads them, so mark records nothing then, and a copy that follows its bucket
// keeps no set of every key it has seen. A read that times out or ends before its end marker never
// ends its marks, which stay collected until the next read begins.
//
// The zero value is an empty set that collects nothing. It has no lock of its own: Copy uses it
// under the mutex of the cache that keeps the copy.
type marks struct {
	keys   map[string]struct{}
	active bool
}

// begin forgets every mark and starts collecting them, for a read that begins.
func (m *marks) begin() {
	clear(m.keys)
	m.active = true
}

// end stops collecting marks and drops the set, once a completed read has swept against it.
func (m *marks) end() {
	m.keys = nil
	m.active = false
}

// mark records that the current read has evidence of key. With no read under way it records
// nothing.
func (m *marks) mark(key string) {
	if !m.active {
		return
	}
	if m.keys == nil {
		m.keys = map[string]struct{}{}
	}
	m.keys[key] = struct{}{}
}

// marked reports whether the current read has evidence of key.
func (m *marks) marked(key string) bool {
	_, ok := m.keys[key]
	return ok
}

// sweep deletes from entries every key marks does not hold, handing each removed key and value to
// removed when it is not nil. It leaves marks as they are, so a copy sweeps each map it keeps per
// key (its entries and its revision fence) against the same read before it ends them.
func sweep[V any](marks *marks, entries map[string]V, removed func(key string, value V)) {
	for key, value := range entries {
		if marks.marked(key) {
			continue
		}
		delete(entries, key)
		if removed != nil {
			removed(key, value)
		}
	}
}
