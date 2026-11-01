package brokertoken

import (
	"crypto/sha256"
	"sync"
	"time"
)

// answerCache remembers the broker's affirmative answers for AffirmativeCacheTTL, so a caller
// sending many requests costs one introspection rather than one a request.
//
// It is keyed on the SHA-256 of the presented token and never holds the token itself: this map
// outlives any one request, and a credential kept in memory for its own lookup key is a
// credential a heap dump carries. It holds no refusal and nothing past the token's own expiry
// (remember), and it is bounded at AffirmativeCacheSize.
type answerCache struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]cachedAnswer
}

// cachedAnswer is one remembered answer and the instant it stops being good.
type cachedAnswer struct {
	answer Introspection
	until  time.Time
}

func newAnswerCache() *answerCache {
	return &answerCache{entries: map[[sha256.Size]byte]cachedAnswer{}}
}

// lookup is the answer remembered for key, if one is and it is still good at now.
func (c *answerCache) lookup(key [sha256.Size]byte, now time.Time) (Introspection, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return Introspection{}, false
	}
	if !entry.until.After(now) {
		delete(c.entries, key)
		return Introspection{}, false
	}
	return entry.answer, true
}

// remember keeps answer for key until AffirmativeCacheTTL from now, or until the token's own
// expiry where that comes first: a token the broker says dies in two seconds is a token Dispatch
// refuses in two seconds, never one its cache keeps alive for five. An answer that is already
// past its expiry is not remembered at all.
//
// A full cache drops what has expired and, if that frees nothing, keeps what it has rather than
// evicting a live entry: the arrival that does not fit pays one introspection, where picking a
// victim at every insert would let a flood of distinct tokens evict the answers of the callers
// actually being served.
func (c *answerCache) remember(key [sha256.Size]byte, answer Introspection, now time.Time) {
	until := now.Add(AffirmativeCacheTTL)
	if expiry := expiryOf(answer); !expiry.IsZero() && expiry.Before(until) {
		until = expiry
	}
	if !until.After(now) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= AffirmativeCacheSize {
		for stored, entry := range c.entries {
			if !entry.until.After(now) {
				delete(c.entries, stored)
			}
		}
		if len(c.entries) >= AffirmativeCacheSize {
			return
		}
	}
	c.entries[key] = cachedAnswer{answer: answer, until: until}
}
