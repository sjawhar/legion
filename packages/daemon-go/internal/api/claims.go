package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
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
// forgot.
func (s *server) register(w http.ResponseWriter, r *http.Request) {
	var req claim.RegisterRequest
	if !readBody(w, r, &req) || !requireFields(w,
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
		writeJSON(w, claim.InvalidBootToken.Status, claim.InvalidBootToken)
		return
	}
	m, supervised := s.supervisor.Machine(launch.Claim)
	if !supervised {
		writeJSON(w, claim.InvalidBootToken.Status, claim.InvalidBootToken)
		return
	}
	secret := rand.Text()
	// The decision is the machine's and runs to its end: a caller that hangs up mid-request does
	// not get to leave a registration half-recorded.
	err = m.Handle(context.WithoutCancel(r.Context()), supervise.RequestRegister{
		Claim:          launch.Claim,
		Generation:     launch.Generation,
		Session:        req.SessionID,
		SessionFile:    req.OmpSessionFile,
		CapabilityHash: capabilityHash(secret),
	})
	if err != nil {
		s.claimFailure(w, "register", launch.Claim, err)
		return
	}
	c := m.Claim()
	s.log.Info("api: claim registered", "claim", c.Token, "generation", launch.Generation,
		"session", req.SessionID, "agent", req.AgentID, "pluginContract", req.PluginContract)
	writeJSON(w, http.StatusOK, claim.RegisterResponse{
		ClaimToken: c.Token,
		Tree:       c.Tree,
		Issue:      c.Issue,
		Role:       c.Role,
		Generation: launch.Generation,
		Secret:     secret,
	})
}

// ready is the agent saying it can be prompted; its machine sends the pending delivery, if any,
// once the claim's connection is registered.
func (s *server) ready(w http.ResponseWriter, r *http.Request) {
	var req claim.ReadyRequest
	if !readBody(w, r, &req) || !requireFields(w,
		field{"claimToken", string(req.ClaimToken)}, field{"sessionId", req.SessionID}, field{"secret", req.Secret},
	) || !requireGeneration(w, req.Generation) {
		return
	}
	m, ok := s.authenticated(req.ClaimToken, req.Secret)
	if !ok {
		writeJSON(w, claim.InvalidSecret.Status, claim.InvalidSecret)
		return
	}
	err := m.Handle(context.WithoutCancel(r.Context()), supervise.RequestReady{
		Claim: req.ClaimToken, Generation: req.Generation, Session: req.SessionID,
	})
	if err != nil {
		s.claimFailure(w, "ready", req.ClaimToken, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// exit is the agent reporting its own end: a worker's claim is released and retires, the tree's
// root claim is suspended until its tree closes, and the reason is logged.
func (s *server) exit(w http.ResponseWriter, r *http.Request) {
	var req claim.ExitRequest
	if !readBody(w, r, &req) || !requireFields(w,
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
	err := m.Handle(context.WithoutCancel(r.Context()), supervise.RequestExit{
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
	s.log.Error("api: a claim request failed", "request", request, "claim", token, "error", err)
	writeJSON(w, http.StatusInternalServerError, errorBody(request+" failed: the daemon could not record it"))
}

// capabilityHash is the one hash a registration's secret is stored and compared by.
func capabilityHash(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// readBody decodes the request's one JSON object into into, refusing with a 400 that names the
// problem — a member the route does not read included, so a caller sending a field this API does
// not have learns which one instead of having it ignored.
func readBody(w http.ResponseWriter, r *http.Request, into any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid request body: "+err.Error()))
		return false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid request body: more than one JSON value"))
		return false
	}
	return true
}

type field struct{ name, value string }

// requireFields refuses the first field that is blank, by its wire name.
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
