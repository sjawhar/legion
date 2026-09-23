package api

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// EmptyResponse is every accepted mutating route whose caller needs no result beyond success.
type EmptyResponse struct{}

// GateRegisterRequest records the document version an architect asked a human to approve.
type GateRegisterRequest struct {
	GrantID    string `json:"grantId"`
	Issue      string `json:"issue"`
	ArtifactID string `json:"artifactId"`
	Version    int    `json:"version"`
}

// GateRegisterResponse confirms the gate registration fact committed.
type GateRegisterResponse struct{}

// WaveReleaseRequest asks an architect to move selected children in its tree to todo.
type WaveReleaseRequest struct {
	GrantID string   `json:"grantId"`
	Issues  []string `json:"issues"`
}

// WaveReleaseResponse lists the issues whose synchronous Dispatch update was accepted.
type WaveReleaseResponse struct {
	Released []string `json:"released"`
}

// PhaseBackwardRequest asks a phase worker to return its own issue to an earlier phase.
type PhaseBackwardRequest struct {
	GrantID string      `json:"grantId"`
	To      phase.Phase `json:"to"`
	Reason  string      `json:"reason"`
}

// PhaseBackwardResponse confirms the phase-backward fact committed.
type PhaseBackwardResponse struct{}

// PhaseRetryRequest records the tree architect's retry-or-escalate choice for a held issue.
type PhaseRetryRequest struct {
	GrantID  string                         `json:"grantId"`
	Issue    string                         `json:"issue"`
	Decision intake.RetryOrEscalateDecision `json:"decision"`
}

// PhaseRetryResponse confirms the retry-or-escalate fact committed.
type PhaseRetryResponse struct{}

// SignOffRequest records the owning architect's post-production-check sign-off.
type SignOffRequest struct {
	GrantID string `json:"grantId"`
	Issue   string `json:"issue"`
}

// SignOffResponse confirms the sign-off fact committed.
type SignOffResponse struct{}

func routeFields(w http.ResponseWriter, fields ...field) bool {
	for _, f := range fields {
		if strings.TrimSpace(f.value) == "" {
			writeFailure(w, http.StatusBadRequest, "MISSING_FIELD", f.name+" is required")
			return false
		}
	}
	return true
}

func (s *server) architectGrant(w http.ResponseWriter, id string) (credential.Grant, bool) {
	grant, ok := s.redeem(w, id)
	if !ok {
		return credential.Grant{}, false
	}
	if grant.Controller || grant.Role != claim.RoleArchitect {
		writeFailure(w, http.StatusForbidden, "ARCHITECT_REQUIRED", "architect grant required")
		return credential.Grant{}, false
	}
	return grant, true
}

func (s *server) phaseWorkerGrant(w http.ResponseWriter, id string) (credential.Grant, bool) {
	grant, ok := s.redeem(w, id)
	if !ok {
		return credential.Grant{}, false
	}
	if grant.Controller || !isPhaseWorker(grant.Role) {
		writeFailure(w, http.StatusForbidden, "PHASE_WORKER_REQUIRED", "phase worker grant required")
		return credential.Grant{}, false
	}
	return grant, true
}

func isPhaseWorker(role claim.Role) bool {
	switch role {
	case claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger:
		return true
	default:
		return false
	}
}

// treeMember reads the record's tree root, the single source of tree membership after Task 3.15.
// The read authorizes the caller; the fact itself still enters workflow through ApplyFact's one
// write transaction.
func (s *server) treeMember(ctx context.Context, tree, issue string) (exists, member bool, err error) {
	if s.pool == nil || s.records == nil {
		return false, false, errors.New("record dependencies are unavailable")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return false, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	recorded, err := s.records.Issue(ctx, tx, issue)
	if err != nil {
		return false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, false, err
	}
	if recorded == nil {
		return false, false, nil
	}
	return true, recorded.Tree == tree, nil
}

func (s *server) architectForIssue(w http.ResponseWriter, r *http.Request, grantID, issue string) (credential.Grant, bool) {
	grant, ok := s.architectGrant(w, grantID)
	if !ok {
		return credential.Grant{}, false
	}
	exists, member, err := s.treeMember(r.Context(), grant.Tree, issue)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read issue tree membership")
		return credential.Grant{}, false
	}
	if !exists {
		writeFailure(w, http.StatusNotFound, "ISSUE_NOT_FOUND", "issue is not recorded")
		return credential.Grant{}, false
	}
	if !member {
		writeFailure(w, http.StatusForbidden, "ISSUE_OUTSIDE_TREE", "issue is outside the architect tree")
		return credential.Grant{}, false
	}
	return grant, true
}

// requestFactID names one API request's fact. A grant serves every request its command makes, so
// the grant cannot name the request: each request is its own fact, applied by its one exchange.
func requestFactID(route string) string {
	return "api:" + route + ":" + rand.Text()
}

func (s *server) applyFact(w http.ResponseWriter, r *http.Request, eventID string, fact intake.Fact, response any) {
	if s.pool == nil {
		writeFailure(w, http.StatusInternalServerError, "FACTS_UNAVAILABLE", "fact intake is unavailable")
		return
	}
	result, err := intake.ApplyFact(r.Context(), s.pool, "api", eventID, fact, s.handlers...)
	if err != nil {
		writeFailure(w, http.StatusInternalServerError, "FACT_APPLY_FAILED", "could not apply fact")
		return
	}
	if result.Refusal != nil {
		writeFailure(w, result.Refusal.Status, result.Refusal.Code, result.Refusal.Message)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *server) gateRegister(w http.ResponseWriter, r *http.Request) {
	var req GateRegisterRequest
	if !readBody(w, r, &req) || !routeFields(w,
		field{"grantId", req.GrantID},
		field{"issue", req.Issue},
		field{"artifactId", req.ArtifactID},
	) {
		return
	}
	if !claim.IsIssueKey(req.Issue) {
		writeFailure(w, http.StatusBadRequest, "INVALID_ISSUE", "issue is not an issue key")
		return
	}
	if !validUUID(req.ArtifactID) {
		writeFailure(w, http.StatusBadRequest, "INVALID_ARTIFACT_ID", "artifactId must be a UUID")
		return
	}
	if req.Version < 1 {
		writeFailure(w, http.StatusBadRequest, "INVALID_VERSION", "version must be positive")
		return
	}
	artifactID := strings.ToLower(req.ArtifactID)
	if _, ok := s.architectForIssue(w, r, req.GrantID, req.Issue); !ok {
		return
	}
	s.applyFact(w, r, "gate:"+req.Issue+":"+artifactID+":"+strconv.Itoa(req.Version),
		intake.GateRegistered{Issue: req.Issue, ArtifactID: artifactID, Version: req.Version}, GateRegisterResponse{})
}

func (s *server) waveRelease(w http.ResponseWriter, r *http.Request) {
	var req WaveReleaseRequest
	if !readBody(w, r, &req) || !routeFields(w, field{"grantId", req.GrantID}) {
		return
	}
	grant, ok := s.architectGrant(w, req.GrantID)
	if !ok {
		return
	}
	if s.dispatch == nil {
		writeFailure(w, http.StatusInternalServerError, "DISPATCH_UNAVAILABLE", "Dispatch is unavailable")
		return
	}
	if req.Issues == nil {
		req.Issues = []string{}
	}
	for _, issue := range req.Issues {
		if !claim.IsIssueKey(issue) {
			writeFailure(w, http.StatusBadRequest, "INVALID_ISSUE", "issues must contain issue keys")
			return
		}
		exists, member, err := s.treeMember(r.Context(), grant.Tree, issue)
		if err != nil {
			writeFailure(w, http.StatusInternalServerError, "RECORD_UNAVAILABLE", "could not read issue tree membership")
			return
		}
		if !exists {
			writeFailure(w, http.StatusNotFound, "ISSUE_NOT_FOUND", "issue is not recorded")
			return
		}
		if !member {
			writeFailure(w, http.StatusForbidden, "ISSUE_OUTSIDE_TREE", "issue is outside the architect tree")
			return
		}
	}
	for _, issue := range req.Issues {
		if err := s.dispatch.SetStatus(r.Context(), issue, "todo"); err != nil {
			var dispatchError *dispatch.Error
			if errors.As(err, &dispatchError) {
				writeFailure(w, http.StatusBadGateway, dispatchError.Code, dispatchError.Message)
				return
			}
			writeFailure(w, http.StatusBadGateway, "DISPATCH_FAILED", "Dispatch status update failed")
			return
		}
	}
	writeJSON(w, http.StatusOK, WaveReleaseResponse{Released: req.Issues})
}

func (s *server) phaseBackward(w http.ResponseWriter, r *http.Request) {
	var req PhaseBackwardRequest
	if !readBody(w, r, &req) || !routeFields(w,
		field{"grantId", req.GrantID},
		field{"to", string(req.To)},
		field{"reason", req.Reason},
	) {
		return
	}
	if !validPhase(req.To) {
		writeFailure(w, http.StatusBadRequest, "INVALID_PHASE", "to is not a phase")
		return
	}
	grant, ok := s.phaseWorkerGrant(w, req.GrantID)
	if !ok {
		return
	}
	s.applyFact(w, r, requestFactID("phase/backward"),
		intake.BackwardMove{Issue: grant.Issue, Requester: grant.Role, To: req.To, Reason: req.Reason}, PhaseBackwardResponse{})
}

func (s *server) phaseRetry(w http.ResponseWriter, r *http.Request) {
	var req PhaseRetryRequest
	if !readBody(w, r, &req) || !routeFields(w,
		field{"grantId", req.GrantID},
		field{"issue", req.Issue},
		field{"decision", string(req.Decision)},
	) {
		return
	}
	if !claim.IsIssueKey(req.Issue) {
		writeFailure(w, http.StatusBadRequest, "INVALID_ISSUE", "issue is not an issue key")
		return
	}
	if req.Decision != intake.RetryDecision && req.Decision != intake.EscalateDecision {
		writeFailure(w, http.StatusBadRequest, "INVALID_DECISION", "decision must be retry or escalate")
		return
	}
	if _, ok := s.architectForIssue(w, r, req.GrantID, req.Issue); !ok {
		return
	}
	s.applyFact(w, r, requestFactID("phase/retry"),
		intake.RetryOrEscalate{Issue: req.Issue, Decision: req.Decision}, PhaseRetryResponse{})
}

func (s *server) signOff(w http.ResponseWriter, r *http.Request) {
	var req SignOffRequest
	if !readBody(w, r, &req) || !routeFields(w, field{"grantId", req.GrantID}, field{"issue", req.Issue}) {
		return
	}
	if !claim.IsIssueKey(req.Issue) {
		writeFailure(w, http.StatusBadRequest, "INVALID_ISSUE", "issue is not an issue key")
		return
	}
	if _, ok := s.architectForIssue(w, r, req.GrantID, req.Issue); !ok {
		return
	}
	s.applyFact(w, r, requestFactID("signoff"), intake.SignOff{Issue: req.Issue}, SignOffResponse{})
}

func validPhase(value phase.Phase) bool {
	switch value {
	case phase.Admitted, phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing,
		phase.Retro, phase.Merging, phase.AwaitingMerge, phase.ProductionCheck, phase.Done, phase.Held:
		return true
	default:
		return false
	}
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') ||
			(character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}
