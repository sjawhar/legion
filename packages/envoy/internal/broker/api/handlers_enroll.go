package api

import (
	"errors"
	"net/http"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// createEnrollmentBody is POST /v1/enrollments's body (the AGENTC-393 overview document's
// enrollment route). It names no approver: the rules pick a request's approver at request time,
// never at enrollment. slot is optional and pod-only: omitted or "" is the runtime's one
// enrollment, and a slot names one of several independent enrollments of the same pod
// (enroll.Enrollment.Slot), chosen by the launcher whose proof authenticates the call.
type createEnrollmentBody struct {
	Kind       string  `json:"kind"`
	RuntimeID  string  `json:"runtime_id"`
	Slot       string  `json:"slot"`
	Operator   *string `json:"operator"`
	Thumbprint string  `json:"thumbprint"`
	SessionID  *string `json:"session_id"`
	PodToken   *string `json:"pod_token"`
}

// createEnrollment authenticates by launcher proof now (payload carries "lid"), never a bearer
// token; cred is the launcher credential's own identity, read by server.authenticate from the
// verified proof's launcher id.
func (s *server) createEnrollment(w http.ResponseWriter, r *http.Request, cred enroll.Credential) {
	var body createEnrollmentBody
	if !readJSON(w, r, &body, "INVALID_ENROLLMENT") {
		return
	}
	if body.Kind != "box" && body.Kind != "host" && body.Kind != "pod" {
		writeError(w, http.StatusBadRequest, "INVALID_KIND", `kind must be "box", "host", or "pod"`)
		return
	}

	result, err := s.deps.Enroll.Create(r.Context(), cred, enroll.Enrollment{
		Kind:       body.Kind,
		RuntimeID:  body.RuntimeID,
		Slot:       body.Slot,
		Operator:   body.Operator,
		Thumbprint: body.Thumbprint,
		SessionID:  body.SessionID,
		PodToken:   derefOr(body.PodToken, ""),
	})
	switch {
	case errors.Is(err, enroll.ErrInvalidSlot):
		writeError(w, http.StatusBadRequest, "INVALID_SLOT", err.Error())
		return
	case errors.Is(err, enroll.ErrOperatorMismatch):
		writeError(w, http.StatusForbidden, "OPERATOR_MISMATCH", err.Error())
		return
	case errors.Is(err, enroll.ErrAlreadyEnrolled):
		writeError(w, http.StatusConflict, "ALREADY_ENROLLED", err.Error())
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
	writeJSON(w, status, map[string]any{
		"enrollment_id":    result.ID.String(),
		"slot":             strPtr(result.Slot),
		"lease_expires_at": result.LeaseExpires,
	})
}

// deleteEnrollment is idempotent: revoking an id that is already revoked, or that never existed,
// still answers 204. enroll.Service.Revoke returns ErrNotLive for exactly that no-op case (by
// design, so it never writes a spurious audit row for an enrollment that never transitioned);
// DELETE's own idempotent semantics mean the caller doesn't need to know or care which happened.
// Ownership is checked first: a launcher credential may only revoke an enrollment of its own
// operator (or, for a service credential, any pod enrollment) — the same trust boundary
// enroll.Service.Create enforces when creating one — so a mismatch is always 403
// OPERATOR_MISMATCH, never a no-op 204, regardless of whether the target enrollment is live.
func (s *server) deleteEnrollment(w http.ResponseWriter, r *http.Request, cred enroll.Credential) {
	id, ok := pathUUID(w, r, "id", "ENROLLMENT_ID_INPUT", "enrollment")
	if !ok {
		return
	}
	err := s.deps.Enroll.Revoke(r.Context(), cred, id, "launcher:"+cred.ID.String())
	if errors.Is(err, enroll.ErrOperatorMismatch) {
		writeError(w, http.StatusForbidden, "OPERATOR_MISMATCH", err.Error())
		return
	}
	if err != nil && !errors.Is(err, enroll.ErrNotLive) {
		writeInternal(w, "revoke enrollment", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// renewEnrollment renews the caller's own enrollment. The proof-verified enrollment id is
// authoritative — a session can only ever act as itself — so a URL {id} that names a different
// enrollment is refused rather than silently honored or silently ignored.
func (s *server) renewEnrollment(w http.ResponseWriter, r *http.Request, id string) {
	urlID, ok := pathUUID(w, r, "id", "ENROLLMENT_ID_INPUT", "enrollment")
	if !ok {
		return
	}
	if urlID != id {
		writeError(w, http.StatusForbidden, "NOT_YOURS", "a session may only renew its own enrollment")
		return
	}
	expires, err := s.deps.Enroll.Renew(r.Context(), id)
	if errors.Is(err, enroll.ErrNotLive) {
		writeError(w, http.StatusUnauthorized, "PROOF_INVALID", "enrollment is not live")
		return
	}
	if err != nil {
		writeInternal(w, "renew enrollment", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lease_expires_at": expires})
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
	writeJSON(w, http.StatusOK, map[string]any{
		"enrollment_id":    enr.ID.String(),
		"kind":             enr.Kind,
		"operator":         enr.Operator,
		"slot":             strPtr(enr.Slot),
		"lease_expires_at": enr.LeaseExpires,
		"grants":           grants,
	})
}

func derefOr(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}
