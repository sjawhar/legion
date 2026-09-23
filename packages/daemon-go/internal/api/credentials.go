package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/credential"
)

// GrantRequest is a request to mint one short-lived grant. Its session form preserves the shipped
// route wire shape: a registered pane identifies its session, tree, issue, and capability secret.
// An empty form is the controller form and must instead carry the operator bearer header.
type GrantRequest struct {
	SessionID string `json:"sessionId"`
	Secret    string `json:"secret"`
	Tree      string `json:"tree"`
	Issue     string `json:"issue"`
}

// GrantResponse is the bearer handle a pane stores in its one-command grant file.
type GrantResponse struct {
	GrantID   string `json:"grantId"`
	ExpiresAt string `json:"expiresAt"`
}

// GrantCredentialRequest is the only request accepted by the routes that redeem a grant.
type GrantCredentialRequest struct {
	GrantID string `json:"grantId"`
}

// GitHubTokenResponse is the token `legion gh` receives for its one child process.
type GitHubTokenResponse struct {
	Token    string `json:"token"`
	AppLogin string `json:"appLogin"`
}

// GitCredentialResponse is the logical credential a git helper writes in the credential protocol.
type GitCredentialResponse struct {
	Username string `json:"username"`
	Password string `json:"password"`
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

func (s *server) grant(w http.ResponseWriter, r *http.Request) {
	var req GrantRequest
	if !readBody(w, r, &req) {
		return
	}
	claimForm := req.SessionID != "" || req.Secret != "" || req.Tree != "" || req.Issue != ""
	if claimForm {
		if !requireFields(w, field{"sessionId", req.SessionID}, field{"secret", req.Secret}, field{"tree", req.Tree}, field{"issue", req.Issue}) {
			return
		}
		claims, err := s.supervisor.Claims(r.Context())
		if err != nil {
			s.log.Error("api: list claims to mint grant", "error", err)
			writeFailure(w, http.StatusInternalServerError, "GRANT_MINT_FAILED", "could not mint grant")
			return
		}
		for _, current := range claims {
			if current.Session != req.SessionID || current.Tree != req.Tree || current.Issue != req.Issue {
				continue
			}
			machine, authenticated := s.authenticated(current.Token, req.Secret)
			if !authenticated {
				break
			}
			grant, err := s.grants.Mint(machine.Claim())
			if err != nil {
				s.log.Error("api: mint claim grant", "error", err)
				writeFailure(w, http.StatusInternalServerError, "GRANT_MINT_FAILED", "could not mint grant")
				return
			}
			writeJSON(w, http.StatusOK, GrantResponse{GrantID: grant.ID, ExpiresAt: grant.ExpiresAt.UTC().Format(timeFormat)})
			return
		}
		writeFailure(w, http.StatusForbidden, "INVALID_SESSION_SECRET", claim.InvalidSecret.Message)
		return
	}
	if !s.operatorAuthorized(r) {
		writeFailure(w, http.StatusForbidden, "INVALID_OPERATOR_TOKEN", invalidOperatorToken)
		return
	}
	grant, err := s.grants.MintController()
	if err != nil {
		s.log.Error("api: mint controller grant", "error", err)
		writeFailure(w, http.StatusInternalServerError, "GRANT_MINT_FAILED", "could not mint grant")
		return
	}
	writeJSON(w, http.StatusOK, GrantResponse{GrantID: grant.ID, ExpiresAt: grant.ExpiresAt.UTC().Format(timeFormat)})
}

const timeFormat = "2006-01-02T15:04:05.999999999Z07:00"

func (s *server) githubToken(w http.ResponseWriter, r *http.Request) {
	grant, ok := s.redeemRepositoryGrant(w, r)
	if !ok {
		return
	}
	lease, ok := s.leaseForGrant(w, r, grant, appauth.AppRoleFor(grant.Role))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, GitHubTokenResponse{Token: lease.Token, AppLogin: lease.Identity.Name})
}

func (s *server) gitCredential(w http.ResponseWriter, r *http.Request) {
	grant, ok := s.redeemRepositoryGrant(w, r)
	if !ok {
		return
	}
	lease, ok := s.leaseForGrant(w, r, grant, appauth.AppRoleFor(grant.Role))
	if !ok {
		return
	}
	response := GitCredentialResponse{Username: "x-access-token", Password: lease.Token}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintf(w, "username=%s\npassword=%s\n", response.Username, response.Password)
}

func (s *server) provisioningCredential(w http.ResponseWriter, r *http.Request) {
	grant, ok := s.redeemRepositoryGrant(w, r)
	if !ok {
		return
	}
	if grant.Role != claim.RoleArchitect {
		writeFailure(w, http.StatusForbidden, "ARCHITECT_REQUIRED", "provisioning credentials require the tree architect")
		return
	}
	lease, ok := s.leaseForGrant(w, r, grant, appauth.Implement)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, GitHubTokenResponse{Token: lease.Token, AppLogin: lease.Identity.Name})
}

func (s *server) redeemRepositoryGrant(w http.ResponseWriter, r *http.Request) (credential.Grant, bool) {
	var req GrantCredentialRequest
	if !readBody(w, r, &req) || !requireFields(w, field{"grantId", req.GrantID}) {
		return credential.Grant{}, false
	}
	grant, err := s.grants.Redeem(req.GrantID)
	if err != nil {
		code := "GRANT_USED"
		message := "grant is unavailable"
		if errors.Is(err, credential.ErrExpired) {
			code, message = "GRANT_EXPIRED", "grant expired"
		}
		writeFailure(w, http.StatusForbidden, code, message)
		return credential.Grant{}, false
	}
	if grant.Controller {
		writeFailure(w, http.StatusForbidden, "CONTROLLER_HAS_NO_REPOSITORY", "controller grants have no repository")
		return credential.Grant{}, false
	}
	return grant, true
}

func (s *server) leaseForGrant(w http.ResponseWriter, r *http.Request, grant credential.Grant, role appauth.AppRole) (appauth.Lease, bool) {
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
		s.log.Error("api: mint GitHub App token", "role", grant.Role, "owner", s.githubOwner, "error", err)
		writeFailure(w, http.StatusBadGateway, "GITHUB_TOKEN_FAILED", "GitHub token exchange failed")
		return appauth.Lease{}, false
	}
	machine, supervised := s.supervisor.Machine(grant.Claim)
	if !supervised || !s.grants.StillMatches(grant, machine.Claim()) {
		writeFailure(w, http.StatusForbidden, "GRANT_REVOKED", "grant no longer belongs to an authenticated claim")
		return appauth.Lease{}, false
	}
	return lease, true
}

func (s *server) operatorAuthorized(r *http.Request) bool {
	given, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return s.operatorSet && bearer && secureBearerEqual(s.operatorHash, given)
}
