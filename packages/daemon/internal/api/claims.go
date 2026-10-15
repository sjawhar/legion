package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// maxRequestBytes bounds a request body: every body this API reads is a handful of short strings.
const maxRequestBytes = 1 << 20

// Supervisor is the claims the daemon supervises, as the routes reach them. Every change to a
// claim is a request posted to its machine, which decides it; the routes decide nothing about a
// claim themselves.
type Supervisor interface {
	// Machine is the claim's machine, if the daemon supervises the claim.
	Machine(token claim.Token) (*supervise.Machine, bool)
	// Create stores c — a new claim, queued — with its role prompt and starts supervising it,
	// reporting true. A claim the daemon already supervises is returned as it is, with false:
	// what may happen to it is its machine's to decide, and its role prompt is not replaced.
	Create(ctx context.Context, c supervise.Claim, rolePrompt string) (*supervise.Machine, bool, error)
	// Claims is every claim the daemon supervises, as its store holds them.
	Claims(ctx context.Context) ([]supervise.Claim, error)
}

// register is the agent's first call: its pane's boot token, and the session it became. The token
// names the launch; the registration is that launch's machine's to accept or refuse, and an
// accepted one is issued a secret whose hash the machine has persisted before this answers — a
// store that refuses the write is a 500 with no secret, so no agent holds a secret the daemon
// forgot. A launch of the daemon's own controller registers as the project's controller too
// (registerLaunchedController), and only under `controller: daemon`. A token no launch minted may
// be the controller capability `legion controller start` fetched, which registers the project's
// controller (registerController), and only under `controller: operator`: a daemon that launches
// its own controller registers no other, so such a token is the unknown one it is.
func (s *server) register(w http.ResponseWriter, r *http.Request) {
	var req claim.RegisterRequest
	if !s.readRegisterBody(w, r, &req) || !requireFields(w,
		field{"bootToken", req.BootToken}, field{"sessionId", req.SessionID},
		field{"ompSessionFile", req.OmpSessionFile}, field{"agentId", req.AgentID},
	) {
		return
	}
	launch, known, err := s.bootTokens.Resolve(r.Context(), req.BootToken)
	if err != nil {
		s.log.Error("api: resolve a registration's boot token", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("register failed: the daemon could not resolve the boot token"))
		return
	}
	if !known {
		if s.controllerLaunched {
			writeJSON(w, claim.InvalidBootToken.Status, claim.InvalidBootToken)
			return
		}
		s.registerController(w, r, req)
		return
	}
	m, supervised := s.supervisor.Machine(launch.Claim)
	if !supervised {
		writeJSON(w, claim.InvalidBootToken.Status, claim.InvalidBootToken)
		return
	}
	if m.Claim().Role == claim.RoleController {
		if !s.controllerLaunched {
			s.log.Warn("api: refused a launched controller's registration: this daemon leaves the controller to its operator",
				"claim", launch.Claim, "generation", launch.Generation, "session", req.SessionID)
			writeJSON(w, http.StatusConflict, errorBody(fmt.Sprintf(
				"%s is a launch of the daemon's own controller, and this daemon leaves the controller to its operator (controller: operator)", launch.Claim)))
			return
		}
		s.registerLaunchedController(w, r, req, launch, m)
		return
	}
	secret := rand.Text()
	// The decision is the machine's and runs to its end, or to the daemon's stop: a caller that hangs
	// up mid-request does not get to leave a registration half-recorded.
	ctx, decided := s.decision(r, launch.Claim)
	defer decided()
	err = m.Handle(ctx, supervise.RequestRegister{
		Claim:          launch.Claim,
		Generation:     launch.Generation,
		Session:        req.SessionID,
		SessionFile:    req.OmpSessionFile,
		CapabilityHash: capabilityHash(secret),
		PluginContract: req.PluginContract,
	})
	if err != nil {
		s.claimFailure(w, "register", launch.Claim, err)
		return
	}
	c := m.Claim()
	s.log.Info("api: claim registered", "claim", c.Token, "generation", launch.Generation,
		"session", req.SessionID, "agent", req.AgentID, "pluginContract", req.PluginContract)
	writeJSON(w, http.StatusOK, claim.RegisterResponse{
		ClaimToken:   c.Token,
		Tree:         c.Tree,
		Issue:        c.Issue,
		Role:         c.Role,
		Generation:   launch.Generation,
		Secret:       secret,
		PromptAgents: s.promptAgentsList(),
	})
}

// promptAgentsList is Options.PromptAgents as the registration answers it: never nil, so a role
// whose prompts dispatch no task agent reads `[]` rather than `null`.
func (s *server) promptAgentsList() []string {
	if len(s.promptAgents) == 0 {
		return []string{}
	}
	return s.promptAgents
}

// ready is the agent saying it can be prompted; its machine sends the pending delivery, if any,
// once the claim's connection is registered. A ready that carries the session's capability report
// (LEGION-663) hands it, normalised, to Options.CapabilityReported once the claim has taken the
// ready: what the report says is never a reason to refuse the ready — the daemon runs the session
// and reports what it lacks — and a row the table has no live row for is dropped and logged by its
// name cleaned as a detail is (capabilities.Normalize).
func (s *server) ready(w http.ResponseWriter, r *http.Request) {
	var req claim.ReadyRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !requireFields(w,
		field{"claimToken", string(req.ClaimToken)}, field{"sessionId", req.SessionID}, field{"secret", req.Secret},
	) || !requireGeneration(w, req.Generation) {
		return
	}
	m, ok := s.authenticated(req.ClaimToken, req.Secret)
	if !ok {
		writeJSON(w, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	if s.refuseIfStaleClaim(w, r, m.Claim()) {
		return
	}
	ctx, decided := s.decision(r, req.ClaimToken)
	defer decided()
	err := m.Handle(ctx, supervise.RequestReady{
		Claim: req.ClaimToken, Generation: req.Generation, Session: req.SessionID,
	})
	if err != nil {
		s.claimFailure(w, "ready", req.ClaimToken, err)
		return
	}
	c := m.Claim()
	if req.Capabilities != nil {
		report := s.capabilityReport(c, req)
		if s.capabilityReported != nil {
			s.capabilityReported(ctx, c, report)
		}
	}
	if s.claimReady != nil {
		s.claimReady(c)
	}
	w.WriteHeader(http.StatusNoContent)
}

// capabilityReport is the ready's capability report as the daemon keeps it: the rows normalised
// (capabilities.Normalize), the claim's process as the reporter — or a locator naming the claim
// alone, for a claim its machine records no process for — and the generation the ready ran at.
func (s *server) capabilityReport(c supervise.Claim, req claim.ReadyRequest) capabilities.Report {
	kept, dropped := capabilities.Normalize(req.Capabilities.Rows)
	if len(dropped) > 0 {
		s.log.Warn("api: a ready's capability report named rows the table has no live row for",
			"claim", req.ClaimToken, "session", req.SessionID, "names", dropped)
	}
	locator := runtime.Locator{Claim: req.ClaimToken}
	if c.Locator != nil {
		locator = *c.Locator
	}
	return capabilities.Report{
		Claim:      req.ClaimToken,
		Generation: req.Generation,
		Locator:    locator,
		MeasuredAt: req.Capabilities.MeasuredAt,
		ReportedAt: time.Now().UTC(),
		ElapsedMs:  req.Capabilities.ElapsedMs,
		Rows:       kept,
	}
}

// exit is the agent reporting its own end: a worker's claim is released and retires, the tree's
// root claim is suspended until its tree closes, and the reason is logged.
func (s *server) exit(w http.ResponseWriter, r *http.Request) {
	var req claim.ExitRequest
	if !s.readBody(w, r, &req, s.claimSecretCheck) || !requireFields(w,
		field{"claimToken", string(req.ClaimToken)}, field{"sessionId", req.SessionID}, field{"secret", req.Secret},
		field{"reason", req.Reason},
	) || !requireGeneration(w, req.Generation) {
		return
	}
	m, ok := s.authenticated(req.ClaimToken, req.Secret)
	if !ok {
		writeJSON(w, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	if s.refuseIfStaleClaim(w, r, m.Claim()) {
		return
	}
	ctx, decided := s.decision(r, req.ClaimToken)
	defer decided()
	err := m.Handle(ctx, supervise.RequestExit{
		Claim: req.ClaimToken, Generation: req.Generation, Session: req.SessionID, Reason: req.Reason,
	})
	if err != nil {
		s.claimFailure(w, "exit", req.ClaimToken, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// authenticated is the claim's machine when secret is the one its registration was issued. A
// claim the daemon does not supervise, one no agent has registered on, and a wrong secret all
// answer the same: nothing about the claim is told to a caller that cannot prove it holds it.
func (s *server) authenticated(token claim.Token, secret string) (*supervise.Machine, bool) {
	m, ok := s.supervisor.Machine(token)
	if !ok {
		return nil, false
	}
	held := m.Claim().CapabilityHash
	if len(held) == 0 || subtle.ConstantTimeCompare(held, capabilityHash(secret)) != 1 {
		return nil, false
	}
	return m, true
}

// claimFailure answers an agent request its machine did not take: a claim refusal with its own
// status and sentence (the plugin exits on the registration's 403 or 409), a request the claim's
// state does not allow with 409 and the machine's reason, and anything else — the store refusing
// a write — with a 500 that says what failed and nothing the caller could use.
func (s *server) claimFailure(w http.ResponseWriter, request string, token claim.Token, err error) {
	var refusal claim.Refusal
	if errors.As(err, &refusal) {
		s.log.Info("api: refused a claim request", "request", request, "claim", token, "reason", err)
		writeJSON(w, refusal.Status, refusal)
		return
	}
	var refused *supervise.RefusedError
	if errors.As(err, &refused) {
		writeJSON(w, http.StatusConflict, errorBody(refused.Error()))
		return
	}
	s.logFailure("api: a claim request failed", "request", request, "claim", token, "error", err)
	writeJSON(w, http.StatusInternalServerError, errorBody(request+" failed: the daemon could not record it"))
}

// capabilityHash is the one hash a registration's secret is stored and compared by.
func capabilityHash(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// decodeJSON decodes r's body (bounded by maxRequestBytes) into into once: strict disallows a
// member the route does not read, lenient ignores it (the register route, and the lenient
// identity read of an undecodable body). It reports "more than one JSON value" for trailing
// bytes either way. The raw bytes are returned too, so a strict failure can be retried leniently
// for a route family's identity.
func decodeJSON(w http.ResponseWriter, r *http.Request, into any, strict bool) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(into); err != nil {
		return body, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return body, errors.New("more than one JSON value")
	}
	return body, nil
}

// refuseBadBody is the 400 every undecodable body gets that names no stale caller: the decoder's
// error, never the body itself (it may carry a secret), logged with the route.
func (s *server) refuseBadBody(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Warn("api: refused an undecodable request body", "route", r.URL.Path, "error", err)
	writeJSON(w, http.StatusBadRequest, errorBody("invalid request body: "+err.Error()))
}

// identityCheck is readBody's lenient fallback for a route family that has a caller to name: it
// reads body's identity fields leniently (unknown fields ignored) and, when they authenticate a
// caller whose recorded contract differs from this daemon's, writes the stale-plugin refusal and
// reports true. A route with no stale identity (register, the operator routes, the
// controller secret and GitHub-credential routes) passes no check: any undecodable body there
// gets refuseBadBody.
type identityCheck func(w http.ResponseWriter, r *http.Request, body []byte) bool

// readBody decodes the request's one JSON object strictly, refusing a member the route does not
// read. On a decode failure it tries check's lenient identity read before refusing: a caller it
// can still name whose claim or controller record speaks another contract is refused with the
// stale-plugin 409 instead of the 400 every other undecodable body gets.
func (s *server) readBody(w http.ResponseWriter, r *http.Request, into any, check identityCheck) bool {
	body, err := decodeJSON(w, r, into, true)
	if err == nil {
		return true
	}
	if check != nil && check(w, r, body) {
		return false
	}
	s.refuseBadBody(w, r, err)
	return false
}

// readRegisterBody decodes the registration body leniently: it is the one body that carries
// the plugin contract the record-and-relaunch path needs, so a bump that changes another field of
// this body still reaches that path rather than refusing the process a 400 it cannot act on.
func (s *server) readRegisterBody(w http.ResponseWriter, r *http.Request, into *claim.RegisterRequest) bool {
	_, err := decodeJSON(w, r, into, false)
	if err != nil {
		s.refuseBadBody(w, r, err)
		return false
	}
	return true
}

// staleClaim reports c's recorded contract and whether the claim is a stale process the daemon is
// relaunching. A claim holds a capability hash only in a live state (registered, ready, working,
// idle — supervise.HoldsCapability): the relaunch that follows a stale registration or a
// boot-time refreshPlugin moves the claim out of those states in the same decision that discovers
// it, and Machine.persist clears the hash the moment it does, so a caller that can still
// authenticate here is necessarily in that window — after the daemon has found it stale (at
// boot, or at the registration that recorded the mismatch) and before its relaunch has taken
// effect. A relaunch the launch path refuses (a tree cleanup reserved, a store error) leaves the
// claim live and stale in that window longer, and the orphan sweep re-posts its relaunch every
// interval until one starts (refreshStalePlugins in internal/daemon), so the sentence this
// refusal carries stays true for as long as the caller can authenticate. A daemon that can
// relaunch nothing (PluginRelaunchable false, LEGION-164's degraded Kubernetes mode) never
// starts that relaunch, so a stale claim there is left running and answered as any other:
// refusing it would stall agents that work today.
func (s *server) staleClaim(c supervise.Claim) (int, bool) {
	return c.PluginContract, s.pluginRelaunchable && c.PluginContract != DaemonAPIVersion
}

// logStaleRefusal is the one log line every stale-caller refusal writes: never the body,
// never a secret.
func (s *server) logStaleRefusal(r *http.Request, identityKey, identityValue string, got int) {
	s.log.Warn("api: refused a call from a process on another plugin contract",
		"route", r.URL.Path, identityKey, identityValue, "pluginContract", got, "daemonApiVersion", DaemonAPIVersion)
}

// refuseIfStaleClaim is the claim-route refusal shape (ready, exit): errorBody naming both
// contracts, by way of claim.Refusal, which a claim route already answers with on any other
// refusal.
func (s *server) refuseIfStaleClaim(w http.ResponseWriter, r *http.Request, c supervise.Claim) bool {
	got, stale := s.staleClaim(c)
	if !stale {
		return false
	}
	s.logStaleRefusal(r, "claim", string(c.Token), got)
	refusal := claim.StalePluginContract(got, DaemonAPIVersion)
	writeJSON(w, refusal.Status, refusal)
	return true
}

// refuseIfStaleClaimFailure is the credential- and workflow-route refusal shape (the grants
// route's claim form, every grant-bearing route): Failure{Code: "STALE_PLUGIN_CONTRACT"}.
func (s *server) refuseIfStaleClaimFailure(w http.ResponseWriter, r *http.Request, c supervise.Claim) bool {
	got, stale := s.staleClaim(c)
	if !stale {
		return false
	}
	s.logStaleRefusal(r, "claim", string(c.Token), got)
	writeFailure(w, http.StatusConflict, "STALE_PLUGIN_CONTRACT", claim.StalePluginContract(got, DaemonAPIVersion).Message)
	return true
}

// claimSecretCheck is readBody's identity check for ready and exit: {claimToken, secret},
// authenticated exactly as authenticated does.
func (s *server) claimSecretCheck(w http.ResponseWriter, r *http.Request, body []byte) bool {
	var id struct {
		ClaimToken claim.Token `json:"claimToken"`
		Secret     string      `json:"secret"`
	}
	if json.Unmarshal(body, &id) != nil || id.ClaimToken == "" || id.Secret == "" {
		return false
	}
	m, ok := s.authenticated(id.ClaimToken, id.Secret)
	if !ok {
		return false
	}
	return s.refuseIfStaleClaim(w, r, m.Claim())
}

type field struct{ name, value string }

// requireFields refuses the first field that is blank, by its wire name, with the sentence alone:
// the claim and operator routes' refusal shape (the credential and workflow routes add a code,
// requireFailureFields).
func requireFields(w http.ResponseWriter, fields ...field) bool {
	for _, f := range fields {
		if strings.TrimSpace(f.value) == "" {
			writeJSON(w, http.StatusBadRequest, errorBody(f.name+" is required"))
			return false
		}
	}
	return true
}

// requireGeneration refuses a request with no generation: a launch is generation 1 or later.
func requireGeneration(w http.ResponseWriter, generation uint64) bool {
	if generation == 0 {
		writeJSON(w, http.StatusBadRequest, errorBody("generation is required"))
		return false
	}
	return true
}

func errorBody(message string) map[string]string { return map[string]string{"error": message} }
