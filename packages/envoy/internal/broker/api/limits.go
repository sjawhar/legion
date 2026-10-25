package api

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/broker/ratelimit"
)

// LauncherLimits bounds POST /v1/oauth/device_authorization and POST
// /v1/launcher-credentials/token, the login routes an unauthenticated caller can use to make the
// broker do work on their behalf. Every source address has its own token bucket (PerAddress), and
// so does every person a machine login's login_hint names and every account a verified token login
// proves (PerLogin). A token login spends its address's bucket before its token is verified, since
// a refused token signature makes the verifier fetch the issuer's keys again, and its account's
// only after; a device login spends its address's bucket before the provider does any work, then
// verifies its request object locally to read the person its bucket is keyed on and spends that.
// So a flood from one address cannot keep the broker verifying tokens or writing machine logins, a
// flood naming one operator cannot bury that operator's pending machine logins, and one account
// cannot log in faster than its bucket allows. A daemon behind an address others share is held to
// that address's bucket too: BROKER_TRUSTED_PROXY_HEADER keys the bucket on the caller's own
// address behind a proxy, and the daemon renews its credential halfway through its life, which
// carries it through brief contention for that bucket, not through a caller that keeps it drained
// until the credential expires.
//
// The per-login bucket, keyed on the request object's own claims rather than the caller's
// address, is unaffected by trustedProxyHeader and remains a smaller, accepted risk. A person's
// machine login spends its own person:<login> bucket, so an attacker naming a specific victim
// operator repeatedly can lock out that operator's machine logins at a low rate. This is inherent
// to a per-login limit on an unauthenticated route.
type LauncherLimits struct {
	PerAddress ratelimit.Limit
	PerLogin   ratelimit.Limit
}

// DefaultLauncherLimits allows a burst of ten requests per source address and five per login,
// refilled at two a minute and one a minute: generous for a person logging in a few machines,
// useless for a flood. Both buckets live in one process, so a fleet of N broker pods allows N
// times each; they are left where they are because a login flood costs the broker its own work
// rather than a shared external quota, and because the load balancer spreads one address's flood
// over the pods instead of handing it all to one.
var DefaultLauncherLimits = LauncherLimits{
	PerAddress: ratelimit.Limit{Every: 30 * time.Second, Burst: 10},
	PerLogin:   ratelimit.Limit{Every: time.Minute, Burst: 5},
}

// DefaultTokenLimit bounds POST /v1/oauth/token per source address, in a bucket of its own: a
// burst of 60 and two a second, per process, so a fleet of N broker pods allows N times it. A
// machine polls its device code every five seconds while its person decides and refreshes about
// once a day, so even many machines behind one address never meet it; a flood from one address is
// refused before the provider does any work.
var DefaultTokenLimit = ratelimit.Limit{Every: 500 * time.Millisecond, Burst: 60}

// DefaultRereadLimit bounds POST /v1/secrets/{name}/reread per source address: a person's write
// burst passes; a flood cannot make the broker hammer DescribeSecret. The bucket is per process,
// so a fleet of N broker pods allows one address N times it; what bounds the fleet's reread
// traffic is DefaultRereadOverallLimit below, which is sized for the replica count.
var DefaultRereadLimit = ratelimit.Limit{Every: 2 * time.Second, Burst: 30}

// DefaultRereadOverallLimit bounds POST /v1/secrets/{name}/reread across every caller at once,
// whatever address each comes from. Every reread takes the policy's writer lock for one
// DescribeSecret, so a per-address limit alone bounds nothing a flood spread over addresses
// cannot pass: with no cap of its own it could keep that lock busy and hold up the five-minute
// reload and the miss-path rereads a session's own request waits on.
//
// This bucket is per process, and every reread it accepts costs the fleet one read of the secret
// store per pod rather than one in total: the pod that served the call reads the store, and the
// fan-out (Deps.NotifyReread) makes every peer read it too. So the fleet's store traffic is
// replicas × this rate × replicas - replicas² × this rate - and the bucket is sized by dividing
// the whole fleet's budget by the replica count squared, in rate and in burst alike. The budget
// divided here is four rereads a second and a burst of ten, which is what one pod alone was
// allowed against a store where a reread is one read; the deployment runs two pods, so each pod
// gets a quarter of it: 2 pods × 1 reread/s × 2 store reads = 4 store reads a second, the budget.
// Read that as the arithmetic rather than as the number two - a third replica divides the fleet's
// budget by nine, not by four.
//
// What the smaller burst costs a person: agent-secrets secret writing several secrets at once
// meets this broker-wide 429 after two of them, sooner than it did, and is told to come back in
// one second (Retry-After follows Every, retryAfterSeconds). The write itself stands in the store
// either way; only the narrowing of who may read it waits for the next token. A reread another
// pod's notification drives takes no token from this bucket, since the pod that served the
// original call already paid for it - which is what makes the arithmetic above the whole of the
// fleet's store traffic rather than a lower bound on it.
var DefaultRereadOverallLimit = ratelimit.Limit{Every: time.Second, Burst: 2}

// GCPRereadOverallLimit is DefaultRereadOverallLimit for the Google Secret Manager store, where a
// reread costs two reads against the 600-a-minute metadata quota the whole project shares. The
// budget divided here is one reread a second and a burst of ten: the 120 metadata reads a minute
// the public route may spend of that shared 600, which is what one pod alone was allowed. It is
// divided by the replica count squared for the same reason as the default bucket - each accepted
// reread costs a store read on every pod, so the fleet spends replicas² × this rate - which for
// two pods is 2 pods × 0.25 reread/s × 2 store reads × 2 metadata reads = 2 metadata reads a
// second, the same 120 a minute. A third replica divides that budget by nine rather than by four.
//
// Here too the burst of two is what a person meets first: agent-secrets secret writing several
// secrets at once is refused broker-wide after two and told to come back in four seconds, while
// each write stands in the store regardless.
var GCPRereadOverallLimit = ratelimit.Limit{Every: 4 * time.Second, Burst: 2}

type launcherLimiter struct {
	perAddress, perLogin *ratelimit.Keyed
	// trustedProxyHeader is BROKER_TRUSTED_PROXY_HEADER: empty means every caller reaches the
	// broker directly, so perAddress keys on r.RemoteAddr. Set only behind a trusted reverse
	// proxy that itself sets this header on every forwarded request (see clientAddress).
	trustedProxyHeader string
}

func newLauncherLimiter(limits LauncherLimits, trustedProxyHeader string) *launcherLimiter {
	return &launcherLimiter{
		perAddress:         ratelimit.NewKeyed(limits.PerAddress),
		perLogin:           ratelimit.NewKeyed(limits.PerLogin),
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

// refuseAddress writes 429 RATE_LIMITED and reports true when r's source address has no request
// left in its bucket: POST /v1/launcher-credentials/token's first limit, spent before the token is
// verified.
func (l *launcherLimiter) refuseAddress(w http.ResponseWriter, r *http.Request) bool {
	if l.addressAllows(r) {
		return false
	}
	refuseWithRetryAfter(w, l.perAddress.Every(), "too many launcher credential requests; try again later")
	return true
}

// refuseAccount writes 429 RATE_LIMITED and reports true when subject, an account a token login
// has just verified, has no login left in its account:<subject> bucket. Only a verified token
// spends it, after its address's bucket.
func (l *launcherLimiter) refuseAccount(w http.ResponseWriter, subject string) bool {
	if l.perLogin.AllowAt("account:"+subject, time.Now()) {
		return false
	}
	refuseWithRetryAfter(w, l.perLogin.Every(), "too many token logins for this ServiceAccount; try again later")
	return true
}

// addressAllows spends one request of r's source address's bucket, reporting whether it had one.
func (l *launcherLimiter) addressAllows(r *http.Request) bool {
	return l.perAddress.AllowAt(clientAddress(r, l.trustedProxyHeader), time.Now())
}

// tokenAllows spends one request of r's source address's token-endpoint bucket, reporting
// whether it had one.
func (s *server) tokenAllows(r *http.Request) bool {
	return s.tokenLimiter.AllowAt(clientAddress(r, s.deps.TrustedProxyHeader), time.Now())
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
// every names (retryAfterSeconds).
func refuseWithRetryAfter(w http.ResponseWriter, every time.Duration, msg string) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(every)))
	writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", msg)
}

// retryAfterSeconds is the Retry-After a bucket refilled one every every names: whole seconds
// rounded up, and never 0, which would invite an immediate retry the bucket would refuse again.
func retryAfterSeconds(every time.Duration) int {
	return max(int((every+time.Second-1)/time.Second), 1)
}
