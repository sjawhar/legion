package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// legacyKinds are the kinds an agent-secrets-helper before legion-envoy 10 sends, each recorded as
// "session": "host" for a session the helper enrolls for itself (Broker.Enroll), and "box" for a key
// a launcher enrolled through its enroll-box op, which agentbox calls. Such a helper keeps running
// until no session is registered with it (dotfiles' installers/agent-secrets.sh restarts it only
// then), so the broker must enroll its sessions through any client rollout. Each such enrollment
// logs "an enrollment named a legacy kind"; this set goes once the production broker has logged
// none for 30 days.
var legacyKinds = map[string]struct{}{"host": {}, "box": {}}

// createEnrollmentBody is POST /v1/enrollments's body (the shared broker contract's
// enrollment route). It names no approver: the policy picks a request's approver at request time,
// never at enrollment. slot is optional and pod-only: omitted or "" is the runtime's one
// enrollment, and a slot names one of several independent enrollments of the same pod
// (enroll.Enrollment.Slot), chosen by the launcher whose proof authenticates the call.
type createEnrollmentBody struct {
	// What the session is: "session" (an agent session on a machine its person logged in) or
	// "pod". The broker also takes "host" and "box" and records either as "session" (legacyKinds).
	Kind string `json:"kind"`
	// The session's runtime: a pod's UID, or whatever names the session on its machine (the
	// helper's host:pid:start time, a container's id). A launcher enrolls one live session per
	// runtime id (and slot).
	RuntimeID string `json:"runtime_id"`
	// A pod only, optional: which of the pod's independent enrollments this is.
	Slot string `json:"slot"`
	// Optional: the operator the session runs for. The broker records the machine credential's own
	// operator whatever is sent, and refuses any other login. Absent or null for a pod.
	Operator *string `json:"operator"`
	// The RFC 7638 JWK thumbprint of the session's P-256 signing key.
	Thumbprint string `json:"thumbprint"`
	// Optional: the Envoy session the broker notifies when one of this session's pending requests
	// expires.
	SessionID *string `json:"session_id"`
	// A pod only: its projected service-account token, which proves the runtime id is its UID.
	PodToken *string `json:"pod_token"`
}

// enrollmentResponse is POST /v1/enrollments's answer: 201 for a new enrollment, 200 when the same
// key is already enrolled in that runtime and slot.
type enrollmentResponse struct {
	// The enrollment's id, which the session's Proof headers name.
	EnrollmentID string `json:"enrollment_id"`
	// When the enrollment's lease lapses unless the session renews it.
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	// The pod slot it holds; null for every enrollment without one.
	Slot *string `json:"slot"`
}

// leaseResponse is POST /v1/enrollments/{id}/renew's answer.
type leaseResponse struct {
	// When the renewed lease lapses.
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// renewEnrollmentBody is POST /v1/enrollments/{id}/renew's optional body: no body at all is a
// renewal carrying no token.
type renewEnrollmentBody struct {
	// A pod only: its current projected service-account token, which must name the pod UID and
	// service account the enrollment was made for. Required once the enrollment has renewed with
	// it (enroll.Service.Renew).
	PodToken *string `json:"pod_token"`
}

// selfResponse is GET /v1/enrollments/self's answer: the calling session as the broker knows it.
type selfResponse struct {
	// The session's enrollment id.
	EnrollmentID string `json:"enrollment_id"`
	// The session's live grants, soonest to expire first.
	Grants []requests.Grant `json:"grants"`
	// "session" or "pod".
	Kind string `json:"kind"`
	// When the session's lease lapses unless it is renewed.
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	// The person the session runs for; null for a pod.
	Operator *string `json:"operator"`
	// The pod slot it holds; null for every enrollment without one.
	Slot *string `json:"slot"`
}

// createEnrollment authenticates by launcher proof now (payload carries "lid"), never a bearer
// token; cred is the launcher credential's own identity, read by server.authenticate from the
// verified proof's launcher id.
func (s *server) createEnrollment(w http.ResponseWriter, r *http.Request, cred enroll.Credential) {
	var body createEnrollmentBody
	if !readJSON(w, r, &body, "INVALID_ENROLLMENT") {
		return
	}
	_, legacy := legacyKinds[body.Kind]
	kind := body.Kind
	switch {
	case legacy:
		kind = "session"
		slog.Info("broker: an enrollment named a legacy kind; recorded as session", "kind", body.Kind)
	case body.Kind == "session" || body.Kind == "pod":
		kind = body.Kind
	default:
		writeError(w, http.StatusBadRequest, "INVALID_KIND", `kind must be "session" or "pod"`)
		return
	}

	result, err := s.deps.Enroll.Create(r.Context(), cred, enroll.Enrollment{
		Kind:       kind,
		RuntimeID:  body.RuntimeID,
		Slot:       body.Slot,
		Operator:   body.Operator,
		Thumbprint: body.Thumbprint,
		SessionID:  body.SessionID,
		PodToken:   derefOr(body.PodToken, ""),
	})
	switch {
	case errors.Is(err, enroll.ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, "LAUNCHER_INVALID", "the launcher credential is not valid")
		return
	case errors.Is(err, enroll.ErrInvalidSlot):
		writeError(w, http.StatusBadRequest, "INVALID_SLOT", err.Error())
		return
	case errors.Is(err, enroll.ErrOperatorMismatch):
		writeError(w, http.StatusForbidden, "OPERATOR_MISMATCH", err.Error())
		return
	case errors.Is(err, enroll.ErrAlreadyEnrolled):
		writeError(w, http.StatusConflict, "ALREADY_ENROLLED", err.Error())
		return
	case errors.Is(err, enroll.ErrPodTokenExpired):
		writeError(w, http.StatusForbidden, "POD_TOKEN_EXPIRED", err.Error())
		return
	case errors.Is(err, enroll.ErrPodIdentity):
		writeError(w, http.StatusForbidden, "POD_IDENTITY_MISMATCH", err.Error())
		return
	case err != nil:
		writeInternal(w, "create enrollment", err)
		return
	}

	status := http.StatusCreated
	if result.Existing {
		status = http.StatusOK
	}
	writeJSON(w, status, enrollmentResponse{
		EnrollmentID:   result.ID.String(),
		LeaseExpiresAt: result.LeaseExpires,
		Slot:           strPtr(result.Slot),
	})
}

// deleteEnrollment is idempotent: revoking an id that is already revoked, or that never existed,
// still answers 204. enroll.Service.Revoke returns ErrNotLive for exactly that no-op case (by
// design, so it never writes a spurious audit row for an enrollment that never transitioned);
// DELETE's own idempotent semantics mean the caller doesn't need to know or care which happened.
// Ownership is checked first: a launcher credential may only revoke an enrollment of its own
// operator, and a pod enrollment, which carries no operator, only when it is the credential that
// enrolled the pod (enroll.Service.Revoke says why, and what that leaves a restarted Legion daemon)
// — so a mismatch is always 403 (OPERATOR_MISMATCH, or NOT_ENROLLING_CREDENTIAL for another
// service credential's pod), never a no-op 204, regardless of whether the target enrollment is live.
func (s *server) deleteEnrollment(w http.ResponseWriter, r *http.Request, cred enroll.Credential) {
	id, ok := pathUUID(w, r, "id", "ENROLLMENT_ID_INPUT", "enrollment")
	if !ok {
		return
	}
	err := s.deps.Enroll.Revoke(r.Context(), cred, id, record.LauncherActor(cred.ID.String()))
	switch {
	case errors.Is(err, enroll.ErrOperatorMismatch):
		writeError(w, http.StatusForbidden, "OPERATOR_MISMATCH", err.Error())
		return
	case errors.Is(err, enroll.ErrNotEnrollingCredential):
		writeError(w, http.StatusForbidden, "NOT_ENROLLING_CREDENTIAL", err.Error())
		return
	case err != nil && !errors.Is(err, enroll.ErrNotLive):
		writeInternal(w, "revoke enrollment", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// renewEnrollment renews the caller's own enrollment. The proof-verified enrollment id is
// authoritative — a session can only ever act as itself — so a URL {id} that names a different
// enrollment is refused rather than silently honored or silently ignored. A pod's renewal carries
// its current projected token in the body, which enroll.Service.Renew verifies against the pod UID
// and service account the enrollment was made for; a pod enrollment that has never renewed with
// one still renews with no body, logged here, naming the enrollment, until the token-less path is
// removed (Renew says when).
func (s *server) renewEnrollment(w http.ResponseWriter, r *http.Request, id string) {
	urlID, ok := pathUUID(w, r, "id", "ENROLLMENT_ID_INPUT", "enrollment")
	if !ok {
		return
	}
	if urlID != id {
		writeError(w, http.StatusForbidden, "NOT_YOURS", "a session may only renew its own enrollment")
		return
	}
	var body renewEnrollmentBody
	if r.ContentLength != 0 && !readJSON(w, r, &body, "INVALID_RENEWAL") {
		return
	}
	renewal, err := s.deps.Enroll.Renew(r.Context(), id, derefOr(body.PodToken, ""))
	switch {
	case errors.Is(err, enroll.ErrNotLive):
		writeError(w, http.StatusUnauthorized, "ENROLLMENT_NOT_LIVE", "enrollment is not live")
		return
	case errors.Is(err, enroll.ErrPodTokenExpired):
		writeError(w, http.StatusForbidden, "POD_TOKEN_EXPIRED", err.Error())
		return
	case errors.Is(err, enroll.ErrPodIdentity):
		writeError(w, http.StatusForbidden, "POD_IDENTITY_MISMATCH", err.Error())
		return
	case errors.Is(err, enroll.ErrPodTokenRequired):
		slog.Warn("broker: pod enrollment renewal refused: projected token required", "enrollment", id)
		writeError(w, http.StatusForbidden, "POD_TOKEN_REQUIRED", err.Error())
		return
	case err != nil:
		writeInternal(w, "renew enrollment", err)
		return
	}
	if renewal.WithoutPodToken {
		slog.Warn("broker: pod enrollment renewed without its projected token", "enrollment", id, "pod_uid", renewal.RuntimeID, "slot", renewal.Slot)
	}
	writeJSON(w, http.StatusOK, leaseResponse{LeaseExpiresAt: renewal.LeaseExpires})
}

func (s *server) readSelf(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()
	enr, err := s.deps.Enroll.Get(ctx, id)
	if errors.Is(err, enroll.ErrNotLive) {
		writeError(w, http.StatusUnauthorized, "PROOF_INVALID", "enrollment is not live")
		return
	}
	if err != nil {
		writeInternal(w, "read enrollment", err)
		return
	}
	grants, err := s.deps.Machine.LiveGrants(ctx, id)
	if err != nil {
		writeInternal(w, "read live grants", err)
		return
	}
	if grants == nil {
		grants = []requests.Grant{}
	}
	writeJSON(w, http.StatusOK, selfResponse{
		EnrollmentID:   enr.ID.String(),
		Grants:         grants,
		Kind:           enr.Kind,
		LeaseExpiresAt: enr.LeaseExpires,
		Operator:       enr.Operator,
		Slot:           strPtr(enr.Slot),
	})
}

func derefOr(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}
