package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/agentsecrets"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A session pod is one an operator launches by hand in the deployment's namespace, outside every
// claim (internal/runtime's ErrNoSuchPod). The daemon never spawns, stops or observes it, and the
// two routes here hand it the only two things a pod cannot get for itself, both of which the
// daemon alone holds: its key's enrollment with the secrets broker, which is made under the
// daemon's own launcher credential (internal/agentsecrets), and a gh bundle, which is minted from
// the GitHub App keys the daemon holds and nothing else does.
//
// Both routes take the operator bearer, which is the same authority `POST
// /legion/v1/controller/secret` requires before it mints anything: a session pod belongs to no
// claim and holds no capability of its own, so there is nothing weaker to authenticate it by, and
// minting a capability for it first would only be a second secret for the one caller — the
// operator's own `legion session` — that already holds the bearer that would buy it. The refusal
// is the coded shape (Failure), not the controller secret route's sentence alone, so a caller
// branches on the code as `legion controller start` does on its refresh route's
// (controllerGitHubCredential).
//
// Neither route widens anything. The enrollment is the enrollment a Legion worker pod's shim
// already gets, proved by the pod's own projected token, and the bundle is the bundle an
// implementer pod already gets for the owner its issue names. Neither trusts a pod uid from the
// request: the daemon resolves the name through its own Kubernetes client (SessionPods), refuses a
// name no pod in its namespace holds, and refuses one of Legion's own pods, so neither route can
// be aimed at a claim's pod.

// SessionEnroller enrolls a session pod's key with the secrets broker under the daemon's launcher
// credential: the daemon's own *agentsecrets.Client. Nil is a deployment that enrolls no pod
// (`runtime.kubernetes.agent_secrets` unset), whose enrollment route refuses every call.
type SessionEnroller interface {
	Enroll(ctx context.Context, e agentsecrets.PodEnrollment) (agentsecrets.Enrollment, error)
}

// SessionPods is the deployment's Kubernetes namespace as the session routes read and write it:
// the Sandbox runtime. Nil is a runtime that runs no pods (tmux), whose session routes refuse
// every call.
type SessionPods interface {
	// SessionPod is the uid of the session pod named name, and the refusals the routes answer by
	// name: runtime.ErrNoSuchPod when the namespace holds no pod of that name, and
	// runtime.ErrPodNotSession when the one it holds is Legion's own.
	SessionPod(ctx context.Context, name string) (uid string, err error)
	// WriteSessionGitHubCredential writes bundle into the session pod's own gh Secret, owned by
	// the pod so Kubernetes deletes it with the pod, and registers the Secret with the refresher
	// that rewrites the role Secrets on a minute tick. It answers the Secret's name.
	WriteSessionGitHubCredential(ctx context.Context, name, uid string, bundle ghconfig.Bundle) (secret string, err error)
	// RecordSessionEnrollment records the broker enrollment made for a session pod, so the
	// runtime's minute sweep revokes it once the pod is gone — the broker proves a renewal with
	// the pod's projected token alone and never asks whether the pod still exists, so an
	// enrollment nobody revokes outlives its pod. A claim's machine revokes its process's the
	// same way when it lets the process go. It only writes memory, so it cannot fail.
	RecordSessionEnrollment(name, uid, enrollment string)
}

// SessionEnrollRequest is `POST /legion/v1/sessions/agent-secrets`'s body: the session pod by
// name, the uid the caller read for it, if it read one, which the daemon compares against the pod
// it finds; the thumbprint of the key `agent-secrets keygen` wrote inside the pod; the pod's
// projected ServiceAccount token for the broker's audience, which proves the pod to the broker;
// and the Oh My Pi session id to record the enrollment under, if the caller has one.
//
// The keygen inside the pod and the `agent-secrets renew` loop that keeps the lease alive belong
// to whatever launches the pod, as they belong to `legion worker-shim` in a claim's pod: the
// daemon runs no `kubectl` and reaches into no pod.
type SessionEnrollRequest struct {
	Pod        string `json:"pod"`
	PodUID     string `json:"podUid,omitempty"`
	Thumbprint string `json:"thumbprint"`
	PodToken   string `json:"podToken"`
	Session    string `json:"session,omitempty"`
}

// SessionEnrollResponse is that route's answer: the enrollment id the pod writes beside its key
// and renews under, the pod uid the daemon enrolled, and when the lease expires if the renewer
// does not keep it alive.
type SessionEnrollResponse struct {
	EnrollmentID   string `json:"enrollmentId"`
	PodUID         string `json:"podUid"`
	LeaseExpiresAt string `json:"leaseExpiresAt"`
}

// SessionCredentialRequest is `POST /legion/v1/sessions/github-credential`'s body: the session pod
// by name, the uid the caller read for it if it read one, and Owner, the owner the session works
// on — its bundle's writer and its default, so its GH_CONFIG_DIR names a directory holding a
// token that can push. An owner outside the project is refused.
type SessionCredentialRequest struct {
	Pod    string `json:"pod"`
	PodUID string `json:"podUid,omitempty"`
	Owner  string `json:"owner"`
}

// SessionCredentialResponse is that route's answer. It names what was written and nothing of it:
// the Secret the pod projects its gh directories from, the pod uid that Secret is owned by, the
// default owner, the App the write token acts as, every owner the bundle holds a directory for,
// and the soonest of the tokens' expiries. No token text is served — the bundle goes into the
// Secret, and the caller never holds it.
type SessionCredentialResponse struct {
	SecretName string   `json:"secretName"`
	PodUID     string   `json:"podUid"`
	Default    string   `json:"default"`
	App        string   `json:"app"`
	Owners     []string `json:"owners"`
	ExpiresAt  string   `json:"expiresAt"`
}

// sessionAgentSecrets is `legion session enroll`'s one daemon call: the operator's bearer buys the
// enrollment of a session pod's key with the secrets broker, made under the daemon's own launcher
// credential and proved with the pod's own projected token, exactly as the daemon enrolls a claim
// pod's shim. The pod is resolved before the broker is called, so a name no pod holds costs no
// enrollment, and a permanent broker refusal is told apart from the broker being out of reach so
// the caller knows whether another attempt can change the answer.
func (s *server) sessionAgentSecrets(w http.ResponseWriter, r *http.Request) {
	if !s.refuseUnlessOperator(w, r, "agent-secrets enrollment") {
		return
	}
	var req SessionEnrollRequest
	if !s.readBody(w, r, &req, nil) {
		return
	}
	if !requireFailureFields(w, field{"pod", req.Pod}, field{"thumbprint", req.Thumbprint}, field{"podToken", req.PodToken}) {
		return
	}
	if s.sessionEnroller == nil {
		writeFailure(w, http.StatusConflict, "AGENT_SECRETS_UNCONFIGURED",
			"this deployment enrolls no pod with the secrets broker (runtime.kubernetes.agent_secrets)")
		return
	}
	uid, ok := s.sessionPodUID(w, r, "agent-secrets enrollment", req.Pod, req.PodUID)
	if !ok {
		return
	}
	enrollment, err := s.sessionEnroller.Enroll(r.Context(), agentsecrets.PodEnrollment{
		PodUID: uid, Thumbprint: req.Thumbprint, PodToken: req.PodToken, Session: req.Session,
	})
	if err != nil {
		// The broker's own sentence names no key, token or credential (internal/agentsecrets), so
		// it is safe to serve, and it is the only thing that says which of its rules refused.
		if agentsecrets.IsPermanent(err) {
			s.log.Warn("api: refused a session agent-secrets enrollment: the broker refused it", "pod", req.Pod, "error", err)
			writeFailure(w, http.StatusForbidden, "ENROLLMENT_REFUSED", err.Error())
			return
		}
		s.log.Warn("api: a session agent-secrets enrollment could not be made", "pod", req.Pod, "error", err)
		writeFailure(w, http.StatusServiceUnavailable, "ENROLLMENT_UNAVAILABLE", err.Error())
		return
	}
	s.sessionPods.RecordSessionEnrollment(req.Pod, uid, enrollment.ID)
	s.log.Info("api: session agent-secrets enrollment served", "pod", req.Pod, "leaseExpiresAt", enrollment.LeaseExpires)
	writeJSON(w, http.StatusOK, SessionEnrollResponse{
		EnrollmentID: enrollment.ID, PodUID: uid,
		LeaseExpiresAt: enrollment.LeaseExpires.UTC().Format(timeFormat),
	})
}

// sessionGitHubCredential is `legion session github-credential`'s call: the operator's bearer buys
// a session pod the gh bundle an implementer pod of the same owner already gets — the implement
// App's write lease for the owner the session works on, that App's read-only lease for every other
// owner of the project (appauth.GitHubCredential) — written into a Secret of the pod's own, which
// the pod projects as its gh directories.
//
// The owner is checked against the project's own owners before anything is minted: a bundle is
// built for the role's owner whatever it is (GitHubCredential adds it), so without this check a
// caller could name an owner the project does not serve and be handed a lease for it. A controller
// is refused the same way inside that function, with the same sentinel, and this refusal reads the
// project's owner set from the one place that computes it (appauth.GitHubOwners), so the set a
// session is checked against and the set a pod's bundle is built from are one computation.
func (s *server) sessionGitHubCredential(w http.ResponseWriter, r *http.Request) {
	if !s.refuseUnlessOperator(w, r, "GitHub credential") {
		return
	}
	var req SessionCredentialRequest
	if !s.readBody(w, r, &req, nil) {
		return
	}
	if !requireFailureFields(w, field{"pod", req.Pod}, field{"owner", req.Owner}) {
		return
	}
	owner := strings.ToLower(req.Owner)
	if !ghconfig.IsOwner(owner) {
		writeFailure(w, http.StatusBadRequest, "INVALID_OWNER", "owner is not a GitHub login")
		return
	}
	if s.tokens == nil {
		writeFailure(w, http.StatusInternalServerError, "GITHUB_TOKEN_SOURCE_UNAVAILABLE", "GitHub token source is unavailable")
		return
	}
	if s.githubOwners == nil {
		writeFailure(w, http.StatusInternalServerError, "GITHUB_OWNER_UNCONFIGURED", "no repository owner is configured for GitHub App tokens")
		return
	}
	if !slices.Contains(appauth.GitHubOwners(s.githubOwners, s.renames), owner) {
		err := fmt.Errorf("the session's owner %q %w", owner, appauth.ErrOwnerOutsideProject)
		s.log.Warn("api: refused a session GitHub credential: OWNER_OUTSIDE_PROJECT", "owner", owner)
		writeFailure(w, http.StatusBadRequest, "OWNER_OUTSIDE_PROJECT", err.Error())
		return
	}
	uid, ok := s.sessionPodUID(w, r, "GitHub credential", req.Pod, req.PodUID)
	if !ok {
		return
	}
	// A session writes to the owner it names, so it is the implement App's bundle: the one
	// AppRoleFor gives every role that can write a branch, and the one a claim's implementer pod
	// holds today for its issue's owner.
	bundle, err := appauth.GitHubCredential(s.tokens, s.githubOwners, s.renames)(r.Context(), appauth.SessionRole, owner)
	switch {
	case errors.Is(err, runtime.ErrNoGitHubOwner):
		writeFailure(w, http.StatusInternalServerError, "GITHUB_OWNER_UNCONFIGURED", "no repository owner is configured for GitHub App tokens")
		return
	case errors.Is(err, appauth.ErrOwnerOutsideProject):
		s.log.Warn("api: refused a session GitHub credential: OWNER_OUTSIDE_PROJECT", "owner", owner)
		writeFailure(w, http.StatusBadRequest, "OWNER_OUTSIDE_PROJECT", err.Error())
		return
	case err != nil:
		s.log.Error("api: mint a session's GitHub App tokens", "error", err)
		writeFailure(w, http.StatusBadGateway, "GITHUB_TOKEN_FAILED", "GitHub token exchange failed")
		return
	}
	secret, err := s.sessionPods.WriteSessionGitHubCredential(r.Context(), req.Pod, uid, bundle)
	if err != nil {
		s.log.Error("api: write a session's gh Secret", "pod", req.Pod, "error", err)
		writeFailure(w, http.StatusBadGateway, "SESSION_SECRET_WRITE_FAILED", "the session's gh Secret could not be written")
		return
	}
	var soonest time.Time
	for _, rendered := range bundle.Owners {
		if soonest.IsZero() || rendered.ExpiresAt.Before(soonest) {
			soonest = rendered.ExpiresAt
		}
	}
	answer := SessionCredentialResponse{
		SecretName: secret, PodUID: uid, Default: bundle.Default, App: bundle.Owners[bundle.Default].App,
		Owners: bundle.OwnerNames(), ExpiresAt: soonest.UTC().Format(timeFormat),
	}
	s.log.Info("api: session github credential served", "pod", req.Pod, "secret", secret, "app", answer.App,
		"expiresAt", soonest, "owners", answer.Owners)
	writeJSON(w, http.StatusOK, answer)
}

// refuseUnlessOperator is both session routes' one authentication: the operator bearer, refused
// with the coded shape so a caller tells it from every other refusal without reading the sentence.
func (s *server) refuseUnlessOperator(w http.ResponseWriter, r *http.Request, what string) bool {
	if s.operatorAuthorized(r) {
		return true
	}
	s.log.Warn("api: refused a session " + what + ": no operator bearer, or the wrong one")
	writeFailure(w, http.StatusForbidden, "INVALID_OPERATOR_TOKEN", invalidOperatorToken)
	return false
}

// sessionPodUID resolves name to the uid of the session pod it names, through the daemon's own
// Kubernetes client and never from the request: the caller's own read of the pod is a claim, and
// a pod of that name replaced since it read one is exactly what expected catches
// (POD_UID_MISMATCH). A runtime that runs no pods serves neither route.
func (s *server) sessionPodUID(w http.ResponseWriter, r *http.Request, what, name, expected string) (string, bool) {
	if s.sessionPods == nil {
		writeFailure(w, http.StatusConflict, "SESSION_PODS_UNAVAILABLE", "this deployment runs no pods, so it has no session pod to serve")
		return "", false
	}
	uid, err := s.sessionPods.SessionPod(r.Context(), name)
	switch {
	case errors.Is(err, runtime.ErrNoSuchPod):
		s.log.Warn("api: refused a session "+what+": POD_NOT_FOUND", "pod", name)
		writeFailure(w, http.StatusNotFound, "POD_NOT_FOUND", err.Error())
		return "", false
	case errors.Is(err, runtime.ErrPodNotSession):
		s.log.Warn("api: refused a session "+what+": POD_NOT_SESSION", "pod", name)
		writeFailure(w, http.StatusConflict, "POD_NOT_SESSION", err.Error())
		return "", false
	case err != nil:
		s.log.Error("api: read a session pod", "pod", name, "error", err)
		writeFailure(w, http.StatusBadGateway, "POD_READ_FAILED", "the session pod could not be read")
		return "", false
	}
	if expected != "" && expected != uid {
		s.log.Warn("api: refused a session "+what+": POD_UID_MISMATCH", "pod", name)
		writeFailure(w, http.StatusConflict, "POD_UID_MISMATCH",
			"the pod named is not the one the request names by uid: a pod of that name was replaced")
		return "", false
	}
	return uid, true
}
