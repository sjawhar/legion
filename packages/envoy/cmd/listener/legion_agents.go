package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
)

// legionAgentRegistry is the Legion agent registry the /v1/legion/agents routes serve: its store
// once the legion_agents bucket opened, or the reason it did not. A listener whose NATS grant lacks
// the bucket serves every other route and answers these 503 naming that reason, since the registry
// is one Dispatch only displays (Dispatch answers `legion_registry: "unavailable"` without it), and
// it opens the bucket again after every NATS connection event (reconnect), because a grant applied
// to the listener's user reaches it on a new connection.
type legionAgentRegistry struct {
	logger *logging.Logger
	opts   []legionagents.Option
	// opening holds one open of the bucket at a time.
	opening sync.Mutex
	state   atomic.Pointer[legionAgentState]
}

// legionAgentState is the store, or, while it is nil, why the routes answer 503.
type legionAgentState struct {
	store       *legionagents.Store
	unavailable string
}

// newLegionAgentRegistry serves store, a registry whose bucket is open.
func newLegionAgentRegistry(store *legionagents.Store) *legionAgentRegistry {
	r := &legionAgentRegistry{}
	r.state.Store(&legionAgentState{store: store})
	return r
}

// openLegionAgentRegistry opens the registry's bucket on conn. A bucket it cannot open, the
// listener's NATS user lacking the grant on it, logs one ERROR naming the grant and leaves the
// registry unavailable rather than ending the start.
func openLegionAgentRegistry(conn *nats.Conn, logger *logging.Logger, opts ...legionagents.Option) *legionAgentRegistry {
	r := &legionAgentRegistry{logger: logger, opts: opts}
	r.state.Store(&legionAgentState{unavailable: "the legion agent registry is opening"})
	r.open(conn)
	return r
}

// open opens the bucket on conn when the registry has no store, or moves the store it has to conn.
func (r *legionAgentRegistry) open(conn *nats.Conn) {
	r.opening.Lock()
	defer r.opening.Unlock()
	if store := r.state.Load().store; store != nil {
		if err := store.Reopen(conn); err != nil {
			r.logger.Error("legion agent registry did not move to the new NATS connection", slog.String("error", err.Error()))
		}
		return
	}
	store, err := legionagents.Open(conn, r.opts...)
	if err != nil {
		reason := fmt.Sprintf("envoy: the legion agent registry is unavailable: this listener could not open the %s bucket, which its NATS user needs a grant on: %v", bus.LegionAgentBucket, err)
		r.state.Store(&legionAgentState{unavailable: reason})
		r.logger.Error("legion agent registry unavailable: this listener's NATS user lacks the grant on the "+bus.LegionAgentBucket+" bucket, so the /v1/legion/agents routes answer 503 until the grant is applied; every other route serves, and the listener opens the bucket again after its next NATS reconnect",
			slog.String("bucket", bus.LegionAgentBucket), slog.String("error", err.Error()))
		return
	}
	r.state.Store(&legionAgentState{store: store})
	r.logger.Info("legion agent registry opened", slog.String("bucket", bus.LegionAgentBucket))
}

// reconnect is the registry's reconnect hook. A store it has moves to conn, failing the hook as any
// other store's move does; a registry without one opens the bucket on conn in the background, so a
// grant still missing holds up no other hook for the open's 10 s, and fails no hook run, which the
// bus would retry without end.
func (r *legionAgentRegistry) reconnect(conn *nats.Conn) error {
	if store := r.state.Load().store; store != nil {
		return store.Reopen(conn)
	}
	go r.open(conn)
	return nil
}

// serving is the store the routes read and write, or false once it has answered the 503 naming
// why there is none.
func (r *legionAgentRegistry) serving(w http.ResponseWriter) (*legionagents.Store, bool) {
	state := r.state.Load()
	if state.store == nil {
		writeJSONError(w, http.StatusServiceUnavailable, state.unavailable)
		return nil, false
	}
	return state.store, true
}

// legionAgentBodyLimit bounds the body of a Legion agent registry write, one entry or a batch.
const legionAgentBodyLimit = 1 << 20

// legionAgentAnswer is one Legion agent registry entry as a read answers it: the entry, the session
// that replaced it within its role token (legionagents.Agent), and whether its session is in this
// listener's session registry now. Both are derived at the read and neither is stored.
type legionAgentAnswer struct {
	legionagents.Agent
	Live bool `json:"live"`
}

// legionAgentList is a listing's answer, `[]` when the registry holds no entry.
type legionAgentList struct {
	Entries []legionAgentAnswer `json:"entries"`
}

// legionAgentBatchFailure is a batch write that failed after some entries may have been written
// (legionagents.PartialWriteError): written counts those, and every write is idempotent, so the
// caller sends the batch again once the failure is one to retry.
type legionAgentBatchFailure struct {
	Error   string `json:"error"`
	Written int    `json:"written"`
}

func answerLegionAgent(sessions *session.SessionRegistry, agent legionagents.Agent) legionAgentAnswer {
	return legionAgentAnswer{Agent: agent, Live: isSessionLive(sessions, agent.SessionID)}
}

func answerLegionAgents(sessions *session.SessionRegistry, agents []legionagents.Agent) legionAgentList {
	entries := make([]legionAgentAnswer, len(agents))
	for i, agent := range agents {
		entries[i] = answerLegionAgent(sessions, agent)
	}
	return legionAgentList{Entries: entries}
}

// legionAgentReadContext bounds a whole registry read, a session key and its role token's listing
// among them, to legionagents.ReadTimeout, so a read the bucket does not answer is a 503 inside the
// listener's 10 s HTTP write timeout.
func legionAgentReadContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), legionagents.ReadTimeout)
}

// legionAgentErrorStatus is the status a registry failure answers: a session moved to another issue
// or role is a 409, an invalid name or entry a 400 naming the rule (and, in a batch, the entry's
// index), a batch past legionagents.MaxBatch a 413, a key NATS refuses as writeNATSError answers it
// (a 413 too large, a 400 otherwise), and anything else a read or write the bucket did not answer, a
// 503 to retry.
func legionAgentErrorStatus(err error) int {
	var conflict *legionagents.ConflictError
	switch {
	case errors.As(err, &conflict):
		return http.StatusConflict
	case errors.Is(err, legionagents.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, legionagents.ErrTooMany), errors.Is(err, bus.ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, bus.ErrRefused):
		return http.StatusBadRequest
	default:
		return http.StatusServiceUnavailable
	}
}

// writeLegionAgentError answers a registry call that failed with legionAgentErrorStatus's status. A
// 503 carries `Retry-After: 1`, and a batch that failed after its checks
// (legionagents.PartialWriteError) answers how many entries it wrote whole as `written`, whatever
// its status.
func writeLegionAgentError(w http.ResponseWriter, err error) {
	status := legionAgentErrorStatus(err)
	message := err.Error()
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "1")
		message = "envoy: the legion agent registry did not answer: " + message
	}
	var partial *legionagents.PartialWriteError
	if errors.As(err, &partial) {
		writeJSON(w, status, legionAgentBatchFailure{Error: message, Written: partial.Written})
		return
	}
	writeJSONError(w, status, message)
}

// decodeLegionAgentBody decodes a registry write's body into into, answering a body past
// legionAgentBodyLimit 413 and one that is not JSON 400. It reports whether the handler goes on.
func decodeLegionAgentBody(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, legionAgentBodyLimit)
	err := json.NewDecoder(r.Body).Decode(into)
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeJSONError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the body is past %d bytes", legionAgentBodyLimit))
		return false
	case err != nil:
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return false
	}
	return true
}

// legionAgentSessionHandler serves /v1/legion/agents/sessions/<session_id>. GET answers the
// session's entry, its replacement within its role token and whether it is live, or
// `404 {"error":"unknown legion agent <id>"}` when the bucket holds no entry for it. PUT records the
// one entry its body holds, whose session_id must be the path's, and answers it as stored.
func legionAgentSessionHandler(d *listenerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/legion/agents/sessions/")
		if sessionID == "" {
			writeJSONError(w, http.StatusBadRequest, "session_id is required", "session_id")
			return
		}
		switch r.Method {
		case http.MethodGet:
			registry, ok := d.legionAgents.serving(w)
			if !ok {
				return
			}
			ctx, cancel := legionAgentReadContext(r)
			defer cancel()
			agent, err := registry.Get(ctx, sessionID)
			switch {
			case errors.Is(err, legionagents.ErrNotFound):
				writeJSONError(w, http.StatusNotFound, "unknown legion agent "+sessionID)
			case err != nil:
				writeLegionAgentError(w, err)
			default:
				writeJSON(w, http.StatusOK, answerLegionAgent(d.sessions, agent))
			}
		case http.MethodPut:
			if err := legionagents.ValidateSessionID(sessionID); err != nil {
				writeLegionAgentError(w, err)
				return
			}
			var entry legionagents.Entry
			if !decodeLegionAgentBody(w, r, &entry) {
				return
			}
			if entry.SessionID != sessionID {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("session_id %q is not the path's %q", entry.SessionID, sessionID), "session_id")
				return
			}
			registry, ok := d.legionAgents.serving(w)
			if !ok {
				return
			}
			stored, err := registry.Put(r.Context(), entry)
			if err != nil {
				writeLegionAgentError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, stored)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

// legionAgentRoleHandler serves GET /v1/legion/agents/roles/<role_token>: every session of the role
// token, newest first, the first being its current holder, `[]` when none.
func legionAgentRoleHandler(d *listenerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		token := strings.TrimPrefix(r.URL.Path, "/v1/legion/agents/roles/")
		if token == "" {
			writeJSONError(w, http.StatusBadRequest, "role_token is required", "role_token")
			return
		}
		registry, ok := d.legionAgents.serving(w)
		if !ok {
			return
		}
		ctx, cancel := legionAgentReadContext(r)
		defer cancel()
		agents, err := registry.ByRoleToken(ctx, token)
		if err != nil {
			writeLegionAgentError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, answerLegionAgents(d.sessions, agents))
	}
}

// legionAgentsHandler serves /v1/legion/agents. GET ?issue=<KEY> answers every session of the
// issue, by role in workflow order, then newest first, `[]` when none. PUT records a batch,
// `{"entries":[…]}` of at most legionagents.MaxBatch entries in a body of at most
// legionAgentBodyLimit bytes, all validated and checked before the first write, so one bad entry
// refuses the batch with nothing written. A batch that fails after that answers how many entries
// were written (writeLegionAgentError).
func legionAgentsHandler(d *listenerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			issue := r.URL.Query().Get("issue")
			if issue == "" {
				writeJSONError(w, http.StatusBadRequest, "issue is required", "issue")
				return
			}
			registry, ok := d.legionAgents.serving(w)
			if !ok {
				return
			}
			ctx, cancel := legionAgentReadContext(r)
			defer cancel()
			agents, err := registry.ByIssue(ctx, issue)
			if err != nil {
				writeLegionAgentError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, answerLegionAgents(d.sessions, agents))
		case http.MethodPut:
			var body struct {
				Entries []legionagents.Entry `json:"entries"`
			}
			if !decodeLegionAgentBody(w, r, &body) {
				return
			}
			if body.Entries == nil {
				writeJSONError(w, http.StatusBadRequest, "entries is required", "entries")
				return
			}
			registry, ok := d.legionAgents.serving(w)
			if !ok {
				return
			}
			stored, err := registry.PutAll(r.Context(), body.Entries)
			if err != nil {
				writeLegionAgentError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string][]legionagents.Entry{"entries": stored})
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}
