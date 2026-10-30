package api

// broker_token.go is the credential that is not a bearer: an access token the secrets broker
// issued, presented under RFC 9449's DPoP scheme. internal/brokertoken decides it; what is here
// is where that decision is made on a request, how it is carried to every layer that resolves
// the caller, what each refusal says, and what an open connection's heartbeat asks instead.
//
// A DPoP proof is single use by its 'jti' and 'htu' (store.DPoPProofs), which is what the two
// pieces below are for: decideBrokerToken decides a request's presentation once however many
// times its handler resolves the caller, and callerStillResolves asks a connection's broker only
// whether the token is still live, never checking the proof the upgrade already spent.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/ratelimit"
)

// decideBrokerToken decides a request's DPoP presentation before its handler runs and carries
// the decision as the caller every layer behind it resolves (resolvedCallerKey), because a DPoP
// proof is single use: a route that resolves its caller more than once (an issue create is
// requireAuthenticated and then requireActor, and a keyed write resolves before its handler)
// would present one proof twice and refuse the second as the replay it would be. The refusal is
// carried too, so a caller is told why it was refused rather than told its sound proof was
// spent.
//
// It answers nothing itself: each route refuses a caller that does not resolve in its own way —
// a JSON error, a closed socket — and reaches this decision through optionalActor as it always
// has. A request presenting no such token passes straight through and is resolved where it
// always was.
func (s *server) decideBrokerToken(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, presented := brokertoken.Presented(r); !presented {
			handler(w, r)
			return
		}
		actor, human, err := s.brokerTokenActor(r)
		decided := resolvedCaller{actor: actor, human: human, err: err}
		handler(w, r.WithContext(context.WithValue(r.Context(), resolvedCallerKey{}, decided)))
	}
}

// callerStillResolves answers whether the caller a connection was admitted as still resolves,
// which is what whileCallerResolves asks on every heartbeat. For every credential but one that
// is the request's own resolution, unchanged.
//
// The exception is a broker-issued token. The heartbeat's copy of the request still carries the
// proof that opened the connection, and that proof is spent: checking it again would be the
// replay it looks like and would close a sound socket on its first heartbeat. It authenticated
// the upgrade, so what is left to ask is whether the broker still holds the token live, which is
// brokertoken.Verifier.Live. Nothing is admitted through this, only kept.
func (s *server) callerStillResolves(r *http.Request) error {
	if _, presented := brokertoken.Presented(r); presented {
		if s.deps.BrokerTokens == nil {
			return errNoBrokerConfigured()
		}
		_, err := s.deps.BrokerTokens.Live(r.Context(), r)
		return err
	}
	_, _, err := s.optionalActor(r)
	return err
}

// errNoBrokerConfigured is what a DPoP presentation is to a Dispatch with no broker: there is no
// such credential kind, and this is what an Authorization header that is not a bearer has always
// been here (auth.BearerToken reads no token from it).
func errNoBrokerConfigured() *apiError {
	return errorf(http.StatusUnauthorized, "UNAUTHORIZED", "invalid bearer token")
}

// brokerTokenActor authenticates a caller presenting an access token the secrets broker issued,
// under the DPoP scheme: the broker is asked whether the token is live and the caller is held to
// the key that token is bound to (internal/brokertoken). A verified caller is the session the
// token names, acting for the person it names, and is never retried as another credential.
//
// Each refusal is its own code, and none of them repeats the token the caller presented:
//
//   - 401 BROKER_TOKEN_NOT_ACTIVE — the broker answered that the token is not live (unknown,
//     revoked or expired).
//   - 401 BROKER_TOKEN_PROOF_INVALID — the proof did not meet RFC 9449 Section 4.3: a bad
//     signature, a method or target URI that is not this request's, an 'iat' outside the window,
//     a missing or wrong 'ath', or a proof already spent.
//   - 401 BROKER_TOKEN_KEY_MISMATCH — a sound proof signed by a key that is not the token's
//     'jkt', which is what makes a stolen token useless.
//   - 401 BROKER_TOKEN_NOT_A_SESSION — the broker holds the token live, but it is not a
//     session's credential: it names no session or no person, or it was issued to a client
//     other than the exchange a session's token comes from. A machine login's own access token
//     is one of these, and Dispatch records every write under the session its credential
//     proved, so it acts on none of them rather than letting a request body name a session for
//     a credential that names none.
//   - 429 BROKER_TOKEN_RATE_LIMITED — this source address has no introspection left in its
//     bucket; nothing was asked of the broker. It carries the wait in Retry-After as well as in
//     its prose, as the broker's own 429s do.
//   - 503 BROKER_UNAVAILABLE — the broker produced no answer, so the token was never judged. It
//     is a 503 for the reason OIDC_ISSUER_UNAVAILABLE is: a caller told its token was refused
//     would discard a token that may be perfectly live, where a 503 asks it to come back.
func (s *server) brokerTokenActor(r *http.Request) (model.Actor, bool, error) {
	if s.deps.BrokerTokens == nil {
		return model.Actor{}, false, errNoBrokerConfigured()
	}
	caller, err := s.deps.BrokerTokens.Verify(r.Context(), r)
	switch {
	case err == nil:
	case errors.Is(err, brokertoken.ErrNotActive):
		return model.Actor{}, false, errorf(http.StatusUnauthorized, "BROKER_TOKEN_NOT_ACTIVE",
			"the secrets broker does not hold this access token as live")
	case errors.Is(err, brokertoken.ErrKeyMismatch):
		return model.Actor{}, false, errorf(http.StatusUnauthorized, "BROKER_TOKEN_KEY_MISMATCH",
			"the DPoP proof's key is not the key this access token is bound to")
	case errors.Is(err, brokertoken.ErrProofInvalid), errors.Is(err, brokertoken.ErrNotPresented):
		return model.Actor{}, false, errorf(http.StatusUnauthorized, "BROKER_TOKEN_PROOF_INVALID",
			"the DPoP proof did not verify")
	case errors.Is(err, brokertoken.ErrNotASession):
		return model.Actor{}, false, errorf(http.StatusUnauthorized, "BROKER_TOKEN_NOT_A_SESSION",
			"this access token is not a session's credential: the secrets broker names no session for it")
	case errors.Is(err, brokertoken.ErrRateLimited):
		wait := s.deps.BrokerTokens.RetryAfter()
		refusal := errorf(http.StatusTooManyRequests, "BROKER_TOKEN_RATE_LIMITED",
			"too many access tokens checked from this address; try again in %s", wait)
		refusal.retryAfter = ratelimit.RetryAfterSeconds(wait)
		return model.Actor{}, false, refusal
	default:
		// The class only, as serviceTokenActor logs one: this runs before any authorization, so
		// anything the caller chose would be theirs to write into the operator's log.
		slog.Warn("dispatch: the secrets broker could not be reached to check an access token", "error", err)
		return model.Actor{}, false, errorf(http.StatusServiceUnavailable, "BROKER_UNAVAILABLE",
			"the secrets broker cannot be reached; try again in a minute")
	}
	actor := model.Actor{Kind: "session", ID: caller.Session, Owner: &caller.Person}
	// Only a token the broker scoped to one project names one, which brokertoken.callerOf has
	// already read: a dev box session's token is scoped to its person, who may write wherever
	// they may, so it carries no project at all.
	if caller.Project != "" {
		actor.Project = &caller.Project
	}
	accesslog.Note(r.Context(), accesslog.KindBrokerToken, caller.Person)
	return actor, false, nil
}
