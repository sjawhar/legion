package dedupe

import (
	"sync"
	"time"
)

// Cache is one process's record of the (dedupe key, session) pairs it has confirmed, kept for a
// TTL. It is not the delivery ledger: the ledger is the machine's record of what every lane
// prompted or forwarded, and the listener's publish route keeps this one beside it, because only a
// receipt that arrived through the route may answer a later send of the same key 200 with nothing
// forwarded (cmd/listener/confirmed.go). Thread-safe via sync.RWMutex.
type Cache struct {
	mu      sync.RWMutex
	entries map[Key]time.Time
	ttl     time.Duration
	clock   func() time.Time
	stop    chan struct{}
}

// New makes a cache with the given TTL and starts the background sweep that drops expired entries.
func New(ttl time.Duration) *Cache {
	c := &Cache{
		entries: make(map[Key]time.Time),
		ttl:     ttl,
		clock:   time.Now,
		stop:    make(chan struct{}),
	}
	go c.cleanup()
	return c
}

// Seen reports whether the pair was recorded within the TTL window. It changes nothing: Record is
// the write, made once the delivery is confirmed.
func (c *Cache) Seen(dedupeKey, sessionID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.entries[Key{DedupeKey: dedupeKey, SessionID: sessionID}]
	if !ok {
		return false
	}
	return c.clock().Sub(t) <= c.ttl
}

// Record marks the pair confirmed. Call it only after a confirmed delivery, so nothing is skipped
// that was not taken.
func (c *Cache) Record(dedupeKey, sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[Key{DedupeKey: dedupeKey, SessionID: sessionID}] = c.clock()
}

// Stop ends the background sweep.
func (c *Cache) Stop() {
	close(c.stop)
}

func (c *Cache) cleanup() {
	ticker := time.NewTicker(c.ttl)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.mu.Lock()
			now := c.clock()
			for k, t := range c.entries {
				if now.Sub(t) > c.ttl {
					delete(c.entries, k)
				}
			}
			c.mu.Unlock()
		case <-c.stop:
			return
		}
	}
}
