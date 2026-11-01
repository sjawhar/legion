package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/controller"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// ControllerStore is the project's controller record: the capability `legion controller start`
// was last issued, and the session that registered with it (`store.Store`).
type ControllerStore interface {
	MintController(ctx context.Context, project string, capabilityHash []byte) (uint64, error)
	Controller(ctx context.Context, project string) (controller.Record, bool, error)
	RegisterController(ctx context.Context, project string, generation uint64, session string, secretHash []byte, at time.Time, pluginContract int) (bool, error)
}

// ControllerSecretRequest is `POST /legion/v1/controller/secret`'s body: the daemon API contract
// `legion controller start` held the controller's plugin to before it asked (its own
// DaemonAPIVersion, which its probe found the plugin's manifest declares). The daemon compares it
// before it mints, since the mint revokes the incumbent controller.
type ControllerSecretRequest struct {
	PluginContract int `json:"pluginContract"`
}

// ControllerSecretResponse is `POST /legion/v1/controller/secret`'s answer: the capability the
// controller's Oh My Pi registers with, and the project's design gate policy (`gates.design`),
// which `legion controller start` tells the controller so its take comment promises a design
// approval only when the gate is armed. `legion controller start` reads it; the plugin never does.
type ControllerSecretResponse struct {
	Secret     string            `json:"secret"`
	DesignGate config.DesignGate `json:"designGate"`
}

// ControllerCredentialRequest is `POST /legion/v1/controller/github-credential`'s body: the
// current controller capability, the same secret `legion controller start` holds from
// `ControllerSecretResponse`, and Default, the owner whose directory the controller's
// GH_CONFIG_DIR names once a first answer named it; empty on that first fetch.
type ControllerCredentialRequest struct {
	Secret  string `json:"secret"`
	Default string `json:"default,omitempty"`
}

// ControllerCredentialResponse is `POST /legion/v1/controller/github-credential`'s answer: the
// review App's read-only token for every GitHub owner of the project, each rendered as the two
// files gh reads (`ghconfig.Rendered`: Hosts for hosts.yml, Config for config.yml) and keyed by
// lowercased owner; Default, the owner whose directory the controller's GH_CONFIG_DIR names (the
// request's, or the first owner, sorted); the App name; and the soonest of the tokens' expiries.
// `legion controller start` writes them under `<state_dir>/gh/<owner>` and refreshes them every
// minute; the plugin never reads this route or that directory.
type ControllerCredentialResponse struct {
	Owners    map[string]ControllerOwnerCredential `json:"owners"`
	Default   string                               `json:"default"`
	App       string                               `json:"app"`
	ExpiresAt string                               `json:"expiresAt"`
}

// ControllerOwnerCredential is one owner's two gh files in a ControllerCredentialResponse.
type ControllerOwnerCredential struct {
	Hosts  string `json:"hosts"`
	Config string `json:"config"`
}

// ControllerRegisterResponse is `POST /legion/v1/claims/register`'s answer to a session that
// registered as the project's controller: the project's controller role token
// (claim.ControllerToken), the role claim.RoleController, a generation, and the secret its
// controller grants authenticate with. It has no tree and no issue; a claim's registration is
// claim.RegisterResponse.
type ControllerRegisterResponse struct {
	ClaimToken claim.Token `json:"claimToken"`
	Role       claim.Role  `json:"role"`
	// Generation is the generation of the capability the operator's controller registered with,
	// and, for the daemon's own controller (`controller: daemon`), the generation of the launch
	// that registered, which its agent names on `claims/ready`.
	Generation uint64 `json:"generation"`
	Secret     string `json:"secret"`
}

// controllerSecret is `legion controller start`'s one daemon call: the operator's bearer, compared
// in constant time, buys a fresh controller capability. The mint replaces the previous capability
// and its registration, and ends every controller grant, so the controller it replaces stops being
// able to act the moment this answers — last start wins. A request whose plugin contract is not
// this daemon's is refused before the mint, naming both: the controller it would start is refused
// at registration, so minting for it would only cut the running controller off (a `legion` binary
// replaced before its daemon restarted, or the other way round). A daemon that launches its own
// controller (`controller: daemon`) refuses every request, since a mint would cut that controller
// off for a second one. Every refusal is one log line and mints nothing.
func (s *server) controllerSecret(w http.ResponseWriter, r *http.Request) {
	if !s.operatorAuthorized(r) {
		s.log.Warn("api: refused a controller secret: no operator bearer, or the wrong one")
		writeJSON(w, http.StatusForbidden, errorBody(invalidOperatorToken))
		return
	}
	if s.controllerLaunched {
		s.log.Warn("api: refused a controller secret: this daemon launches the project's controller itself")
		writeJSON(w, http.StatusConflict, errorBody(
			"this daemon launches the project's controller itself (controller: daemon), so legion controller start has none to start"))
		return
	}
	var req ControllerSecretRequest
	if !s.readBody(w, r, &req, nil) {
		return
	}
	if req.PluginContract != DaemonAPIVersion {
		s.log.Warn("api: refused a controller secret: legion controller start holds the plugin to another daemon API contract",
			"pluginContract", req.PluginContract, "daemonApiVersion", DaemonAPIVersion)
		named := "names no daemon API contract"
		if req.PluginContract != 0 {
			named = fmt.Sprintf("holds the controller's pi-legion to daemon API contract %d", req.PluginContract)
		}
		writeJSON(w, http.StatusConflict, errorBody(fmt.Sprintf(
			"legion controller start %s; this daemon requires %d: run the legion built with this daemon",
			named, DaemonAPIVersion)))
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
	writeJSON(w, http.StatusOK, ControllerSecretResponse{Secret: secret, DesignGate: s.designGate})
}

// controllerGitHubCredential is `legion controller start`'s refresh call: proving the current
// controller capability buys the review App's fresh read-only tokens for every owner of the
// project (appauth.GitHubCredential), each rendered as the two files `gh` reads. The capability
// compare runs under controllerMu, the same lock the mint and the controller-session grant take,
// so a capability a concurrent mint just replaced is never matched; the token mints run after the
// lock is released, since GitHub is the slow step and holding the lock across it would block every
// other controller-record read or mint for as long as GitHub takes to answer. A caller whose
// capability a later `legion controller start` has since replaced is refused with a code the CLI
// can tell apart from a missing or malformed secret, so its refresh loop stops rather than
// retrying forever; a daemon that launches its own controller (`controller: daemon`) refuses every
// call, as controllerSecret does, since its controller's tokens live in its pod's Secret and no
// `legion controller start` runs against it.
func (s *server) controllerGitHubCredential(w http.ResponseWriter, r *http.Request) {
	if s.controllerLaunched {
		s.log.Warn("api: refused a controller GitHub credential: this daemon launches the project's controller itself")
		writeFailure(w, http.StatusConflict, "CONTROLLER_LAUNCHED",
			"this daemon launches the project's controller itself (controller: daemon), so legion controller start has none to start")
		return
	}
	var req ControllerCredentialRequest
	if !s.readBody(w, r, &req, nil) {
		return
	}
	if !requireFailureFields(w, field{"secret", req.Secret}) {
		return
	}
	matched, err := s.controllerCapabilityCurrent(r, req.Secret)
	if err != nil {
		s.log.Error("api: read the controller record for a GitHub credential", "error", err)
		writeFailure(w, http.StatusInternalServerError, "CONTROLLER_RECORD_UNREADABLE", "could not read the controller record")
		return
	}
	if !matched {
		s.log.Warn("api: refused a controller GitHub credential: INVALID_CONTROLLER_CAPABILITY")
		writeFailure(w, http.StatusForbidden, "INVALID_CONTROLLER_CAPABILITY",
			"the controller capability is not the current one: a later legion controller start replaced it")
		return
	}
	if req.Default != "" && !ghconfig.IsOwner(strings.ToLower(req.Default)) {
		writeFailure(w, http.StatusBadRequest, "INVALID_DEFAULT_OWNER", "default is not a GitHub login")
		return
	}
	if s.tokens == nil {
		writeFailure(w, http.StatusInternalServerError, "GITHUB_TOKEN_SOURCE_UNAVAILABLE", "GitHub token source is unavailable")
		return
	}
	bundle, err := appauth.GitHubCredential(s.tokens, s.githubOwners)(r.Context(), claim.RoleController, req.Default)
	switch {
	case errors.Is(err, runtime.ErrNoGitHubOwner):
		writeFailure(w, http.StatusInternalServerError, "GITHUB_OWNER_UNCONFIGURED", "no repository owner is configured for GitHub App tokens")
		return
	case errors.Is(err, appauth.ErrOwnerOutsideProject):
		s.log.Warn("api: refused a controller GitHub credential: DEFAULT_OWNER_OUTSIDE_PROJECT", "default", strings.ToLower(req.Default))
		writeFailure(w, http.StatusBadRequest, "DEFAULT_OWNER_OUTSIDE_PROJECT", err.Error())
		return
	case err != nil:
		s.log.Error("api: mint the controller's GitHub App tokens", "error", err)
		writeFailure(w, http.StatusBadGateway, "GITHUB_TOKEN_FAILED", "GitHub token exchange failed")
		return
	}
	answer := ControllerCredentialResponse{
		Owners:  make(map[string]ControllerOwnerCredential, len(bundle.Owners)),
		Default: bundle.Default,
		App:     bundle.Owners[bundle.Default].App,
	}
	var soonest time.Time
	for owner, rendered := range bundle.Owners {
		answer.Owners[owner] = ControllerOwnerCredential{Hosts: rendered.Hosts, Config: rendered.Config}
		if soonest.IsZero() || rendered.ExpiresAt.Before(soonest) {
			soonest = rendered.ExpiresAt
		}
	}
	answer.ExpiresAt = soonest.UTC().Format(timeFormat)
	s.log.Info("api: controller github credential served", "app", answer.App, "expiresAt", soonest, "owners", bundle.OwnerNames())
	writeJSON(w, http.StatusOK, answer)
}

// controllerCapabilityCurrent compares secret against the project's current controller
// capability under controllerMu, so the compare never races the mint that would replace it.
func (s *server) controllerCapabilityCurrent(r *http.Request, secret string) (bool, error) {
	s.controllerMu.Lock()
	defer s.controllerMu.Unlock()
	record, found, err := s.controller.Controller(r.Context(), s.project)
	if err != nil {
		return false, err
	}
	return found && subtle.ConstantTimeCompare(record.CapabilityHash, capabilityHash(secret)) == 1, nil
}

// registerController is a registration whose token is no launch's boot token, on a daemon that
// leaves the controller to its operator (`controller: operator`; register routes none here
// otherwise): it registers req's session as the project's controller when the token is the current
// controller capability, and answers the one refusal an unknown token gets otherwise. No boot gate
// checks the operator's Oh My Pi — the daemon gates only the panes it launches — so a controller
// whose plugin speaks another daemon API contract is refused here, naming both, and nothing is
// recorded. The registration is issued a secret of its own, persisted by its hash before this
// answers, which the session's controller grants authenticate with.
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
	if req.PluginContract != DaemonAPIVersion {
		s.log.Warn("api: refused a controller registration: its plugin speaks another daemon API contract",
			"session", req.SessionID, "pluginContract", req.PluginContract, "daemonApiVersion", DaemonAPIVersion)
		writeJSON(w, http.StatusConflict, errorBody(fmt.Sprintf(
			"pi-legion speaks daemon API contract %d; this daemon requires %d", req.PluginContract, DaemonAPIVersion)))
		return
	}
	secret := rand.Text()
	registered, err := s.controller.RegisterController(ctx, s.project, record.Generation, req.SessionID, capabilityHash(secret), time.Now().UTC(), req.PluginContract)
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
		Role:       claim.RoleController,
		Generation: record.Generation,
		Secret:     secret,
	})
}

// registerLaunchedController is the registration of a launch of the daemon's own controller, on a
// daemon under `controller: daemon` (register refuses one otherwise): its agent registers with the
// launch's boot token, which the daemon minted for that launch and wrote into the pod's Secret,
// where the operator's controller presents the capability its bearer bought. The claim's machine
// takes the registration first, behind the generation and same-agent fences every claim's has, and
// persists the session, its transcript and the issued secret's hash; then the session is recorded
// as the project's controller — the record admission's wakes, the controller grant route and the
// state read — under controller.UnheldCapability, which revokes every earlier controller's grants
// and keeps the boot token out of the record, whose invariant that function's doc states. The
// answer is the operator's controller's registration, its generation the launch's, which the
// agent's ready names.
func (s *server) registerLaunchedController(w http.ResponseWriter, r *http.Request, req claim.RegisterRequest, launch BootToken, m *supervise.Machine) {
	ctx, decided := s.decision(r, launch.Claim)
	defer decided()
	secret := rand.Text()
	s.controllerMu.Lock()
	defer s.controllerMu.Unlock()
	if err := m.Handle(ctx, supervise.RequestRegister{
		Claim: launch.Claim, Generation: launch.Generation, Session: req.SessionID, SessionFile: req.OmpSessionFile,
		CapabilityHash: capabilityHash(secret), PluginContract: req.PluginContract,
	}); err != nil {
		s.claimFailure(w, "register", launch.Claim, err)
		return
	}
	generation, err := s.controller.MintController(ctx, s.project, controller.UnheldCapability())
	if err != nil {
		s.logFailure("api: record the launched controller's capability", "claim", launch.Claim, "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("register failed: the daemon could not record its controller"))
		return
	}
	s.grants.RevokeControllers()
	registered, err := s.controller.RegisterController(ctx, s.project, generation, req.SessionID, capabilityHash(secret), time.Now().UTC(), req.PluginContract)
	if err != nil || !registered {
		s.logFailure("api: record the launched controller's registration", "claim", launch.Claim, "registered", registered, "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("register failed: the daemon could not record its controller"))
		return
	}
	s.log.Info("api: launched controller registered", "claim", launch.Claim, "generation", launch.Generation,
		"session", req.SessionID, "agent", req.AgentID, "pluginContract", req.PluginContract)
	writeJSON(w, http.StatusOK, ControllerRegisterResponse{
		ClaimToken: launch.Claim,
		Role:       claim.RoleController,
		Generation: launch.Generation,
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
	if !found || !controllerSessionRegistered(record, req.SessionID, req.Secret) {
		writeFailure(w, http.StatusForbidden, "INVALID_SESSION_SECRET", claim.InvalidSecret.Message)
		return
	}
	if s.refuseIfStaleController(w, r, record) {
		return
	}
	s.mintControllerGrant(w)
}

// controllerSessionRegistered is the one authentication of a controller session against its
// record: the current capability's registration, naming this session, whose secret hashes to the
// recorded one. controllerSessionGrant and refuseIfStaleControllerSession both authenticate by it.
func controllerSessionRegistered(record controller.Record, session, secret string) bool {
	return record.Registered() && record.Session == session &&
		subtle.ConstantTimeCompare(record.SecretHash, capabilityHash(secret)) == 1
}

// refuseIfStaleController is the stale-caller refusal for the operator's controller record
// (LEGION-164). When the deployment launches its own controller (`controller: daemon`), the caller named
// by this record is a claim: its machine (claim.ControllerToken) carries the contract its own
// registration recorded, so it is refused by the same rule and sentence (claim.StalePluginContract,
// staleClaim) any other claim is — no machine recorded means nothing is stale. Otherwise (the
// operator's own `legion controller start`) the daemon never relaunches the operator's controller,
// so there is no relaunch to wait for: it is stale, with claim.StaleControllerContract's sentence,
// whenever its recorded contract differs at all.
func (s *server) refuseIfStaleController(w http.ResponseWriter, r *http.Request, record controller.Record) bool {
	if !s.controllerLaunched {
		if record.PluginContract == DaemonAPIVersion {
			return false
		}
		s.logStaleRefusal(r, "session", record.Session, record.PluginContract)
		refusal := claim.StaleControllerContract(record.PluginContract, DaemonAPIVersion)
		writeFailure(w, http.StatusConflict, "STALE_PLUGIN_CONTRACT", refusal.Message)
		return true
	}
	m, found := s.supervisor.Machine(claim.ControllerToken(s.project))
	if !found {
		return false
	}
	c := m.Claim()
	got, stale := s.staleClaim(c)
	if !stale {
		return false
	}
	s.logStaleRefusal(r, "session", record.Session, got)
	refusal := claim.StalePluginContract(got, DaemonAPIVersion)
	writeFailure(w, http.StatusConflict, "STALE_PLUGIN_CONTRACT", refusal.Message)
	return true
}

// refuseIfStaleControllerSession is readBody's identity check for the grants route's controller
// form ({sessionId, secret}): the session registered with the current controller capability,
// authenticated exactly as controllerSessionGrant does.
func (s *server) refuseIfStaleControllerSession(w http.ResponseWriter, r *http.Request, sessionID, secret string) bool {
	s.controllerMu.Lock()
	record, found, err := s.controller.Controller(r.Context(), s.project)
	s.controllerMu.Unlock()
	if err != nil || !found || !controllerSessionRegistered(record, sessionID, secret) {
		return false
	}
	return s.refuseIfStaleController(w, r, record)
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
