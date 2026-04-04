// Package dedupe provides a time-windowed cache for suppressing duplicate
// envelope delivery. The cache is keyed by (DedupeKey, SessionID) so the same
// envelope can be delivered to different sessions independently.
package dedupe

import (
	"sync"
	"time"
)

// Key identifies a unique delivery: same envelope to same session.
type Key struct {
	DedupeKey string
	SessionID string
}

// Cache tracks recently delivered (dedupe_key, session_id) pairs to prevent
// duplicate delivery during NATS retry storms. Thread-safe via sync.RWMutex.
type Cache struct {
	mu      sync.RWMutex
	entries map[Key]time.Time
	ttl     time.Duration
	clock   func() time.Time
	stop    chan struct{}
}

// New creates a dedupe cache with the given TTL and starts a background
// cleanup goroutine that periodically removes expired entries.
func New(ttl time.Duration) *Cache {
	return NewWithClock(ttl, time.Now)
}

// NewWithClock creates a dedupe cache with an injectable clock for testing.
func NewWithClock(ttl time.Duration, clock func() time.Time) *Cache {
	c := &Cache{
		entries: make(map[Key]time.Time),
		ttl:     ttl,
		clock:   clock,
		stop:    make(chan struct{}),
	}
	go c.cleanup()
	return c
}

// Seen returns true if the (dedupeKey, sessionID) pair was delivered within
// the TTL window. Does not modify state — call Record after successful delivery.
func (c *Cache) Seen(dedupeKey, sessionID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.entries[Key{DedupeKey: dedupeKey, SessionID: sessionID}]
	if !ok {
		return false
	}
	return c.clock().Sub(t) <= c.ttl
}

// Record marks a (dedupeKey, sessionID) pair as delivered.
// Call only after successful delivery to preserve at-least-once semantics.
func (c *Cache) Record(dedupeKey, sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[Key{DedupeKey: dedupeKey, SessionID: sessionID}] = c.clock()
}

// Len returns the number of entries currently in the cache.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// Stop halts the background cleanup goroutine.
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
