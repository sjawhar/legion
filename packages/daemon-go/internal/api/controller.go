package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"net/http"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/controller"
)

// ControllerStore is the project's controller record: the capability `legion controller start`
// was last issued, and the session that registered with it (`store.Store`).
type ControllerStore interface {
	MintController(ctx context.Context, project string, capabilityHash []byte) (uint64, error)
	Controller(ctx context.Context, project string) (controller.Record, bool, error)
	RegisterController(ctx context.Context, project string, generation uint64, session string, secretHash []byte, at time.Time) (bool, error)
}

// ControllerSecretResponse is `POST /legion/v1/controller/secret`'s answer: the capability the
// controller's Oh My Pi registers with.
type ControllerSecretResponse struct {
	Secret string `json:"secret"`
}

// ControllerRole is the role a controller registration names: the operator's controller, which
// holds no claim — the operator launches it and nothing supervises it — so it is no claim.Role.
const ControllerRole = "controller"

// ControllerRegisterResponse is `POST /legion/v1/claims/register`'s answer to a session that
// registered with the controller capability: the project's controller role token
// (claim.ControllerToken), ControllerRole, the capability's generation, and the secret its
// controller grants authenticate with. It has no tree and no issue; a claim's registration is
// claim.RegisterResponse.
type ControllerRegisterResponse struct {
	ClaimToken claim.Token `json:"claimToken"`
	Role       string      `json:"role"`
	Generation uint64      `json:"generation"`
	Secret     string      `json:"secret"`
}

// controllerSecret is `legion controller start`'s one daemon call (the shipped
// handleControllerSecret, packages/daemon/src/daemon/api/routes/controller.ts:61-88): the
// operator's bearer, compared in constant time, buys a fresh controller capability. The mint
// replaces the previous capability and its registration, and ends every controller grant, so the
// controller it replaces stops being able to act the moment this answers — last start wins. Every
// refusal is one log line and mints nothing.
func (s *server) controllerSecret(w http.ResponseWriter, r *http.Request) {
	if !s.operatorAuthorized(r) {
		s.log.Warn("api: refused a controller secret: no operator bearer, or the wrong one")
		writeJSON(w, http.StatusForbidden, errorBody(invalidOperatorToken))
		return
	}
	var req struct{}
	if !readBody(w, r, &req) {
		return
	}
	secret := rand.Text()
	s.controllerMu.Lock()
	defer s.controllerMu.Unlock()
	generation, err := s.controller.MintController(context.WithoutCancel(r.Context()), s.project, capabilityHash(secret))
	if err != nil {
		s.log.Error("api: mint a controller capability", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("controller secret failed: the daemon could not record it"))
		return
	}
	s.grants.RevokeControllers()
	s.log.Info("api: minted a controller capability; the previous controller's registration and grants are revoked", "generation", generation)
	writeJSON(w, http.StatusOK, ControllerSecretResponse{Secret: secret})
}

// registerController is a registration whose token is no launch's boot token: it registers
// req's session as the project's controller when the token is the current controller capability,
// and answers the one refusal an unknown token gets otherwise. No boot gate checks the operator's
// Oh My Pi — the daemon gates only the panes it launches — so a controller whose plugin speaks
// another Go daemon API contract is refused here, naming both, and nothing is recorded. The
// registration is issued a secret of its own, persisted by its hash before this answers, which
// the session's controller grants authenticate with.
func (s *server) registerController(w http.ResponseWriter, r *http.Request, req claim.RegisterRequest) {
	ctx := context.WithoutCancel(r.Context())
	s.controllerMu.Lock()
	defer s.controllerMu.Unlock()
	record, found, err := s.controller.Controller(ctx, s.project)
	if err != nil {
		s.log.Error("api: read the controller record for a registration", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("register failed: the daemon could not read its controller record"))
		return
	}
	if !found || subtle.ConstantTimeCompare(record.CapabilityHash, capabilityHash(req.BootToken)) != 1 {
		writeJSON(w, claim.InvalidBootToken.Status, claim.InvalidBootToken)
		return
	}
	if req.PluginContract != GoDaemonAPIVersion {
		s.log.Warn("api: refused a controller registration: its plugin speaks another Go daemon API contract",
			"session", req.SessionID, "pluginContract", req.PluginContract, "goDaemonApiVersion", GoDaemonAPIVersion)
		writeJSON(w, http.StatusConflict, errorBody(fmt.Sprintf(
			"pi-legion-envoy speaks Go daemon API contract %d; this daemon requires %d", req.PluginContract, GoDaemonAPIVersion)))
		return
	}
	secret := rand.Text()
	registered, err := s.controller.RegisterController(ctx, s.project, record.Generation, req.SessionID, capabilityHash(secret), time.Now().UTC())
	if err != nil {
		s.log.Error("api: record a controller registration", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("register failed: the daemon could not record it"))
		return
	}
	if !registered {
		writeJSON(w, claim.InvalidBootToken.Status, claim.InvalidBootToken)
		return
	}
	token := claim.ControllerToken(s.project)
	s.log.Info("api: controller registered", "claim", token, "generation", record.Generation,
		"session", req.SessionID, "agent", req.AgentID)
	writeJSON(w, http.StatusOK, ControllerRegisterResponse{
		ClaimToken: token,
		Role:       ControllerRole,
		Generation: record.Generation,
		Secret:     secret,
	})
}

// controllerSessionGrant is the grants route's controller-session form: the session registered
// with the current capability, proving the secret its registration was issued, mints a controller
// grant. Anything else answers the claim form's refusal.
func (s *server) controllerSessionGrant(w http.ResponseWriter, r *http.Request, req GrantRequest) {
	s.controllerMu.Lock()
	defer s.controllerMu.Unlock()
	record, found, err := s.controller.Controller(r.Context(), s.project)
	if err != nil {
		s.log.Error("api: read the controller record for a grant", "error", err)
		writeFailure(w, http.StatusInternalServerError, "GRANT_MINT_FAILED", "could not mint grant")
		return
	}
	if !found || !record.Registered() || record.Session != req.SessionID ||
		subtle.ConstantTimeCompare(record.SecretHash, capabilityHash(req.Secret)) != 1 {
		writeFailure(w, http.StatusForbidden, "INVALID_SESSION_SECRET", claim.InvalidSecret.Message)
		return
	}
	s.mintControllerGrant(w)
}

// mintControllerGrant answers a controller grant for a caller its route has authorised.
func (s *server) mintControllerGrant(w http.ResponseWriter) {
	grant, err := s.grants.MintController()
	if err != nil {
		s.log.Error("api: mint controller grant", "error", err)
		writeFailure(w, http.StatusInternalServerError, "GRANT_MINT_FAILED", "could not mint grant")
		return
	}
	writeJSON(w, http.StatusOK, GrantResponse{GrantID: grant.ID, ExpiresAt: grant.ExpiresAt.UTC().Format(timeFormat)})
}
