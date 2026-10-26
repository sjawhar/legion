package kvwatch

// Marks is the set of keys a cache has evidence of from the current read: the keys that read
// delivered, and those a write-through, read-through or delete of the current incarnation touched.
// A cache resets it in BeginRead, marks a key on each piece of such evidence, and once the read
// completes removes every entry the read left unmarked (Sweep): the bucket no longer holds it. A
// key is marked before any revision fence is compared, so a delivered value older than a local
// write still keeps that key through the sweep.
//
// The zero value is an empty set. Marks has no lock of its own: the cache uses it under the mutex
// that guards its entries, which every cache hook takes.
type Marks struct {
	keys map[string]struct{}
}

// Reset forgets every mark, for a read that begins.
func (m *Marks) Reset() {
	clear(m.keys)
}

// Mark records that the current read has evidence of key.
func (m *Marks) Mark(key string) {
	if m.keys == nil {
		m.keys = map[string]struct{}{}
	}
	m.keys[key] = struct{}{}
}

// Marked reports whether the current read has evidence of key.
func (m *Marks) Marked(key string) bool {
	_, ok := m.keys[key]
	return ok
}

// Sweep deletes from entries every key marks does not hold, handing each removed key and value to
// removed when it is not nil (a cache records there what it records for an expired entry). It
// leaves marks as they are, so a cache sweeps each map it keeps per key (its entries and its
// revision fence) against the same read before it resets them.
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
