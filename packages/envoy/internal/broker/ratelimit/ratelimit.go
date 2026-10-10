// Package ratelimit is the broker's token buckets: per key (a source address, a named operator),
// so a flood under one key never spends another's, and one every caller shares.
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

// Every is how often each of k's buckets gains a request back: how long a refused caller waits.
func (k *Keyed) Every() time.Duration { return k.limit.Every }

// Allow takes one request from key's bucket now, reporting false when it has none left.
func (k *Keyed) Allow(key string) bool {
	return k.AllowAt(key, time.Now())
}

// AllowAt takes one request from key's bucket at now, reporting false when it has none left.
func (k *Keyed) AllowAt(key string, now time.Time) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.bucket(key, now).AllowN(now, 1)
}

// AllowBothAt takes one request from key's bucket and one from shared at now when each has one
// left, reporting true. Otherwise it takes from neither and answers false with the limit that
// refused, key's bucket's first: a request one bucket refuses spends nothing of the other, so a
// key flooding past its own limit leaves shared to everyone else, and a request shared refuses
// leaves its key's bucket as it was. Key's bucket is taken from only under k's lock, so a request
// shared allows always finds it as it was checked.
func (k *Keyed) AllowBothAt(key string, shared *Bucket, now time.Time) (refused Limit, ok bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	bucket := k.bucket(key, now)
	if bucket.TokensAt(now) < 1 {
		return k.limit, false
	}
	if !shared.bucket.AllowN(now, 1) {
		return shared.limit, false
	}
	bucket.AllowN(now, 1)
	return Limit{}, true
}

// bucket is key's bucket, made full when key has none. The caller holds k.mu.
func (k *Keyed) bucket(key string, now time.Time) *rate.Limiter {
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
	return bucket
}

// Bucket is one Limit every caller shares: a bound on the work all of them together can cause.
type Bucket struct {
	limit  Limit
	bucket *rate.Limiter
}

// NewBucket is one bucket of limit, full.
func NewBucket(limit Limit) *Bucket {
	return &Bucket{limit: limit, bucket: rate.NewLimiter(rate.Every(limit.Every), limit.Burst)}
}
