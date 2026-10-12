package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// GrantRequest is a request to mint one short-lived grant, in one of three forms. The claim form
// preserves the shipped route wire shape: a registered pane identifies its session, tree, issue,
// and capability secret. The controller-session form is the session registered with the current
// controller capability and its registration secret, with no tree and no issue (the shipped
// controller form, credentials.ts:26-47). An empty form is the operator's and must instead carry
// the operator bearer header.
type GrantRequest struct {
	SessionID string `json:"sessionId"`
	Secret    string `json:"secret"`
	Tree      string `json:"tree"`
	Issue     string `json:"issue"`
}

// GrantResponse is the bearer handle a pane stores in its per-command grant file.
type GrantResponse struct {
	GrantID   string `json:"grantId"`
	ExpiresAt string `json:"expiresAt"`
}

// Failure is the refusal shape for the credential and workflow routes. Its stable code lets a CLI
// distinguish an expired bearer from an unavailable grant without exposing the bearer itself.
type Failure struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

func writeFailure(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, Failure{Code: code, Error: message})
}

// requireFailureFields refuses the first field that is blank, by its wire name, in the Failure
// shape every credential and workflow refusal takes. The claim and operator routes refuse with
// their sentence alone (requireFields).
func requireFailureFields(w http.ResponseWriter, fields ...field) bool {
	for _, f := range fields {
		if strings.TrimSpace(f.value) == "" {
			writeFailure(w, http.StatusBadRequest, "MISSING_FIELD", f.name+" is required")
			return false
		}
	}
	return true
}

func (s *server) grant(w http.ResponseWriter, r *http.Request) {
	var req GrantRequest
	if !s.readBody(w, r, &req, s.grantIdentityCheck) {
		return
	}
	switch {
	case req.Tree != "" || req.Issue != "":
		if requireFailureFields(w, field{"sessionId", req.SessionID}, field{"secret", req.Secret}, field{"tree", req.Tree}, field{"issue", req.Issue}) {
			s.claimGrant(w, r, req)
		}
	case req.SessionID != "" || req.Secret != "":
		if requireFailureFields(w, field{"sessionId", req.SessionID}, field{"secret", req.Secret}) {
			s.controllerSessionGrant(w, r, req)
		}
	case !s.operatorAuthorized(r):
		writeFailure(w, http.StatusForbidden, "INVALID_OPERATOR_TOKEN", invalidOperatorToken)
	default:
		s.mintControllerGrant(w)
	}
}

// claimGrant is the claim form: the claim registered as req's session on its tree and issue,
// proving its registration secret, mints a grant for that claim.
func (s *server) claimGrant(w http.ResponseWriter, r *http.Request, req GrantRequest) {
	machine, authenticated, err := s.claimForGrant(r.Context(), req.SessionID, req.Secret, req.Tree, req.Issue)
	if err != nil {
		s.log.Error("api: list claims to mint grant", "error", err)
		writeFailure(w, http.StatusInternalServerError, "GRANT_MINT_FAILED", "could not mint grant")
		return
	}
	if !authenticated {
		writeFailure(w, http.StatusForbidden, "INVALID_SESSION_SECRET", claim.InvalidSecret.Message)
		return
	}
	if s.refuseIfStaleClaimFailure(w, r, machine) {
		return
	}
	grant, err := s.grants.Mint(machine.Claim())
	if err != nil {
		s.log.Error("api: mint claim grant", "error", err)
		writeFailure(w, http.StatusInternalServerError, "GRANT_MINT_FAILED", "could not mint grant")
		return
	}
	writeJSON(w, http.StatusOK, GrantResponse{GrantID: grant.ID, ExpiresAt: grant.ExpiresAt.UTC().Format(timeFormat)})
}

// claimForGrant finds the claim session, tree and issue name and authenticates it with secret, as
// the claim form of the grants route does; the lenient identity read of an undecodable grants
// body (grantIdentityCheck) uses it too.
func (s *server) claimForGrant(ctx context.Context, sessionID, secret, tree, issue string) (*supervise.Machine, bool, error) {
	claims, err := s.supervisor.Claims(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, current := range claims {
		if current.Session != sessionID || current.Tree != tree || current.Issue != issue {
			continue
		}
		m, ok := s.authenticated(current.Token, secret)
		return m, ok, nil
	}
	return nil, false, nil
}

// grantIdentityCheck is readBody's identity check for the grants route: the claim form
// ({sessionId, secret, tree, issue}) or the controller form ({sessionId, secret}), told apart the
// same way grant's own dispatch does. The operator form (neither session nor secret) has no stale
// identity.
func (s *server) grantIdentityCheck(w http.ResponseWriter, r *http.Request, body []byte) bool {
	var id GrantRequest
	if json.Unmarshal(body, &id) != nil {
		return false
	}
	switch {
	case id.Tree != "" || id.Issue != "":
		if id.SessionID == "" || id.Secret == "" {
			return false
		}
		machine, authenticated, err := s.claimForGrant(r.Context(), id.SessionID, id.Secret, id.Tree, id.Issue)
		if err != nil || !authenticated {
			return false
		}
		return s.refuseIfStaleClaimFailure(w, r, machine)
	case id.SessionID != "" || id.Secret != "":
		return s.refuseIfStaleControllerSession(w, r, id.SessionID, id.Secret)
	default:
		return false
	}
}

const timeFormat = "2006-01-02T15:04:05.999999999Z07:00"

// redeem is every route's one grant check. A grant serves each request of the command it was
// minted for until it expires, and a claim grant only while its claim is supervised under the
// registration that minted it; the credential routes check that again after their GitHub await.
// A grant whose claim or controller record speaks another plugin contract than the daemon's is
// refused the stale-caller 409 here, once every other check has passed: every grant-bearing
// route takes its caller's staleness from this one place.
func (s *server) redeem(w http.ResponseWriter, r *http.Request, id string) (credential.Grant, bool) {
	grant, err := s.grants.Redeem(id)
	if errors.Is(err, credential.ErrExpired) {
		writeFailure(w, http.StatusForbidden, "GRANT_EXPIRED", "grant expired")
		return credential.Grant{}, false
	}
	if err != nil {
		writeFailure(w, http.StatusForbidden, "GRANT_UNAVAILABLE", "grant is unavailable")
		return credential.Grant{}, false
	}
	if grant.Controller {
		record, found, err := s.controller.Controller(r.Context(), s.project)
		if err != nil {
			s.log.Error("api: read the controller record to redeem a grant", "error", err)
			writeFailure(w, http.StatusInternalServerError, "GRANT_UNAVAILABLE", "grant is unavailable")
			return credential.Grant{}, false
		}
		if found && s.refuseIfStaleController(w, r, record) {
			return credential.Grant{}, false
		}
		return grant, true
	}
	machine, supervised := s.supervisor.Machine(grant.Claim)
	if !supervised || !s.grants.StillMatches(grant, machine.Claim()) {
		writeFailure(w, http.StatusForbidden, "GRANT_REVOKED", grantRevoked)
		return credential.Grant{}, false
	}
	if s.refuseIfStaleClaimFailure(w, r, machine) {
		return credential.Grant{}, false
	}
	return grant, true
}

const grantRevoked = "grant no longer belongs to an authenticated claim"

// claimHolds says whether grant's claim is supervised and still holds the capability that minted it.
func (s *server) claimHolds(grant credential.Grant) bool {
	machine, supervised := s.supervisor.Machine(grant.Claim)
	return supervised && s.grants.StillMatches(grant, machine.Claim())
}

// redeemIdentityCheck is readBody's identity check for every grant-bearing route: {grantId}. A
// grant that still redeems, naming a claim or the controller record whose recorded contract
// differs, gets the 409 instead of the 400 every other undecodable body gets; an expired,
// unavailable, or revoked grant, or a current caller's, leaves the body's own decode error
// standing.
func (s *server) redeemIdentityCheck(w http.ResponseWriter, r *http.Request, body []byte) bool {
	var id struct {
		GrantID string `json:"grantId"`
	}
	if json.Unmarshal(body, &id) != nil || id.GrantID == "" {
		return false
	}
	grant, err := s.grants.Redeem(id.GrantID)
	if err != nil {
		return false
	}
	if grant.Controller {
		s.controllerMu.Lock()
		record, found, err := s.controller.Controller(r.Context(), s.project)
		s.controllerMu.Unlock()
		if err != nil || !found {
			return false
		}
		return s.refuseIfStaleController(w, r, record)
	}
	machine, supervised := s.supervisor.Machine(grant.Claim)
	if !supervised || !s.grants.StillMatches(grant, machine.Claim()) {
		return false
	}
	return s.refuseIfStaleClaimFailure(w, r, machine)
}

// appLease is the GitHub App token mint every credential route needs: the token source and
// repository owner must be configured, and the mint itself must succeed. logRole names the role
// in the error log line; it carries no secret.
func (s *server) appLease(w http.ResponseWriter, r *http.Request, role appauth.AppRole, logRole string) (appauth.Lease, bool) {
	if s.tokens == nil {
		writeFailure(w, http.StatusInternalServerError, "GITHUB_TOKEN_SOURCE_UNAVAILABLE", "GitHub token source is unavailable")
		return appauth.Lease{}, false
	}
	if s.githubOwner == "" {
		writeFailure(w, http.StatusInternalServerError, "GITHUB_OWNER_UNCONFIGURED", "no repository owner is configured for GitHub App tokens")
		return appauth.Lease{}, false
	}
	lease, err := s.tokens.Token(r.Context(), role, s.githubOwner)
	if err != nil {
		s.log.Error("api: mint GitHub App token", "role", logRole, "owner", s.githubOwner, "error", err)
		writeFailure(w, http.StatusBadGateway, "GITHUB_TOKEN_FAILED", "GitHub token exchange failed")
		return appauth.Lease{}, false
	}
	return lease, true
}

// leaseForGrant is appLease for a claim's grant: the mint runs first since it is the slow step,
// then the grant's claim must still hold the capability that minted it, so a claim revoked while
// the mint was in flight is caught before its token is handed back.
func (s *server) leaseForGrant(w http.ResponseWriter, r *http.Request, grant credential.Grant, role appauth.AppRole) (appauth.Lease, bool) {
	lease, ok := s.appLease(w, r, role, string(grant.Role))
	if !ok {
		return appauth.Lease{}, false
	}
	if !s.claimHolds(grant) {
		writeFailure(w, http.StatusForbidden, "GRANT_REVOKED", grantRevoked)
		return appauth.Lease{}, false
	}
	return lease, true
}

func (s *server) operatorAuthorized(r *http.Request) bool {
	given, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return s.operatorSet && bearer && secureBearerEqual(s.operatorHash, given)
}
