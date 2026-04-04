package session

import (
	"sync"
	"time"
)

// Dedupe tracks recently seen dedupe keys to prevent duplicate delivery.
type Dedupe struct {
	mu      sync.Mutex
	seen    map[string]time.Time
	window  time.Duration
}

// NewDedupe creates a deduplicator with the given time window.
func NewDedupe(window time.Duration) *Dedupe {
	return &Dedupe{
		seen:   make(map[string]time.Time),
		window: window,
	}
}

// Check returns true if this key was already seen within the window.
// If not seen, records it and returns false.
func (d *Dedupe) Check(key string) bool {
	if key == "" {
		return false // no key = no dedup
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now()

	// Lazy cleanup: remove expired entries
	for k, t := range d.seen {
		if now.Sub(t) > d.window {
			delete(d.seen, k)
		}
	}

	if _, exists := d.seen[key]; exists {
		return true // duplicate
	}
	d.seen[key] = now
	return false
}
