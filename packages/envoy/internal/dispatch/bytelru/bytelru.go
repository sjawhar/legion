// Package bytelru holds the byte-bounded least-recently-used cache Dispatch keeps immutable
// encodings in: the display trees a read is shown as (pmdoc) and the dashboard assets another
// build's tabs ask for (routes). Both key by content - a hash of the markdown, a hashed build
// file name - so an entry never goes stale and only ever leaves to make room for another.
//
// The bound is on the bytes of the values themselves and nothing else: not the keys, not the
// list's own links. That is what each package's own bound promises its reader.
package bytelru

import (
	"container/list"
	"sync"
)

// entry is one kept value with the key that finds it, so eviction can drop the map's entry from
// the list's own element.
type entry[K comparable, V ~[]byte] struct {
	key   K
	value V
}

// Cache is the values kept by their key, least recently used first out, holding at most the
// limit it was made with. Every method takes the cache's lock, so one cache serves every
// goroutine of a process.
type Cache[K comparable, V ~[]byte] struct {
	limit int64

	mu    sync.Mutex
	held  int64
	items map[K]*list.Element
	order *list.List // of *entry[K, V], the most recently used at the front
}

// New is an empty cache holding at most limit bytes of values.
func New[K comparable, V ~[]byte](limit int64) *Cache[K, V] {
	return &Cache[K, V]{limit: limit, items: make(map[K]*list.Element), order: list.New()}
}

// Get is the value kept under key, marked the most recently used. The value is shared, never
// copied: a caller must not write to it.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, held := c.items[key]
	if !held {
		var missing V
		return missing, false
	}
	c.order.MoveToFront(element)
	return element.Value.(*entry[K, V]).value, true
}

// Put keeps value under key and drops the least recently used entries until what is held fits
// the limit. A value larger than the whole limit is never kept, so one enormous one cannot empty
// the cache for everybody else; that same bound leaves the list holding at least two entries
// whenever the eviction loop runs, so its back is always an entry. A key already held is only
// marked used again, since a caller keys by content and the value under one key never changes.
func (c *Cache[K, V]) Put(key K, value V) {
	size := int64(len(value))
	if size > c.limit {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, held := c.items[key]; held {
		c.order.MoveToFront(element)
		return
	}
	c.items[key] = c.order.PushFront(&entry[K, V]{key: key, value: value})
	c.held += size
	for c.held > c.limit {
		evicted := c.order.Remove(c.order.Back()).(*entry[K, V])
		delete(c.items, evicted.key)
		c.held -= int64(len(evicted.value))
	}
}

// Held is the bytes of the values the cache holds now, which is what its limit bounds.
func (c *Cache[K, V]) Held() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.held
}
