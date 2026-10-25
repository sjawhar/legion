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
//
// The rate is one every eight seconds rather than the one every two it was, because this route
// takes no credential at all and ratelimit.Keyed.AllowBothAt charges a refusal to neither
// bucket: a caller that never floods, and merely keeps succeeding at the rate the broker-wide
// bucket refills, holds the whole fleet's reread budget at zero and is never refused itself. An
// emptied broker-wide bucket is the one thing this fan-out exists to prevent - while it is empty
// no narrowing retag reaches any pod before that pod's own five-minute reload - so one address
// has to sustain strictly less than the shared bucket refills, and by enough that it takes more
// than one of them. The rule this rate is derived from, rather than a figure of its own: it
// refills at half the rate of the tighter of the two shared buckets, which is Secret Manager's,
// so it always takes at least two addresses to hold the fleet's budget at zero whatever the
// replica count. At two pods that is Every = 2 × GCPRereadOverallLimit.Every = 8 s, 0.125 a
// second, against a shared 0.25 a second on Secret Manager and 1 a second on the default store:
// two addresses to starve the first and eight to starve the second, which is exactly the
// ordering this route had before the fan-out existed, when 0.5 a second per address stood
// against a shared 1 and 4 a second. At four pods the two shared rates are divided by sixteen
// rather than by four (0.0625 and 0.25 a second), so this one follows them down to Every = 32 s:
// all three constants are re-derived together when the replica count changes, and whether the
// ordering still holds is what says that it was done.
//
// What that costs: past its burst of 30, one address writing secrets in bulk now waits eight
// seconds between writes rather than two. What it does not fix: one address can still spend the
// shared bucket's whole burst at once, as it always could, because a burst is a depth and the
// per-address burst has always been the larger of the two. What this rate orders is the
// sustained share, which is the only thing a caller that merely has to keep succeeding lives on.
var DefaultRereadLimit = ratelimit.Limit{Every: 8 * time.Second, Burst: 30}

// DefaultRereadOverallLimit bounds POST /v1/secrets/{name}/reread across every caller at once,
// whatever address each comes from. Every reread takes the policy's writer lock for one
// DescribeSecret, so a per-address limit alone bounds nothing a flood spread over addresses
// cannot pass: with no cap of its own it could keep that lock busy and hold up the five-minute
// reload and the miss-path rereads a session's own request waits on.
//
// This bucket is per process, and every reread it accepts costs the fleet one read of the secret
// store per pod rather than one in total: the pod that served the call reads the store, and the
// fan-out (Deps.NotifyReread) makes every peer read it too. So the fleet's sustained store
// traffic is replicas × this rate × replicas - replicas² × this rate - and the rate is sized by
// dividing the whole fleet's budget by the replica count squared. The budget divided here is
// four rereads a second. It is not the provider's quota: AWS Secrets Manager serves
// DescribeSecret at 40,000 requests a second per Region
// (docs.aws.amazon.com/general/latest/gr/asm.html), which nothing this route could do would
// approach. What four a second is, is the policy writer lock above - one pod's worth of reread
// traffic that still leaves that lock free for the five-minute reload and the miss-path
// rereads - so the budget is the broker's own contention and not an external cap, and a
// deployment that wants more of it is changing a local trade rather than asking a provider for
// quota. The deployment runs two pods, so each pod gets a quarter of it:
// 2 pods × 1 reread/s × 2 store reads = 4 store reads a second, the budget. Read that as the
// arithmetic rather than as the number two - a third replica divides the fleet's budget by nine,
// not by four.
//
// The burst is divided by the replica count and not by its square, and that difference is
// deliberate. A burst is a depth rather than a rate, and the depth this one has to admit is one
// person's write burst, which does not grow with the replica count: a person who writes three
// secrets at once writes three whatever the fleet's size. What does grow with the replica count
// is the transient on each pod's own policy writer lock, since the fan-out makes every pod serve
// its own burst B and each of its peers' as well, N × B in all; B = 10 / N = 5 puts that
// transient back at the depth of ten one pod always had. Dividing the burst by the square
// instead protected nothing the rate does not already protect and cost a person their third
// consecutive write: a whole-fleet transient of 40 metadata reads is four seconds of Secret
// Manager's 600 a minute, and the sustained rate re-bounds the traffic the instant the burst is
// spent. What pins this floor is cmd/agent-secrets/secret_e2e_test.go's
// TestSecretDeleteAndRestoreAreServedByTheBrokerAtOnce, which drives the real client through
// three consecutive writes as a person does - delete, restore, retag - each sending its own
// reread, and goes red at any burst below three. It is not a unit test of this constant but the
// product's own promise that a write is served at once rather than at the next reload, so
// anyone tightening this number meets it.
//
// A reread another pod's notification drives takes no token from this bucket, since the pod that
// served the original call already paid for it - which is what makes the arithmetic above the
// whole of the fleet's store traffic rather than a lower bound on it. A caller that does meet
// this bucket is told to come back in one second (Retry-After follows Every,
// retryAfterSeconds), and its write stands in the store either way; only the narrowing of who
// may read it waits for the next token.
var DefaultRereadOverallLimit = ratelimit.Limit{Every: time.Second, Burst: 5}

// GCPRereadOverallLimit is DefaultRereadOverallLimit for the Google Secret Manager store, which
// unlike the default store does bound this route by a provider's quota: Secret Manager serves
// 600 read requests a minute per project (cloud.google.com/secret-manager/quotas, "Read
// request"), the whole project shares them, and one reread spends two of them (the secret and
// its version). The budget divided here is a fifth of that quota - 120 metadata reads a minute,
// one reread a second - which is what one pod alone was allowed; the other four fifths are what
// every pod's five-minute namespace reload, its miss-path rereads and its releases spend, and
// the fan-out multiplies those too, so this route's share is not the place to find more room.
// Its rate is divided by the replica count squared for the same reason as the default bucket's
// - each accepted reread costs a store read on every pod, so the fleet spends replicas² × this
// rate - which for two pods is 2 pods × 0.25 reread/s × 2 store reads × 2 metadata reads =
// 2 metadata reads a second, the same 120 a minute. A third replica divides that budget by nine
// rather than by four.
//
// Its burst is the default bucket's, 10 / N = 5: divided by the replica count and not by its
// square for the reason given there, that the depth a burst has to admit is one person's write
// burst and the replica count does not change how many secrets a person writes at once. The
// whole fleet's transient at that depth is 2 pods × 5 rereads × 2 store reads × 2 metadata reads
// = 40 metadata reads, four seconds of the shared 600 a minute, after which the rate above holds
// the traffic down again.
var GCPRereadOverallLimit = ratelimit.Limit{Every: 4 * time.Second, Burst: 5}

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
