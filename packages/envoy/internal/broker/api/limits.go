package api

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// LauncherLimits bounds POST /v1/launcher-credentials, the one route an unauthenticated caller
// can use to make the broker call Dispatch. Every source address and every named operator has its
// own token bucket: a flood from one address cannot keep the broker busy opening asks, and a
// flood naming one operator cannot bury that operator's standing issue in asks.
type LauncherLimits struct {
	PerAddress  Limit
	PerOperator Limit
}

// Limit is a token bucket: Burst requests at once, refilled at one every Every.
type Limit struct {
	Every time.Duration
	Burst int
}

// DefaultLauncherLimits allows a burst of ten requests per source address and five per operator,
// refilled at two a minute and one a minute: generous for a person logging in a few launchers,
// useless for a flood.
var DefaultLauncherLimits = LauncherLimits{
	PerAddress:  Limit{Every: 30 * time.Second, Burst: 10},
	PerOperator: Limit{Every: time.Minute, Burst: 5},
}

// maxBuckets bounds a keyedLimiter's memory: past it, full buckets (indistinguishable from fresh
// ones) are dropped.
const maxBuckets = 10000

type keyedLimiter struct {
	limit   Limit
	mu      sync.Mutex
	buckets map[string]*rate.Limiter
}

func newKeyedLimiter(limit Limit) *keyedLimiter {
	return &keyedLimiter{limit: limit, buckets: map[string]*rate.Limiter{}}
}

func (k *keyedLimiter) allow(key string, now time.Time) bool {
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

type launcherLimiter struct {
	perAddress, perOperator *keyedLimiter
	retryAfter              time.Duration
}

func newLauncherLimiter(limits LauncherLimits) *launcherLimiter {
	return &launcherLimiter{
		perAddress:  newKeyedLimiter(limits.PerAddress),
		perOperator: newKeyedLimiter(limits.PerOperator),
		retryAfter:  max(limits.PerAddress.Every, limits.PerOperator.Every),
	}
}

// refuse writes 429 RATE_LIMITED and reports true when r's source address, or the operator it
// names, has no request left in its bucket.
func (l *launcherLimiter) refuse(w http.ResponseWriter, r *http.Request, operator string) bool {
	now := time.Now()
	address, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		address = r.RemoteAddr
	}
	if l.perAddress.allow(address, now) && l.perOperator.allow(operator, now) {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(l.retryAfter.Seconds())))
	writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many launcher credential requests; try again later")
	return true
}
