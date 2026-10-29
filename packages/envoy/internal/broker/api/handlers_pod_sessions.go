package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/broker/exchange"
	"github.com/sjawhar/envoy/internal/oidc"
)

// podSessionBody is POST /v1/oauth/exchange/pod-sessions's exact shape.
type podSessionBody struct {
	// The pod's own projected Kubernetes ServiceAccount token, whose audience is the broker's
	// (BROKER_K8S_OIDC_AUDIENCE): the very token the registrar hands POST /v1/enrollments as it
	// enrolls that pod. It is what names the pod — its account is the token's subject and its uid
	// the kubernetes.io pod uid — so a registrar cannot register a pod whose token it does not
	// hold, and nothing is taken on the registrar's word but the session name.
	PodToken string `json:"pod_token"`
	// The session name that pod may exchange under.
	Session string `json:"session"`
}

// podSessionResponse is POST /v1/oauth/exchange/pod-sessions's answer: the registration as it now
// stands, so a registrar reads back which pod and name it wrote and how long the broker honours
// it without writing it again (oauthstore.PodSessionRetention past this write).
type podSessionResponse struct {
	// The uid of the pod the registration is for, read from the token the body carried.
	PodUID string `json:"pod_uid"`
	// The session name that pod may exchange under.
	Session string `json:"session"`
	// When the broker stops honouring the registration unless it is written again.
	ExpiresAt time.Time `json:"expires_at"`
}

// registerPodSession is POST /v1/oauth/exchange/pod-sessions: a deployment's own daemon, logging
// in as the service BROKER_SERVICE_LOGINS names its account (serviceAuth), records that one pod
// may exchange under one session name. The token exchange binds a session name to a principal —
// the listed service account — and never to a pod, so two pods of one account are one principal
// there and nothing in that claim can tell them apart; this row is what does, and the exchange
// refuses a pod of a free-session account whose uid no registration names for the name it asks
// for. A relaunch is a second pod uid registered for the same name, so a successor is never
// refused what its predecessor held.
//
// The body's token is verified with the verifier an enrolling pod's token is verified with
// (Deps.Enroll.Pod), which the serviceAuth class has already proved the broker has: an expired
// token is 401 POD_TOKEN_EXPIRED and one that does not verify 401 POD_TOKEN_INVALID. The policy
// then refuses a token bound to no pod (400 POD_NOT_BOUND), an account BROKER_EXCHANGE_ACCOUNTS
// does not list (403 POD_ACCOUNT_NOT_LISTED, a code of its own, since the caller's own list
// refusing is ACCOUNT_NOT_LISTED and the two lists are two facts), an account whose entry fixes a
// session (409
// SESSION_FIXED, since such a pod needs no registration and registering one would say something
// the configuration already says), and a session name no request could name back (400
// SESSION_INVALID). Each refusal names the account or the name it refused and repeats no token.
func (s *server) registerPodSession(w http.ResponseWriter, r *http.Request, service string) {
	var body podSessionBody
	if !readJSON(w, r, &body, "INVALID_REQUEST") {
		return
	}
	claims, err := s.deps.Enroll.Pod.VerifyAccount(r.Context(), body.PodToken)
	switch {
	case errors.Is(err, oidc.ErrExpired):
		writeError(w, http.StatusUnauthorized, "POD_TOKEN_EXPIRED", "the pod's ServiceAccount token has expired")
		return
	case err != nil:
		writeError(w, http.StatusUnauthorized, "POD_TOKEN_INVALID", "the pod's ServiceAccount token does not verify")
		return
	}
	expires, err := s.deps.OAuth.RegisterPodSession(r.Context(), claims.Subject, claims.PodUID, body.Session)
	switch {
	case errors.Is(err, exchange.ErrNoPodBinding):
		writeError(w, http.StatusBadRequest, "POD_NOT_BOUND", err.Error())
	case errors.Is(err, exchange.ErrNoEntry):
		writeError(w, http.StatusForbidden, "POD_ACCOUNT_NOT_LISTED", err.Error())
	case errors.Is(err, exchange.ErrSessionFixed):
		writeError(w, http.StatusConflict, "SESSION_FIXED", err.Error())
	case errors.Is(err, exchange.ErrSessionInvalid):
		writeError(w, http.StatusBadRequest, "SESSION_INVALID", err.Error())
	case err != nil:
		writeInternal(w, "register the pod's session name", err)
	default:
		// The one record of who may now write under this name: the registrar is the authority
		// here, so a write it made and a write it stopped making are both read from this line.
		slog.Info("broker: registered a pod to write under a session name",
			"service", service, "account", claims.Subject, "pod", claims.PodUID, "session", body.Session)
		writeJSON(w, http.StatusOK, podSessionResponse{PodUID: claims.PodUID, Session: body.Session, ExpiresAt: expires})
	}
}
