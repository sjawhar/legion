package api

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/broker/ratelimit"
)

// LauncherLimits bounds POST /v1/launcher-credentials, the one route an unauthenticated caller
// can use to make the broker do work on their behalf. Every source address and every named
// operator has its own token bucket: a flood from one address cannot keep the broker busy
// verifying and recording machine-login requests, and a flood naming one operator cannot bury
// that operator's pending machine logins.
type LauncherLimits struct {
	PerAddress  ratelimit.Limit
	PerOperator ratelimit.Limit
}

// DefaultLauncherLimits allows a burst of ten requests per source address and five per operator,
// refilled at two a minute and one a minute: generous for a person logging in a few launchers,
// useless for a flood.
var DefaultLauncherLimits = LauncherLimits{
	PerAddress:  ratelimit.Limit{Every: 30 * time.Second, Burst: 10},
	PerOperator: ratelimit.Limit{Every: time.Minute, Burst: 5},
}

// DefaultRereadLimit bounds POST /v1/secrets/{name}/reread per source address: a person's write
// burst passes; a flood cannot make the broker hammer DescribeSecret.
var DefaultRereadLimit = ratelimit.Limit{Every: 2 * time.Second, Burst: 30}

// DefaultRereadOverallLimit bounds POST /v1/secrets/{name}/reread across every caller at once,
// whatever address each comes from. Every reread takes the policy's writer lock for one
// DescribeSecret, so a per-address limit alone bounds nothing a flood spread over addresses
// cannot pass: with no cap of its own it could keep that lock busy and hold up the five-minute
// reload and the miss-path rereads a session's own request waits on. Four a second, burst ten:
// well above the rereads a person's writes make (one per secret written, in bursts), and far
// below what would keep the lock busy.
var DefaultRereadOverallLimit = ratelimit.Limit{Every: 250 * time.Millisecond, Burst: 10}

type launcherLimiter struct {
	perAddress, perOperator *ratelimit.Keyed
	// trustedProxyHeader is BROKER_TRUSTED_PROXY_HEADER: empty means every caller reaches the
	// broker directly, so perAddress keys on r.RemoteAddr. Set only behind a trusted reverse
	// proxy that itself sets this header on every forwarded request (see clientAddress).
	trustedProxyHeader string
}

func newLauncherLimiter(limits LauncherLimits, trustedProxyHeader string) *launcherLimiter {
	return &launcherLimiter{
		perAddress:         ratelimit.NewKeyed(limits.PerAddress),
		perOperator:        ratelimit.NewKeyed(limits.PerOperator),
		trustedProxyHeader: trustedProxyHeader,
	}
}

// clientAddress resolves the source address a per-address bucket keys on: the launcher limiter's
// and the reread limiter's. With no trusted proxy header configured (the default, e.g. local/dev
// use or a broker reached directly) it is r.RemoteAddr's host. Behind a reverse proxy or load
// balancer (this broker's documented deployment shape: "the shared internal ALB"), r.RemoteAddr
// as the broker sees it is the SAME address for every real caller — the proxy's — which would
// otherwise collapse every legitimate caller into one shared bucket a single caller can exhaust.
// Configuring the proxy's own forwarding header (e.g. X-Forwarded-For) lets this read the last
// entry — the hop the trusted proxy itself appended — rather than an earlier, client-supplied
// entry a caller could forge to pick its own bucket.
func clientAddress(r *http.Request, trustedProxyHeader string) string {
	if trustedProxyHeader != "" {
		if raw := r.Header.Get(trustedProxyHeader); raw != "" {
			hops := strings.Split(raw, ",")
			if candidate := strings.TrimSpace(hops[len(hops)-1]); candidate != "" {
				return candidate
			}
		}
	}
	address, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return address
}

// refuse writes 429 RATE_LIMITED and reports true when r's source address, or the operator it
// names, has no request left in its bucket, with the Retry-After of the slower of the two buckets.
//
// The per-operator bucket, keyed on the request body's own "operator" field rather than the
// caller's address, is unaffected by trustedProxyHeader and remains a smaller, accepted risk: an
// attacker naming a specific victim operator repeatedly can still lock out that operator's
// launcher logins at a low rate. This is inherent to a per-operator limit on an unauthenticated
// route.
func (l *launcherLimiter) refuse(w http.ResponseWriter, r *http.Request, operator string) bool {
	now := time.Now()
	address := clientAddress(r, l.trustedProxyHeader)
	if l.perAddress.AllowAt(address, now) && l.perOperator.AllowAt(operator, now) {
		return false
	}
	refuseWithRetryAfter(w, max(l.perAddress.Every(), l.perOperator.Every()), "too many launcher credential requests; try again later")
	return true
}

// refuseReread writes 429 RATE_LIMITED and reports true when r's source address, or the broker as
// a whole, has no reread left in its bucket, naming the refusing bucket's Retry-After. A reread
// spends a token in each bucket only when both allow it (ratelimit.Keyed.AllowBothAt): one address
// flooding past its own limit spends nothing of the broker-wide bucket, so it refuses no other
// caller, and a reread the broker-wide bucket refuses spends nothing of its address's own, so a
// caller retrying through a flood is not then refused by its own limit after the flood stops.
func (s *server) refuseReread(w http.ResponseWriter, r *http.Request) bool {
	refused, ok := s.rereadLimiter.AllowBothAt(clientAddress(r, s.deps.TrustedProxyHeader), s.rereadOverall, time.Now())
	if ok {
		return false
	}
	refuseWithRetryAfter(w, refused.Every, "too many secret rereads; try again later")
	return true
}

// refuseWithRetryAfter answers 429 RATE_LIMITED with the Retry-After a bucket refilled one every
// every names: whole seconds rounded up, and never 0, which would invite an immediate retry the
// bucket would refuse again.
func refuseWithRetryAfter(w http.ResponseWriter, every time.Duration, msg string) {
	seconds := max(int((every+time.Second-1)/time.Second), 1)
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", msg)
}
