package main

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/oidc"
)

const bearerPrefix = "Bearer "

// apiAuth requires JSON bodies for /v1, then authenticates it with the shared ENVOY_API_TOKEN
// bearer, with an access token the secrets broker issued when one is presented under the DPoP
// scheme (brokers, brokerauth.go), and, when a verifier is configured, with a projected
// service-account token issued to its issuer and audience. Exempt paths pass in every
// configuration; /v1 is open only when neither bearer kind is configured, so an empty shared
// token beside a configured verifier requires a token rather than opening the tree. An open /v1
// serves only a request whose Host names this listener (servesHost: a loopback name or address,
// or one of allowedHosts), so a page that rebinds its own name to an address reaching the
// listener cannot use it; a listener that requires a bearer checks no Host, since such a page
// holds no bearer. A bearer the verifier rejects is answered 401 and never falls back to another
// authentication path; the failure class goes to the log and never to the caller, since naming
// it would tell an unauthenticated client which credential the listener is configured for.
//
// The broker-token branch is taken only on a listener that takes this credential (brokers is
// nil on one that does not), and there the scheme alone decides it, before both bearer ones:
// RFC 9449 Section 7.2 refuses a DPoP-bound token presented under any other scheme, so a DPoP
// presentation is this credential whatever the listener's other settings are, and is never
// retried as a bearer. It comes before the open pass-through too, which is what binds a session
// on an open listener: a caller that presents a broker token is held to the session it names
// even where /v1 would have served the request with no credential at all.
//
// A listener with none of the broker variables set reads that header as it read every
// Authorization header it did not know before this credential existed: the open listener serves
// the request, and a listener that requires a bearer answers it the same 401 it always did.
// That is what keeps the loopback, no-bearer listener the GCP VMs and the clustered hub run
// serving the clients that hold a broker route of their own — a client sends Authorization:
// DPoP whenever it has a broker and an identity to trade and no Envoy bearer is set, whatever
// the listener it is calling takes.
//
// Configuring it opens nothing and closes nothing. A request presenting no token is judged
// exactly as it was before this credential existed — the open listener still serves it, the Host
// and Content-Type fences still apply, and validateListenerAPIAuth still refuses a non-loopback
// listener with no bearer and no ENVOY_API_ALLOW_UNAUTHENTICATED — because a broker token is a
// credential a caller may hold rather than one this listener can require: every client of /v1
// that holds none would be locked out by a setting whose purpose is to tell callers apart.
func apiAuth(token string, verifier *oidc.Verifier, brokers *brokerTokens, allowedHosts []string, logger *logging.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAPIAuthExemptPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		open := token == "" && verifier == nil
		if open && !servesHost(r.Host, allowedHosts) {
			logger.Warn("listener: request for a host it does not serve",
				slog.String("host", r.Host),
				slog.String("path", r.URL.Path))
			writeJSONError(w, http.StatusMisdirectedRequest,
				fmt.Sprintf("request Host %q is not a host this listener serves; an unauthenticated listener serves loopback and the names in ENVOY_ALLOWED_HOSTS", r.Host))
			return
		}
		if r.ContentLength != 0 {
			contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || contentType != "application/json" {
				writeJSONError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
				return
			}
		}
		if _, presented := brokertoken.Presented(r); brokers != nil && presented {
			caller, err := brokers.verify(r)
			if err != nil {
				refuseBrokerToken(w, logger, r, brokers, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(withBrokerCaller(r.Context(), caller)))
			return
		}
		if open {
			next.ServeHTTP(w, r)
			return
		}

		authorization := r.Header.Get("Authorization")
		if token != "" && subtle.ConstantTimeCompare(
			[]byte(authorization),
			[]byte(bearerPrefix+token),
		) == 1 {
			next.ServeHTTP(w, r)
			return
		}

		if verifier != nil {
			if raw, ok := strings.CutPrefix(authorization, bearerPrefix); ok && oidc.LooksLikeJWT(raw) {
				_, err := verifier.Verify(r.Context(), raw)
				if err == nil {
					next.ServeHTTP(w, r)
					return
				}
				logger.Warn("listener: service-account token rejected",
					slog.String("reason", oidc.Reason(err)),
					slog.String("path", r.URL.Path))
			}
		}

		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
	})
}

// refuseBrokerToken answers a DPoP presentation this listener did not admit, and logs the one
// line that says why: the class, as the service-account branch logs oidc.Reason's, the path, and
// never the token or the proof.
//
// Three of the outcomes are not refusals of the caller and are not answered as one. A broker
// that gave no answer is a 503, for the reason Dispatch answers one: a caller told its token was
// refused would discard a token that may be perfectly live, where a 503 asks it to come back. A
// spent-proof store that did not answer is the same 503: the listener could not run the check,
// which is its own failure rather than the token's, and a client does not retry a 401, so a NATS
// fault of a few seconds would otherwise fail every send a live token makes. A source address
// with no introspection left in its bucket is the 429 that bucket exists to write, carrying the
// wait it must come back after; nothing of it reached the broker. Every other outcome is the
// same 401 and the same body every other refused credential gets here, so the answer tells an
// unauthenticated caller nothing about which credentials this listener takes.
func refuseBrokerToken(w http.ResponseWriter, logger *logging.Logger, r *http.Request, brokers *brokerTokens, err error) {
	reason := brokerTokenReason(err)
	logger.Warn("listener: broker token rejected",
		slog.String("reason", reason),
		slog.String("path", r.URL.Path))
	switch {
	case errors.Is(err, errBrokerTokensStarting):
		w.Header().Set("Retry-After", "1")
		writeJSONError(w, http.StatusServiceUnavailable, "service starting")
	case errors.Is(err, brokertoken.ErrBrokerUnreachable):
		w.Header().Set("Retry-After", "1")
		writeJSONError(w, http.StatusServiceUnavailable, "the secrets broker cannot be reached; try again in a minute")
	case errors.Is(err, brokertoken.ErrRateLimited):
		w.Header().Set("Retry-After", strconv.Itoa(brokers.retryAfter()))
		writeJSONError(w, http.StatusTooManyRequests, "too many access tokens checked from this address")
	case reason == brokerTokenCheckFailed:
		w.Header().Set("Retry-After", "1")
		writeJSONError(w, http.StatusServiceUnavailable, "this access token could not be checked; try again in a minute")
	default:
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
	}
}

// brokerTokenCheckFailed is the class of a presentation this listener could not judge at all:
// its spent-proof store did not answer. It is this listener's own failure, so refuseBrokerToken
// answers it the 503 every other unjudged presentation gets.
const brokerTokenCheckFailed = "check_failed"

// brokerTokenReason is the class one refused presentation goes into the log under, the
// vocabulary an operator filters on. It names what was wrong and never what was presented.
func brokerTokenReason(err error) string {
	switch {
	case errors.Is(err, errBrokerTokensStarting):
		return "starting"
	case errors.Is(err, brokertoken.ErrNotActive):
		return "not_active"
	case errors.Is(err, brokertoken.ErrKeyMismatch):
		return "key_mismatch"
	case errors.Is(err, brokertoken.ErrProofInvalid), errors.Is(err, brokertoken.ErrNotPresented):
		return "proof_invalid"
	case errors.Is(err, brokertoken.ErrNotASession):
		return "not_a_session"
	case errors.Is(err, brokertoken.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, brokertoken.ErrBrokerUnreachable):
		return "unavailable"
	}
	// The residue is this listener's own failure to run the check — its spent-proof store did
	// not answer — which brokertoken reports as neither the broker's nor the caller's.
	return brokerTokenCheckFailed
}

// servesHost reports whether a request's Host names this listener: its name, port stripped,
// brackets off an IPv6 address, case folded and one trailing dot dropped, is a loopback name or
// address or one of allowedHosts, which config.Load normalizes the same way. An empty Host (an
// HTTP/1.0 request that sent none) names nothing.
func servesHost(host string, allowedHosts []string) bool {
	name := config.HostName(host)
	if name == "" {
		return false
	}
	return isLoopbackHost(name) || slices.Contains(allowedHosts, name)
}

func isAPIAuthExemptPath(path string) bool {
	return path == "/healthz" || path == "/metrics" || strings.HasPrefix(path, "/webhook/")
}

// resolveListenerOIDCConfig binds the listener's two variable names to the
// shared both-or-neither rule. They are configured both or neither: one alone
// is a refusal naming the missing one.
func resolveListenerOIDCConfig(getenv func(string) string) (issuer, audience string, err error) {
	return oidc.ConfigFromEnv(getenv, "ENVOY_OIDC_ISSUER", "ENVOY_OIDC_AUDIENCE")
}

// describeListenerAPIAuth names the credentials /v1 accepts, for the boot log. Broker tokens are
// named beside the two bearers, and an operator reading the line beside the `listener API hosts:`
// one below gets the whole picture: a listener that accepts only broker tokens still serves /v1
// with no credential, since that credential says who is calling rather than whether they may.
func describeListenerAPIAuth(token, issuer string, brokers *brokerTokens) string {
	named := make([]string, 0, 3)
	if token != "" {
		named = append(named, "shared token")
	}
	if issuer != "" {
		named = append(named, "oidc "+issuer)
	}
	if described := brokers.describe(); described != "" {
		named = append(named, described)
	}
	if len(named) == 0 {
		return "disabled"
	}
	return strings.Join(named, " and ")
}

// describeListenerAPIHosts names the Hosts /v1 is served to, for the boot log: any Host when a
// bearer is required, loopback and ENVOY_ALLOWED_HOSTS otherwise. A list set beside a bearer is
// said to be unused rather than left to look as if it applied.
func describeListenerAPIHosts(token string, verifier *oidc.Verifier, allowedHosts []string) string {
	if token != "" || verifier != nil {
		if len(allowedHosts) > 0 {
			return "any (a bearer is required, so ENVOY_ALLOWED_HOSTS is unused)"
		}
		return "any (a bearer is required)"
	}
	if len(allowedHosts) == 0 {
		return "loopback"
	}
	return "loopback, " + strings.Join(allowedHosts, ", ")
}

// validateListenerAPIAuth refuses a listener that would serve /v1 with no credential on an address
// other than loopback. It judges cfg.ListenHost, which with ENVOY_LISTEN_FD is the inherited
// socket's own address, so a socket on every interface is refused as ENVOY_LISTEN_HOST=0.0.0.0 is.
func validateListenerAPIAuth(cfg config.Service, token, allowUnauthenticated string, verifier *oidc.Verifier) error {
	if token != "" || verifier != nil || allowUnauthenticated == "1" || isLoopbackHost(cfg.ListenHost) {
		return nil
	}
	listen := fmt.Sprintf("ENVOY_LISTEN_HOST=%q is not loopback", cfg.ListenHost)
	if cfg.ListenSocket != nil {
		listen = cfg.ListenSocket.String() + ", is not on loopback"
	}
	return fmt.Errorf("ENVOY_API_TOKEN or the ENVOY_OIDC_ISSUER/ENVOY_OIDC_AUDIENCE pair is required when %s; set ENVOY_API_ALLOW_UNAUTHENTICATED=1 only during the Fargate transition", listen)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
