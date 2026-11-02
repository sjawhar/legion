package main

// brokerauth.go is the listener's third credential: an access token the secrets broker issued,
// presented under RFC 9449's DPoP scheme (Authorization: DPoP <token> with a DPoP: proof header).
// internal/brokertoken decides it; what is here is the settings a deployment configures it with,
// the spent-proof store it needs, how a decided caller reaches the handlers, and which requests
// are held to the session that caller names.
//
// It is the one credential that says who is calling. The shared token and the OIDC pair say only
// that the caller holds a credential this listener trusts, so a request under either names its
// own session in its body and is believed; a broker token names the session the broker issued it
// to, so a write that acts as a session is held to that one (sessionNotBound).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// The variables a listener takes broker access tokens on, beside ENVOY_API_TOKEN and the
// ENVOY_OIDC_* pair. They are one group: a listener that names any of them must name the broker,
// an origin and exactly one account identity, since a half-configured credential would otherwise
// be a listener that quietly takes none (resolveListenerBrokerConfig).
const (
	// EnvBrokerURL is the secrets broker's base URL, the origin the RFC 7662 introspection route
	// is asked on. Setting it is what turns the credential on; unset, the listener takes no
	// broker token at all.
	EnvBrokerURL = "ENVOY_AGENT_SECRETS_URL"
	// EnvBrokerAccountTokenFile is the file holding this listener's own projected Kubernetes
	// ServiceAccount token, the bearer each introspection presents. A listener in a pod sets it.
	EnvBrokerAccountTokenFile = "ENVOY_AGENT_SECRETS_ACCOUNT_TOKEN_FILE"
	// EnvBrokerGoogleAudience is the audience this listener's own Google identity token is
	// minted for, read from the Compute Engine metadata server at call time
	// (internal/googleidentity). A listener on a VM, which holds no Kubernetes token, sets this
	// instead.
	EnvBrokerGoogleAudience = "ENVOY_AGENT_SECRETS_GOOGLE_AUDIENCE"
	// EnvPublicURL is this listener's own public origin: the absolute http(s) URL, with no path,
	// that a caller reaches /v1 at. A proof's 'htu' is checked against it plus the request's
	// path and never against a forwarded header, so a caller behind a proxy cannot choose the
	// URL its proof names.
	EnvPublicURL = "ENVOY_PUBLIC_URL"
)

// listenerBrokerConfig is what the four variables above say. The zero value is a listener that
// takes no broker token.
type listenerBrokerConfig struct {
	BrokerURL        string
	AccountTokenFile string
	GoogleAudience   string
	Origin           string
}

// configured reports whether this listener takes broker access tokens at all.
func (c listenerBrokerConfig) configured() bool { return c.BrokerURL != "" }

// account is the platform identity each introspection presents: the projected token file, or
// this machine's Google identity token for the audience named. resolveListenerBrokerConfig has
// already refused a configuration naming neither or both.
func (c listenerBrokerConfig) account() brokertoken.Account {
	if c.AccountTokenFile != "" {
		return brokertoken.NewAccountToken(c.AccountTokenFile)
	}
	return brokertoken.NewGoogleAccount(c.GoogleAudience)
}

// resolveListenerBrokerConfig binds the listener's four variable names to one all-or-nothing
// rule, as resolveListenerOIDCConfig binds its pair: none of them set is a listener that takes no
// broker token, and any of them set requires the broker URL, the origin, and exactly one account
// identity. Each refusal names the variable that is missing or the two that cannot both be set.
//
// The account identity is one or the other because the two are different platforms' answers to
// the same question — which service is calling — and a listener that read both would have to
// decide which identity the broker is to resolve it as, a decision the deployment makes by
// setting one.
func resolveListenerBrokerConfig(getenv func(string) string) (listenerBrokerConfig, error) {
	config := listenerBrokerConfig{
		BrokerURL:        strings.TrimSpace(getenv(EnvBrokerURL)),
		AccountTokenFile: strings.TrimSpace(getenv(EnvBrokerAccountTokenFile)),
		GoogleAudience:   strings.TrimSpace(getenv(EnvBrokerGoogleAudience)),
		Origin:           strings.TrimSpace(getenv(EnvPublicURL)),
	}
	if config == (listenerBrokerConfig{}) {
		return listenerBrokerConfig{}, nil
	}
	if config.BrokerURL == "" {
		return listenerBrokerConfig{}, fmt.Errorf("%s is required to take broker access tokens, which %s, %s or %s asks for",
			EnvBrokerURL, EnvBrokerAccountTokenFile, EnvBrokerGoogleAudience, EnvPublicURL)
	}
	if config.AccountTokenFile != "" && config.GoogleAudience != "" {
		return listenerBrokerConfig{}, fmt.Errorf("%s and %s are two identities to call %s as; set one",
			EnvBrokerAccountTokenFile, EnvBrokerGoogleAudience, EnvBrokerURL)
	}
	if config.AccountTokenFile == "" && config.GoogleAudience == "" {
		return listenerBrokerConfig{}, fmt.Errorf("%s or %s is required when %s is set: the broker resolves the calling service from the identity token each introspection presents",
			EnvBrokerAccountTokenFile, EnvBrokerGoogleAudience, EnvBrokerURL)
	}
	if config.Origin == "" {
		return listenerBrokerConfig{}, fmt.Errorf("%s is required when %s is set: a DPoP proof's target URI is checked against this listener's own origin", EnvPublicURL, EnvBrokerURL)
	}
	return config, nil
}

// errBrokerTokensOff is a DPoP presentation to a listener that takes no broker token: there is no
// such credential kind here, so it is the 401 any Authorization header this listener does not
// know has always been.
var errBrokerTokensOff = errors.New("this listener takes no broker access token")

// errBrokerTokensStarting is a DPoP presentation that arrives after /v1 is mounted and before the
// spent-proof bucket is open, which is the window between the listening socket and NATS. It is
// the starting gate's 503 rather than a 401: the credential is configured and the listener cannot
// judge it yet, and a caller told its token was refused would discard a live one.
var errBrokerTokensStarting = errors.New("the listener is still opening the store this credential needs")

// brokerTokens is the listener's broker-token credential as apiAuth reads it: nil on a listener
// that takes none, and otherwise a holder whose verifier is set once the spent-proof bucket is
// open (open). It is a holder rather than the verifier itself because /v1 is mounted before NATS
// is connected (main), while the verifier cannot exist before the bucket it records spent proofs
// in does.
type brokerTokens struct {
	config   listenerBrokerConfig
	verifier atomic.Pointer[brokertoken.Verifier]
}

// newBrokerTokens is the credential config names, or nil when it names none.
func newBrokerTokens(config listenerBrokerConfig) *brokerTokens {
	if !config.configured() {
		return nil
	}
	return &brokerTokens{config: config}
}

// open builds the verifier over the spent-proof bucket on conn's JetStream, and is the listener's
// refusal to serve this credential any other way: a listener configured to take broker tokens on
// a bus with no JetStream fails here, naming EnvBrokerURL, rather than falling back to a store in
// its own memory, which would let one replica accept a proof another replica had already spent.
func (b *brokerTokens) open(conn *nats.Conn, replicas int) error {
	if b == nil {
		return nil
	}
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return fmt.Errorf("%s needs JetStream for the spent-proof bucket %s: %w", EnvBrokerURL, bus.DPoPProofBucket, err)
	}
	kv, err := bus.EnsureKeyValue(js, &nats.KeyValueConfig{
		Bucket:   bus.DPoPProofBucket,
		TTL:      spentProofRetention,
		Storage:  nats.FileStorage,
		Replicas: replicas,
	})
	if err != nil {
		return fmt.Errorf("%s needs the spent-proof bucket %s, which this bus did not open: %w", EnvBrokerURL, bus.DPoPProofBucket, err)
	}
	verifier, err := brokertoken.New(brokertoken.Options{
		BrokerURL: b.config.BrokerURL,
		Account:   b.config.account(),
		Origin:    strings.TrimSuffix(b.config.Origin, "/"),
		Replay:    spentProofs{kv: kv},
		// The listener trusts no forwarding header for this: it has no proxy-trust setting of
		// its own, and a bucket keyed on a header a caller writes bounds nothing. The connection
		// is what is left, which is right for the listener's own deployments, where /v1 is
		// reached directly.
		Address: listenerSourceAddress,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", EnvBrokerURL, err)
	}
	b.verifier.Store(verifier)
	return nil
}

// verify decides the caller of r, and is the whole of what apiAuth asks of this credential: a nil
// holder is a listener that takes none, and a holder with no verifier yet is one still starting.
func (b *brokerTokens) verify(r *http.Request) (brokertoken.Caller, error) {
	if b == nil {
		return brokertoken.Caller{}, errBrokerTokensOff
	}
	verifier := b.verifier.Load()
	if verifier == nil {
		return brokertoken.Caller{}, errBrokerTokensStarting
	}
	return verifier.Verify(r.Context(), r)
}

// retryAfter is the wait a rate-limited caller is given, in seconds.
func (b *brokerTokens) retryAfter() int {
	if verifier := b.verifier.Load(); verifier != nil {
		return ratelimit.RetryAfterSeconds(verifier.RetryAfter())
	}
	return 1
}

// describeBrokerTokens names this credential for the boot line: the broker it asks and the
// identity it asks as, and nothing of either credential's bytes.
func (b *brokerTokens) describe() string {
	if b == nil {
		return ""
	}
	identity := "google audience " + b.config.GoogleAudience
	if b.config.AccountTokenFile != "" {
		identity = "projected token " + b.config.AccountTokenFile
	}
	return "broker tokens at " + b.config.BrokerURL + " as " + identity
}

// listenerSourceAddress is the address one introspection bucket is keyed on: the connection's
// own, with its port dropped so a caller's every connection shares one bucket.
func listenerSourceAddress(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// spentProofRetention is how long one spent proof's record is kept: the whole window the proof it
// records can still be presented in, and a minute more.
//
// A proof is accepted while now is between its 'iat' less brokertoken.ProofClockSkew and its
// 'iat' plus brokertoken.ProofLifespan plus that skew, so a proof recorded now can be presented
// again for at most two skews and a lifespan from this moment, whatever its 'iat'. The extra
// minute covers the skew between the listener that recorded it and the one that reads it, as the
// margin on Dispatch's own record of the same thing does (store.jtiRetentionMargin).
//
// It is the bucket's TTL rather than a per-key one because every key means the same thing and
// lives the same span: nothing reads a record back, so a key whose window has passed says
// nothing, and letting it expire costs no delete.
const spentProofRetention = 2*brokertoken.ProofClockSkew + brokertoken.ProofLifespan + time.Minute

// spentProofs is the listener's rfc9449.DPoPReplayStorage: one key per proof already spent, in
// the KV bucket every listener of a deployment shares (bus.DPoPProofBucket), so a proof captured
// from one listener cannot be presented to another inside its window. The listener has no
// database; this is the store it does have, and the one it must have — a store in one process's
// memory would make a replica's first sight of a proof its first.
type spentProofs struct {
	kv bus.KeyValue
}

// CheckAndSetDPoPProofUsed records a proof as spent and reports whether it already was, which the
// bucket answers by itself: a create of a key that is there is nats.ErrKeyExists, and the key is
// gone once the bucket's TTL has outlived the proof's window. Two listeners creating one key at
// the same moment therefore see one success and one ErrKeyExists, as two Dispatch pods see one
// insert and one conflict.
//
// The key is the proof's 'jti' and normalized target URI hashed together, for the reason
// Dispatch's store keys on the same two: jkt, nonce and htm only ever widen the key, and this
// store serves one resource server whose proofs all carry an 'ath' the library has already
// checked. It is hashed because a KV key is a NATS subject token, which a 'jti' a caller chose
// and a URI are not (bus.KeyValue refuses the dots and spaces either can hold), and the hash of
// the pair is the same length whatever they were.
//
// exp is not read: it dates the end of one proof's window on the clock of the listener that
// checked it, and every key here lives spentProofRetention, which is that window's longest.
func (s spentProofs) CheckAndSetDPoPProofUsed(ctx context.Context, jti, _, _, _, htu string, _ time.Time) (bool, error) {
	key := sha256.Sum256([]byte(jti + "\x00" + htu))
	// Bounded ends the wait with the request, as every other bucket call this listener makes
	// does; the abandoned create may still land, which is the right way round for a record whose
	// whole purpose is to be there.
	_, _, err := bus.Bounded(ctx, func() (uint64, error) {
		return s.kv.Create(hex.EncodeToString(key[:]), nil)
	})
	if errors.Is(err, nats.ErrKeyExists) {
		return true, nil
	}
	if err != nil {
		// Nothing is known about this proof, so it is neither spent nor sound: the library turns
		// this into its server_error, which brokertoken reports as a failure of the check rather
		// than as a refusal of the caller.
		return false, fmt.Errorf("record the spent DPoP proof in %s: %w", bus.DPoPProofBucket, err)
	}
	return false, nil
}

// brokerCallerKey is the context key the decided caller is carried under. It is an unexported
// zero-size type, so nothing outside this package can read or write the value, and no other
// package's key can collide with it.
type brokerCallerKey struct{}

// withBrokerCaller carries the caller a broker token proved to every handler behind apiAuth.
//
// The whole Caller is carried rather than its session alone: the broker's answer also names the
// person or the service it acts for and the project it is scoped to, and a later route that
// reports or records either reads it here instead of asking the broker a second time, which a
// spent proof makes impossible anyway. It is set exactly once, by the one place that verifies a
// presentation (apiAuth), so no handler can be reached with a caller nothing checked, and no
// handler presents the proof a second time.
func withBrokerCaller(ctx context.Context, caller brokertoken.Caller) context.Context {
	return context.WithValue(ctx, brokerCallerKey{}, caller)
}

// brokerCaller is the caller r's credential proved, and whether it proved one at all. A request
// under the shared token, under a service-account token, or under no credential proves none, and
// every route serves it as it always has.
func brokerCaller(r *http.Request) (brokertoken.Caller, bool) {
	caller, ok := r.Context().Value(brokerCallerKey{}).(brokertoken.Caller)
	return caller, ok
}

// sessionNotBound is the refusal a write that acts as session gets from the credential the
// request presented: empty when the request presented no broker token, or when the token names
// this very session, and otherwise the message the route answers 403 with. It logs that refusal
// where it decides it, one line naming both sessions and the path and never the token, as
// apiAuth's own refusals do.
//
// It is called by each of the five routes that act as a session, after that route has read the
// session its body names, rather than being a wrapper around them: the routes name the session
// in four different shapes (two message bodies' source_session, subscribe's and unsubscribe's
// session_id, and the role claim's), and a wrapper would have to decode each body a second time
// to find it. The read routes that name another session by design — a registry entry, a
// session's interests, the session list — call nothing here: narrowing those would stop every
// session looking another up.
//
// An empty session is no refusal: the body names no session, so the request acts as none, and
// nothing is recorded under another session's name. The two message routes alone allow that, and
// what they produce is an envelope with no source session.
func sessionNotBound(r *http.Request, logger *logging.Logger, session string) string {
	caller, ok := brokerCaller(r)
	if !ok || session == "" || caller.Session == session {
		return ""
	}
	logger.Warn("listener: broker token acts as another session",
		slog.String("token_session", caller.Session),
		slog.String("request_session", session),
		slog.String("path", r.URL.Path))
	return fmt.Sprintf("this access token acts as session %s, so it may not act as %s", caller.Session, session)
}
