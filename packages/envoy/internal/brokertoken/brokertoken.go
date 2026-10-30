// Package brokertoken decides a caller that presents an access token the secrets broker issued,
// under the RFC 9449 DPoP scheme: Authorization: DPoP <token> with a DPoP: proof header.
//
// Deciding such a caller is two checks. The broker is asked what the token is (RFC 7662
// introspection, IntrospectPath), and the caller is held to the key that token is bound to (RFC
// 9449 Section 7.1, which the library's own resource check performs). Neither check is Dispatch's
// alone — the Envoy listener takes the same one — so this package lives outside
// internal/dispatch and reaches Dispatch only through the interfaces in Options.
//
// What it does not do is write the reasons down for a browser: Verify answers one of the
// sentinels below and whoever mounts it turns each into a status.
package brokertoken

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"authelia.com/provider/oauth2"
	"authelia.com/provider/oauth2/handler/rfc9449"
	"authelia.com/provider/oauth2/token/jose"
	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/internal/ratelimit"
)

// The outcomes a caller has to tell apart. None of them carries the token presented.
var (
	// ErrNotPresented is a request carrying no token under the DPoP scheme. Whoever mounts this
	// check decides a request by the credential it presents, so this is reached only by a caller
	// that routed one here without a token.
	ErrNotPresented = errors.New("brokertoken: the request presents no access token under the DPoP scheme")
	// ErrNotActive is the broker answering that the token is not live: unknown, revoked or
	// expired. The caller is refused; the answer is never remembered.
	ErrNotActive = errors.New("brokertoken: the broker answered that the token is not live")
	// ErrProofInvalid is a proof that did not meet the RFC 9449 Section 4.3 criteria: a bad
	// signature, a method or target URI that is not this request's, an 'iat' outside the window,
	// a missing or wrong 'ath', or a proof already spent (a replay).
	ErrProofInvalid = errors.New("brokertoken: the DPoP proof did not verify")
	// ErrKeyMismatch is a sound proof signed by a key that is not the one the token is bound to,
	// which is what makes a stolen token useless. A live token the broker bound to no key at all
	// is refused here too: no proof can be the key it does not have.
	ErrKeyMismatch = errors.New("brokertoken: the DPoP proof's key is not the key the token is bound to")
	// ErrBrokerUnreachable is an introspection that produced no answer: a transport failure, a
	// status the broker does not answer a sound request with, a body that does not decode, or
	// Dispatch's own account token unread. It is not a refusal — a caller told its token was
	// refused would discard a token that may be live — so whoever mounts this check answers 503
	// and the caller comes back.
	ErrBrokerUnreachable = errors.New("brokertoken: the broker could not be reached")
	// ErrRateLimited is a source address with no introspection left in its bucket
	// (DefaultAddressLimit). Nothing was asked of the broker.
	ErrRateLimited = errors.New("brokertoken: too many introspections from this source address")
	// ErrNotASession is a live token that is not a session's credential: the broker's answer
	// names no session, or no person, or the token was issued to a client other than the
	// exchange every Dispatch access token comes from (ExchangeClientID). A machine login's own
	// access token is exactly that — the credential a dev box holds to ask the broker for other
	// credentials, DPoP-bound and perfectly live, for which no exchange ever decided a session,
	// a person or a project scope — and so is any other token the broker issued for something
	// other than acting as a session. Whoever mounts this check acts as the session a token
	// names, so a token that names none is refused here rather than admitted for a request body
	// to name one in its place.
	ErrNotASession = errors.New("brokertoken: the access token is not a session's credential")
)

// AffirmativeCacheTTL is how long one live token's introspection answer is reused, so a caller
// sending many requests asks the broker about its token a few times a minute rather than once a
// request.
//
// Five seconds is sized against the broker's budget. The broker spends one source-address bucket
// for its whole account-authenticated class before it verifies anything — a burst of 200 and 50 a
// second per broker process (internal/broker/api/limits.go, DefaultAccountLimit) — and that one
// bucket is shared with the approval relays a person's click produces, so introspection has to
// stay well under it or a flood of checks would refuse a person their own approval. With
// single-flight in front, one Dispatch pod's sustained introspection rate is its distinct live
// tokens divided by this window: under 128 live tokens that is under 26 a second, half the
// bucket's refill and a fifth of its burst, leaving the relays theirs.
//
// Shortening it costs broker requests in that proportion; lengthening it leaves a revoked token
// accepted for longer, which is the other half of the trade and why the window is seconds.
//
// The window runs from the broker's answer, not from the revocation: an answer is remembered
// when the introspection that produced it completes, and an introspection already in flight
// when the token is revoked may take up to introspectTimeout to complete. So a revoked token
// stops resolving within this window of the last introspection that called it live, which is up
// to introspectTimeout later than the revocation itself.
const AffirmativeCacheTTL = 5 * time.Second

// AffirmativeCacheSize bounds how many answers are remembered at once, so a flood of distinct
// live tokens cannot grow the cache without bound. It is far above the live tokens one pod sees —
// one per running agent session — and an answer that does not fit is simply not remembered, which
// costs that token an introspection and nothing else.
const AffirmativeCacheSize = 4096

// DefaultAddressLimit bounds the introspections one source address can make Dispatch send, in a
// bucket of its own per address (Options.Address): a burst of 20 and five a second, per
// process. A caller holding one token spends one token of this bucket per AffirmativeCacheTTL,
// since every request inside that window is answered from the cache, so an honest caller never
// meets it; a stranger presenting tokens the broker has never issued meets it at once, before
// anything of Dispatch's reaches the broker.
//
// That ordering is the rule INFRA-228 wrote down
// (docs/solutions/best-practices/a-route-that-verifies-before-it-limits-lets-a-stranger-spend-your-upstream-INFRA-228.md):
// a check that costs an upstream call is work an unauthenticated caller can make you do, so the
// bucket is spent before the call and not inside the handler after it. The budget being guarded
// is the broker's account-class bucket, which a person's approval relay shares.
//
// The residual that best practice records stands here too: the bucket is per process, so a fleet
// of N pods allows N times this, and a flood spread over many addresses still reaches the broker.
// What bounds that overall is the broker's own bucket refusing Dispatch 429, which is
// ErrBrokerUnreachable rather than a refusal of anyone's token.
var DefaultAddressLimit = ratelimit.Limit{Every: 200 * time.Millisecond, Burst: 20}

// ProofClockSkew and ProofLifespan are the window a proof's 'iat' must fall in: the library
// accepts a proof from ProofClockSkew before its 'iat' until ProofLifespan plus ProofClockSkew
// after it. They are the broker's own figures — BROKER_PROOF_SKEW_SECONDS' default of a minute
// and the one second internal/broker/oauth sets as the library's lifespan — so a client whose
// proofs the broker accepts is not refused here for its clock.
const (
	ProofClockSkew = time.Minute
	ProofLifespan  = time.Second
)

// proofAlgorithms are the JWS algorithms a proof may be signed with. The broker binds every token
// it issues to a key its own helper minted, and that key is ES256 (internal/broker/proof.NewKey),
// so a wider set would admit no token the broker issues and would widen the signatures a stranger
// can make Dispatch verify.
var proofAlgorithms = []jose.SignatureAlgorithm{jose.ES256}

// proofAlgorithmNames is proofAlgorithms as the library's StrategyConfig spells them. It is built
// once here rather than in the method below because the resource check asks for it on every proof
// it reads — once per DPoP-authenticated request — and the answer never differs: the set is a
// package-level constant in all but name, and the library only reads the names it is given.
var proofAlgorithmNames = func() []string {
	names := make([]string, 0, len(proofAlgorithms))
	for _, algorithm := range proofAlgorithms {
		names = append(names, string(algorithm))
	}
	return names
}()

// Caller is who a verified token names: what a writer is recorded under, and nothing else the
// broker's answer carries. It never carries the token.
type Caller struct {
	// Session is the session the writes are recorded as, which a Caller always names: an answer
	// naming none is ErrNotASession and never reaches one.
	Session string
	// Person is the person that session acts for, which a Caller always names too.
	Person string
	// Project is the one project key this caller may write, and is empty for a caller that is
	// held to no single project. callerOf applies the broker's named-scope rule, so a reader
	// takes this field as it stands rather than reading the scope beside it.
	Project string
}

// Options is what a Verifier needs. BrokerURL, Account, Origin, Replay and Address are required.
type Options struct {
	// BrokerURL is the secrets broker's base URL (DISPATCH_AGENT_SECRETS_URL), the origin
	// IntrospectPath is asked on.
	BrokerURL string
	// Account reads Dispatch's own projected ServiceAccount token, the bearer each introspection
	// presents.
	Account *AccountToken
	// Origin is Dispatch's own public origin (DISPATCH_SERVER_URL): an absolute http(s) URL with
	// no path. A proof's 'htu' is checked against it plus the request's path, never against a
	// forwarded header — see atOrigin.
	Origin string
	// Replay is where a spent proof is recorded, so a captured one cannot be presented twice
	// (store.DPoPProofs for Dispatch).
	Replay rfc9449.DPoPReplayStorage
	// Address is the source address one introspection bucket is keyed on (DefaultAddressLimit),
	// read from the request as the deployment's own proxy-trust rule reads it — Dispatch passes
	// auth.ForwardedAddress. It is required and has no default: a default would either carry
	// one caller's proxy-trust rule into another's listener, or key every request behind a
	// proxy on one bucket, and both stop the bucket bounding what reaches the broker per
	// caller. New refuses a Verifier without one rather than letting a release lose the bound
	// quietly.
	Address func(r *http.Request) string
	// HTTP sends the introspection; nil is a client bounded at introspectTimeout.
	HTTP *http.Client
	// AddressLimit is each source address's introspection bucket; the zero value is
	// DefaultAddressLimit.
	AddressLimit ratelimit.Limit
	// Now is the clock the cache and the address buckets read; nil is time.Now. A proof's own
	// acceptance window is the library's and stays on the real clock whatever this says, and so
	// does an answer's 'exp', which the broker dated — so a test's clock starts from the real one
	// and moves forward from there rather than naming an arbitrary instant.
	Now func() time.Time
}

// Verifier decides callers presenting a broker-issued DPoP token. It is safe for concurrent use
// and is built once per process.
type Verifier struct {
	brokerURL string
	account   *AccountToken
	origin    *url.URL
	// originTLS is the TLS state atOrigin puts on the request it rewrites: one empty state for
	// the whole process when the origin is https, nil when it is not. Nothing reads more of it
	// than whether it is there, and nothing writes to it, so every request shares this one.
	originTLS *tls.ConnectionState
	http      *http.Client
	strategy  *rfc9449.DefaultStrategy
	address   func(r *http.Request) string
	addresses *ratelimit.Keyed
	cache     *answerCache
	flight    singleflight.Group
	now       func() time.Time
}

// New builds a Verifier, refusing options it cannot serve a single request with rather than
// failing one request at a time: a missing dependency and an Origin that is not an absolute
// http(s) origin are both deployment errors.
func New(options Options) (*Verifier, error) {
	if options.BrokerURL == "" {
		return nil, errors.New("brokertoken: the broker URL is required")
	}
	if options.Account == nil {
		return nil, errors.New("brokertoken: the account token reader is required")
	}
	if options.Replay == nil {
		return nil, errors.New("brokertoken: the DPoP replay store is required")
	}
	if options.Address == nil {
		return nil, errors.New("brokertoken: the source address function is required")
	}
	origin, err := url.Parse(options.Origin)
	if err != nil {
		return nil, fmt.Errorf("brokertoken: the origin %q does not parse: %w", options.Origin, err)
	}
	if (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host == "" || origin.Path != "" {
		return nil, fmt.Errorf("brokertoken: the origin %q must be an absolute http(s) URL with no path", options.Origin)
	}
	limit := options.AddressLimit
	if limit == (ratelimit.Limit{}) {
		limit = DefaultAddressLimit
	}
	client := options.HTTP
	if client == nil {
		client = &http.Client{Timeout: introspectTimeout}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	// One empty state serves every request atOrigin rewrites, built here rather than per
	// request: what reads it asks only whether the request carried TLS.
	var originTLS *tls.ConnectionState
	if origin.Scheme == "https" {
		originTLS = &tls.ConnectionState{}
	}
	return &Verifier{
		// The trailing slash goes so BrokerURL + IntrospectPath is one path, as agentsecrets.New
		// trims it for the same reason.
		brokerURL: strings.TrimRight(options.BrokerURL, "/"),
		account:   options.Account,
		origin:    origin,
		originTLS: originTLS,
		http:      client,
		strategy:  rfc9449.NewDefaultStrategy(proofConfig{}, withoutNonces{options.Replay}),
		address:   options.Address,
		addresses: ratelimit.NewKeyed(limit),
		cache:     newAnswerCache(),
		now:       now,
	}, nil
}

// RetryAfter is how long a source address waits for its next introspection, for the Retry-After
// an ErrRateLimited answer carries.
func (v *Verifier) RetryAfter() time.Duration { return v.addresses.Every() }

// Presented reports the access token r carries under the DPoP scheme, and whether it carries one
// at all. A bearer token is not one: RFC 9449 Section 7.2 refuses a DPoP-bound token presented
// under any other scheme, and whoever mounts this check routes a Bearer request to the credential
// that owns that scheme.
//
// The header is read here rather than through rfc9449.AccessTokenFromRequest, which this would
// otherwise be a line of: for a request not under the DPoP scheme that helper falls back to
// RFC 6750's other two places for a token, and a request carrying no Authorization header at all
// — a browser's, under its session cookie — reaches the form one, which costs
// r.ParseMultipartForm. A check asked whether such a request is this credential would consume
// the body of every multipart upload that is not. The scheme comparison below is the helper's
// own, case insensitive as RFC 9110 Section 11.1 requires of an auth scheme.
func Presented(r *http.Request) (string, bool) {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, oauth2.DPoPAccessToken) || token == "" {
		return "", false
	}
	return token, true
}

// Verify decides the caller of r. It asks the broker what the presented token is — answering an
// affirmative answer from its cache while that answer is good — and then holds the caller to the
// key that token is bound to, through the library's own RFC 9449 resource check.
//
// Its error is one of the sentinels above, and the Caller it answers is only ever one the broker
// called live and whose proof verified.
func (v *Verifier) Verify(ctx context.Context, r *http.Request) (Caller, error) {
	token, answer, err := v.live(ctx, r)
	if err != nil {
		return Caller{}, err
	}
	signed := v.atOrigin(r)
	if _, err := v.strategy.ValidateResourceAccess(ctx, signed, token, answer.Thumbprint, false); err != nil {
		return Caller{}, v.refused(err, signed, answer.Thumbprint)
	}
	return callerOf(answer), nil
}

// Live is Verify without the proof check: the broker's answer alone, cached or fresh.
//
// It is for a connection that outlives the request that opened it and whose caller has to keep
// resolving while it is open — a websocket Dispatch re-resolves on every heartbeat
// (api.whileCallerResolves) — where the opening request's proof already authenticated the
// caller. A proof is single use by its 'jti' and 'htu', so presenting that one again is a replay
// and would close a sound connection; what is left to ask of an open connection is whether the
// token is still live, which is this. Nothing admits a caller through it.
func (v *Verifier) Live(ctx context.Context, r *http.Request) (Caller, error) {
	_, answer, err := v.live(ctx, r)
	if err != nil {
		return Caller{}, err
	}
	return callerOf(answer), nil
}

// live is the broker's half of both checks: the token r presents and an answer saying it is
// live, bound to a key, and issued for acting as a session.
func (v *Verifier) live(ctx context.Context, r *http.Request) (string, Introspection, error) {
	token, ok := Presented(r)
	if !ok {
		return "", Introspection{}, ErrNotPresented
	}
	answer, err := v.known(ctx, r, token)
	if err != nil {
		return "", Introspection{}, err
	}
	if !answer.Active {
		return "", Introspection{}, ErrNotActive
	}
	if answer.Thumbprint == "" {
		return "", Introspection{}, ErrKeyMismatch
	}
	if answer.Session == "" || answer.Person == "" || answer.ClientID != ExchangeClientID {
		return "", Introspection{}, ErrNotASession
	}
	return token, answer, nil
}

// known is the broker's answer about token: the one remembered for it while that answer is still
// good, or a fresh introspection.
//
// The cache is read first, because an answer served from it reaches no broker and so spends
// nothing of the budget the bucket guards. A miss joins the single flight for that token, and the
// bucket is spent inside that flight, by whichever request opens it and once the flight's own
// look at the cache has missed too — so one request of the source address's bucket is spent for
// exactly one introspection, which is what the bucket is there to bound. N requests arriving at
// once with one cold token therefore cost one introspection and one token of the bucket, where
// charging each of them would refuse an honest caller that made a burst of calls on a token it
// had just been given. A stranger still pays per distinct token, which is the flood the bucket
// exists for. The introspection runs under the context of whichever request opened the flight,
// so one that goes away takes the others' answer with it; they are answered ErrBrokerUnreachable
// and come back, which is what every other lost answer does.
func (v *Verifier) known(ctx context.Context, r *http.Request, token string) (Introspection, error) {
	key := sha256.Sum256([]byte(token))
	if answer, ok := v.cache.lookup(key, v.now()); ok {
		return answer, nil
	}
	shared, err, _ := v.flight.Do(string(key[:]), func() (any, error) {
		// The flight's own look at the cache: a request that waited for this flight's
		// predecessor to finish would otherwise introspect again for an answer just remembered.
		if answer, ok := v.cache.lookup(key, v.now()); ok {
			return answer, nil
		}
		if !v.addresses.AllowAt(v.address(r), v.now()) {
			return Introspection{}, ErrRateLimited
		}
		answer, err := v.introspect(ctx, token)
		if err != nil {
			return Introspection{}, err
		}
		// Only an affirmative answer is remembered. A refusal held even briefly would keep
		// refusing a token the broker has just issued or reinstated, and it saves nothing worth
		// having: a caller presenting a token the broker does not know is the caller the address
		// bucket above is there to slow.
		if answer.Active {
			v.cache.remember(key, answer, v.now())
		}
		return answer, nil
	})
	if err != nil {
		return Introspection{}, err
	}
	return shared.(Introspection), nil
}

// atOrigin is r as if it had reached Dispatch's own public origin directly: its URL's scheme and
// host and its Host are that origin's, it carries TLS state exactly when that origin is https,
// and it has no forwarding headers.
//
// The library rebuilds a proof's expected 'htu' from X-Forwarded-Proto and r.Host
// (oauth2.RequestURL), both of which a caller behind a proxy could choose, so the proof would
// name a URL the caller picked rather than Dispatch's. The broker answers this the same way for
// its own OAuth routes (internal/broker/api/handlers_oauth.go, atPublicURL): the caller signs
// DISPATCH_SERVER_URL plus the request's path, and Dispatch checks against exactly that.
func (v *Verifier) atOrigin(r *http.Request) *http.Request {
	out := r.Clone(r.Context())
	out.URL.Scheme, out.URL.Host, out.Host = v.origin.Scheme, v.origin.Host, v.origin.Host
	out.TLS = v.originTLS
	out.Header.Del("X-Forwarded-Proto")
	out.Header.Del("X-Forwarded-Host")
	return out
}

// refused is what the library's refusal of a proof means here.
//
// The library reports every RFC 9449 Section 4.3 criterion as the one invalid_dpop_proof error,
// so the key mismatch — the criterion that tells a stolen token from a sound one, and the one
// worth its own outcome — is told apart by reading the proof's own key rather than the refusal's
// prose. ParseProof is the library's own parser and runs only on this path, behind the address
// bucket; a proof that does not even parse is no key mismatch, which is what the error from it
// means here. A failure of the replay store is the library's server_error, which is Dispatch's
// own failure and neither a refusal nor the broker's.
func (v *Verifier) refused(err error, signed *http.Request, boundJKT string) error {
	if errors.Is(err, oauth2.ErrServerError) {
		return fmt.Errorf("brokertoken: check the DPoP proof: %w", err)
	}
	if proof, parseErr := rfc9449.ParseProof(signed.Header.Get("DPoP"), proofAlgorithms); parseErr == nil && proof.Thumbprint != boundJKT {
		return ErrKeyMismatch
	}
	// The hint, never the debug field: a hint names the claim that failed and the request's own
	// method and target URI, and nothing of the token or the key.
	return fmt.Errorf("%w: %s", ErrProofInvalid, oauth2.ErrorToRFC6749Error(err).HintField)
}

// proofConfig is the rfc9449.StrategyConfig the resource check reads its window and algorithms
// from.
type proofConfig struct{}

func (proofConfig) GetDPoPAllowedJWSAlgorithms(context.Context) []string {
	return proofAlgorithmNames
}

func (proofConfig) GetDPoPClockSkew(context.Context) time.Duration     { return ProofClockSkew }
func (proofConfig) GetDPoPProofLifespan(context.Context) time.Duration { return ProofLifespan }

// GetDPoPNonceLifespan is read only where a nonce is minted (DefaultStrategy.NewDPoPNonce), which
// this package never calls, so it names no window.
func (proofConfig) GetDPoPNonceLifespan(context.Context) time.Duration { return 0 }

// withoutNonces is a replay store as the whole rfc9449.Storage the strategy takes. Dispatch
// issues no DPoP nonce: a nonce challenge is a second round trip for a proof already bound to its
// token by 'ath', so Verify asks for none (requireNonce false), and with that
// ValidateResourceAccess reaches neither of these methods —
// handler/rfc9449/strategy.go's ValidateDPoPProof skips the whole nonce branch, and
// CreateDPoPNonce is reached only from NewDPoPNonce.
//
// So each answers an error rather than a value: a release that starts asking for a nonce fails
// loudly instead of minting one nothing stored or accepting one nobody issued.
type withoutNonces struct {
	rfc9449.DPoPReplayStorage
}

// errNoNonces is why: Dispatch mints none.
var errNoNonces = errors.New("brokertoken: Dispatch issues no DPoP nonce")

func (withoutNonces) CreateDPoPNonce(context.Context, string, time.Time) error { return errNoNonces }

func (withoutNonces) IsDPoPNonceValid(context.Context, string) (bool, error) {
	return false, errNoNonces
}
