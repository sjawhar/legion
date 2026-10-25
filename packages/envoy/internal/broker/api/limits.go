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

// DefaultIntrospectLimit bounds POST /v1/oauth/introspect per source address, in a bucket of its
// own: a burst of 200 and fifty a second. The route is authenticated, so this bounds a listed
// caller rather than a flood — a resource server asks about one token per request it serves, with
// its own short cache in front, and fifty a second is well past what one such server asks and far
// below what would keep the broker's storage busy.
var DefaultIntrospectLimit = ratelimit.Limit{Every: 20 * time.Millisecond, Burst: 200}

// DefaultRereadBurst is the depth of every per-address bucket on POST
// /v1/secrets/{name}/reread: a person's write burst passes; a flood cannot make the broker
// hammer DescribeSecret. The bucket is per process, so a fleet of N broker pods allows one
// address N times it; what bounds the fleet's reread traffic is DefaultRereadOverallLimit
// below, which is sized for the replica count.
//
// A depth is not where the ordering between one address and the whole fleet is settled, and
// this one does not try to settle it: one address can still spend the shared bucket's whole
// burst at once, as it always could, because the per-address depth has always been the larger
// of the two. What orders the two is the sustained rate RereadLimitFor derives, which is the
// only thing a caller that merely has to keep succeeding lives on.
const DefaultRereadBurst = 30

// perAddressFactor is how much slower one address's reread bucket refills than the shared
// bucket the process mounts: RereadLimitFor multiplies that bucket's Every by it. Two is the
// smallest factor that makes holding the fleet's reread budget at zero take more than one
// address, whichever store is mounted; what it costs, and why it is not larger, is in
// RereadLimitFor's comment.
const perAddressFactor = 2

// RereadLimitFor answers the per-address reread limit for a process that mounts overall as its
// broker-wide bucket: one depth for everyone (DefaultRereadBurst), refilled perAddressFactor
// times slower than the bucket this process actually mounts.
//
// It is a derivation rather than a constant because the two deployments do not mount the same
// shared bucket: cmd/broker mounts GCPRereadOverallLimit only under gcp-secret-manager and
// DefaultRereadOverallLimit otherwise, so no single constant serves both. One derived from the
// Google figure tightens every AWS deployment fourfold - one write every eight seconds against
// a shared bucket of its own that refills every second - for a Secret Manager quota that
// deployment never touches. One derived from the default figure leaves a single address on
// Secret Manager able to hold that fleet's whole reread budget at zero.
//
// What the ordering buys is the fan-out itself. This route takes no credential at all, and
// ratelimit.Keyed.AllowBothAt charges a refusal to neither bucket, so a caller that never
// floods and merely keeps succeeding at the rate the shared bucket refills holds that bucket
// at zero for ever and is never refused itself. While it is empty no narrowing retag reaches
// any pod before that pod's own five-minute reload, which is the one thing this fan-out exists
// to prevent. So one address has to sustain strictly less than the shared bucket refills, and
// by enough that holding it down takes more than one address.
//
// The factor of two is exactly that "more than one", and it is the same two on both stores: 0.5
// a second per address against a shared 1 a second on the default store, 0.125 against a shared
// 0.25 on Secret Manager, so either store takes two addresses to starve whatever the replica
// count. On the default store that is a weakening, and it is written down as one. It used to
// take eight addresses there, but that eight was never designed: it fell out of two constants
// chosen independently of each other, one per-address and one shared. Buying it back means
// refilling an address's bucket every eight seconds on a store whose shared bucket refills
// every second, which charges every honest bulk writer on that store a fourfold tightening -
// eight seconds between writes past the burst rather than two - for the difference between two
// attackers and eight on an endpoint that asks for no credential. That trade was ruled not
// worth making.
//
// Reading the rate off the mounted bucket also leaves no third constant to re-derive when the
// replica count changes: both shared buckets are sized by the replica count squared (below),
// and this follows whichever of them the process mounted down with it.
func RereadLimitFor(overall ratelimit.Limit) ratelimit.Limit {
	return ratelimit.Limit{Every: perAddressFactor * overall.Every, Burst: DefaultRereadBurst}
}

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
// The rate and the burst are two rules rather than one rule and a special case of it, and they
// are written apart here because each has its own reason. The rate divides by the replica count
// squared, as above, because every reread this bucket accepts is sustained traffic: it costs a
// store read on every pod for as long as the caller goes on paying for it, and sustained traffic
// is the thing a provider's quota counts. The burst does not divide at all - it stays at the
// budget's ten however many pods run - because what a burst has to admit is one person's write
// burst, which is fixed in the replica count (a person writing four secrets at once writes four
// whatever the fleet's size), while what it guards is a transient on each pod's own policy
// writer lock, which costs latency against the reload and the miss-path rereads rather than
// anyone's quota. One side is a hard floor and the other is a soft cost, and sizing by the soft
// one is the error this corrects: the burst was 10 / N, which made a floor that cannot depend on
// the replica count depend on it.
//
// That floor is four, and it is measured rather than argued.
// cmd/agent-secrets/secret_e2e_test.go's TestSecretDeleteAndRestoreAreServedByTheBrokerAtOnce
// drives the real client through one person's own sequence and sends four rereads: the delete,
// the restore, the retag, and the NO_VALUE_KEY retag that expects the broker's no-current-value
// refusal and meets RATE_LIMITED instead once this bucket is spent. An overlay changing only
// this burst and nothing else measures 2 FAIL, 3 FAIL, 4 ok, 5 ok against that live row, so ten
// carries six tokens of headroom over the floor. The row is not a unit test of this constant but
// the product's own promise that a write is served at once rather than at the next reload, which
// is what makes it this burst's pin: anyone tightening the number meets it.
//
// What the undivided depth costs is two quantities and never one. A burst momentarily lets the
// fleet accept replicas × burst rereads, and each accepted reread costs a store read on every
// pod, so the fleet's momentary store cost is replicas² × burst: at a burst of ten that is 20
// accepted and 40 store reads at two pods, 30 and 90 at three, 40 and 160 at four. On Secret
// Manager (GCPRereadOverallLimit) each store read is two metadata reads, so those store figures
// double to 80, 180 and 320 metadata reads against the 600 a minute that comment cites - 8, 18
// and 32 seconds of that quota, which is the form to judge them in. The protection belongs
// beside the cost rather than a paragraph away from it: 320 is a transient, spent once, and the
// sustained rate re-bounds the traffic the instant the burst is gone, which is the half of the
// pair that does the protecting.
//
// The momentary store cost grows with the square of the replica count while the sustained rate
// is already divided by it, so there is a replica count above which this shape stops making
// sense, and nobody has had to find where it is because the fleet is two.
//
// Recorded so it is not re-proposed: max(budget/replicas, personWriteBurst) is correct and still
// not worth writing, because its two branches differ at exactly one deployment shape - 10/3 is
// already under the floor of four, so from three pods upward the maximum is the floor - which
// makes the divisor a rule no deployment past two pods ever uses, dressed as a rule that governs
// every one of them.
//
// A reread another pod's notification drives takes no token from this bucket, since the pod that
// served the original call already paid for it - which is what makes the arithmetic above the
// whole of the fleet's store traffic rather than a lower bound on it. A caller that does meet
// this bucket is told to come back in one second (Retry-After follows Every,
// retryAfterSeconds), and its write stands in the store either way; only the narrowing of who
// may read it waits for the next token.
var DefaultRereadOverallLimit = ratelimit.Limit{Every: time.Second, Burst: 10}

// GCPRereadOverallLimit is DefaultRereadOverallLimit for the Google Secret Manager store, which
// unlike the default store does bound this route by a provider's quota: Secret Manager serves
// 600 read requests a minute per project (cloud.google.com/secret-manager/quotas, "Read
// request"), the whole project shares them, and one reread spends two of them (the secret and
// its version). The budget divided here is a fifth of that quota - 120 metadata reads a minute,
// one reread a second - which is what one pod alone was allowed; the other four fifths are what
// every pod's five-minute namespace reload, its miss-path rereads and its releases spend. Those
// are multiplied by running more pods, not by the fan-out: each pod spends its own, and none of
// them notifies - only this route's reread does, while a miss-path reread, a reload and a release
// send nothing - so they too grow with the replica count, and this route's share is not the place
// to find more room.
// Its rate is divided by the replica count squared for the same reason as the default bucket's
// - each accepted reread costs a store read on every pod, so the fleet spends replicas² × this
// rate - which for two pods is 2 pods × 0.25 reread/s × 2 store reads × 2 metadata reads =
// 2 metadata reads a second, the same 120 a minute. A third replica divides that budget by nine
// rather than by four.
//
// Its burst is the default bucket's ten, undivided by the replica count for the reason given
// there: the depth a burst admits is one person's write burst, which the replica count does not
// change, and what the depth guards is this pod's own writer lock rather than the project's
// quota. The whole fleet's transient at that depth is 2 pods × 10 rereads × 2 store reads × 2
// metadata reads = 80 metadata reads, eight seconds of the shared 600 a minute, after which the
// rate above holds the traffic down again.
var GCPRereadOverallLimit = ratelimit.Limit{Every: 4 * time.Second, Burst: 10}

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
