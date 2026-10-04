// Package ratelimit is the broker's per-key token buckets: every key (a source address, a named
// operator) has a bucket of its own, so a flood under one key never spends another's.
package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limit is a token bucket: Burst requests at once, refilled at one every Every.
type Limit struct {
	Every time.Duration
	Burst int
}

// maxBuckets bounds a Keyed's memory: past it, full buckets (indistinguishable from fresh ones)
// are dropped.
const maxBuckets = 10000

// Keyed is one Limit applied to each key separately.
type Keyed struct {
	limit   Limit
	mu      sync.Mutex
	buckets map[string]*rate.Limiter
}

// NewKeyed gives every key its own bucket of limit.
func NewKeyed(limit Limit) *Keyed {
	return &Keyed{limit: limit, buckets: map[string]*rate.Limiter{}}
}

// Allow takes one request from key's bucket now, reporting false when it has none left.
func (k *Keyed) Allow(key string) bool {
	return k.AllowAt(key, time.Now())
}

// AllowAt takes one request from key's bucket at now, reporting false when it has none left.
func (k *Keyed) AllowAt(key string, now time.Time) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	bucket, ok := k.buckets[key]
	if !ok {
		if len(k.buckets) >= maxBuckets {
			for other, b := range k.buckets {
				if b.TokensAt(now) >= float64(k.limit.Burst) {
					delete(k.buckets, other)
				}
			}
		}
		bucket = rate.NewLimiter(rate.Every(k.limit.Every), k.limit.Burst)
		k.buckets[key] = bucket
	}
	return bucket.AllowN(now, 1)
}
