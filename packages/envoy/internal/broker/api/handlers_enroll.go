// packages/envoy/internal/broker/api/handlers_enroll.go
package api

import (
	"errors"
	"net/http"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

type approverBody struct {
	Kind  string  `json:"kind"`
	Issue *string `json:"issue"`
}

type createEnrollmentBody struct {
	Kind       string       `json:"kind"`
	RuntimeID  string       `json:"runtime_id"`
	Operator   *string      `json:"operator"`
	Approver   approverBody `json:"approver"`
	Thumbprint string       `json:"thumbprint"`
	SessionID  *string      `json:"session_id"`
	PodToken   *string      `json:"pod_token"`
}

func (s *server) createEnrollment(w http.ResponseWriter, r *http.Request) {
	var body createEnrollmentBody
	if !readJSON(w, r, &body, "INVALID_ENROLLMENT") {
		return
	}
	if body.Kind != "box" && body.Kind != "host" && body.Kind != "pod" {
		writeError(w, http.StatusBadRequest, "INVALID_KIND", `kind must be "box", "host", or "pod"`)
		return
	}
	var approverIssue *string
	switch body.Approver.Kind {
	case "operator":
	case "issue_assignee":
		if body.Approver.Issue == nil || *body.Approver.Issue == "" {
			writeError(w, http.StatusBadRequest, "INVALID_APPROVER", `approver.issue is required when approver.kind is "issue_assignee"`)
			return
		}
		approverIssue = body.Approver.Issue
	default:
		writeError(w, http.StatusBadRequest, "INVALID_APPROVER", `approver.kind must be "operator" or "issue_assignee"`)
		return
	}

	cred, _ := r.Context().Value(ctxLauncher).(enroll.Credential)
	result, err := s.deps.Enroll.Create(r.Context(), cred, enroll.Enrollment{
		Kind:          body.Kind,
		RuntimeID:     body.RuntimeID,
		Operator:      body.Operator,
		ApproverKind:  body.Approver.Kind,
		ApproverIssue: approverIssue,
		Thumbprint:    body.Thumbprint,
		SessionID:     body.SessionID,
		PodToken:      derefOr(body.PodToken, ""),
	})
	switch {
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
		writeError(w, http.StatusInternalServerError, "INTERNAL", "create enrollment failed")
		return
	}

	status := http.StatusCreated
	if result.Existing {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"enrollment_id":    result.ID.String(),
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
func (s *server) deleteEnrollment(w http.ResponseWriter, r *http.Request) {
	cred, _ := r.Context().Value(ctxLauncher).(enroll.Credential)
	err := s.deps.Enroll.Revoke(r.Context(), cred, r.PathValue("id"), "launcher:"+cred.ID.String())
	if errors.Is(err, enroll.ErrOperatorMismatch) {
		writeError(w, http.StatusForbidden, "OPERATOR_MISMATCH", err.Error())
		return
	}
	if err != nil && !errors.Is(err, enroll.ErrNotLive) {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "revoke enrollment failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// renewEnrollment renews the caller's own enrollment. The proof-verified ctx enrollment id is
// authoritative — a session can only ever act as itself — so a URL {id} that names a different
// enrollment is refused rather than silently honored or silently ignored.
func (s *server) renewEnrollment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := ctx.Value(ctxEnrollment).(string)
	if urlID := r.PathValue("id"); urlID != id {
		writeError(w, http.StatusForbidden, "NOT_YOURS", "a session may only renew its own enrollment")
		return
	}
	expires, err := s.deps.Enroll.Renew(ctx, id)
	if errors.Is(err, enroll.ErrNotLive) {
		writeError(w, http.StatusUnauthorized, "PROOF_INVALID", "enrollment is not live")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "renew enrollment failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lease_expires_at": expires})
}

func (s *server) readSelf(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := ctx.Value(ctxEnrollment).(string)
	enr, err := s.deps.Enroll.Get(ctx, id)
	if errors.Is(err, enroll.ErrNotLive) {
		writeError(w, http.StatusUnauthorized, "PROOF_INVALID", "enrollment is not live")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "read enrollment failed")
		return
	}
	grants, err := s.deps.Machine.LiveGrants(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "read grants failed")
		return
	}
	if grants == nil {
		grants = []requests.Grant{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enrollment_id":    enr.ID.String(),
		"kind":             enr.Kind,
		"operator":         enr.Operator,
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
