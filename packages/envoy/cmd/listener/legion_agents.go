package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
)

// legionAgentRegistry is the Legion agent registry the /v1/legion/agents routes serve: its store
// once the legion_agents bucket opened, or the reason it did not. A listener that cannot open the
// bucket, its NATS grant lacking it being the usual cause, serves every other route and answers
// these 503 naming that reason, since the registry is one Dispatch only displays (Dispatch answers
// `legion_registry: "unavailable"` without it). While it has no store it opens the bucket again in
// the background (retry), after a backoff and at once after every NATS connection event, so a
// JetStream timeout, a slow first create, or a grant applied by a configuration reload with no
// reconnect each serves once the open succeeds.
type legionAgentRegistry struct {
	logger *logging.Logger
	// openBucket opens the bucket on a connection: legionagents.Open with the listener's options.
	openBucket func(*nats.Conn) (*legionagents.Store, error)
	// conn is the connection the next open uses, the latest a connection event brought.
	conn  atomic.Pointer[nats.Conn]
	state atomic.Pointer[legionAgentState]
	// failing is what the last open failed at, "open" or "move", or empty when it has not failed, so
	// each kind of failure logs once rather than at each retry. Only the open in progress reads or
	// writes it, and there is never more than one.
	failing string
	// reconnected wakes the retry at once after a connection event.
	reconnected chan struct{}
	// firstWait and maxWait bound the retry's backoff, which doubles from one to the other.
	firstWait, maxWait time.Duration
	// stop ends the retry, and retried is closed once it has returned.
	stop    context.CancelFunc
	retried chan struct{}
}

// legionAgentState is the store, or, while it is nil, why the routes answer 503.
type legionAgentState struct {
	store       *legionagents.Store
	unavailable string
}

// The backoff between the retries of an open that failed: 1 s, doubling to 1 min.
const (
	legionAgentRetryFirstWait = time.Second
	legionAgentRetryMaxWait   = time.Minute
)

// newLegionAgentRegistry serves store, a registry whose bucket is open.
func newLegionAgentRegistry(store *legionagents.Store) *legionAgentRegistry {
	r := &legionAgentRegistry{}
	r.state.Store(&legionAgentState{store: store})
	return r
}

// openLegionAgentRegistry opens the registry's bucket on conn. A bucket it cannot open logs one
// ERROR and leaves the registry unavailable rather than ending the start, and the open is retried
// in the background until it succeeds or close ends it.
func openLegionAgentRegistry(conn *nats.Conn, logger *logging.Logger, opts ...legionagents.Option) *legionAgentRegistry {
	return startLegionAgentRegistry(conn, logger, func(conn *nats.Conn) (*legionagents.Store, error) {
		return legionagents.Open(conn, opts...)
	}, legionAgentRetryFirstWait, legionAgentRetryMaxWait)
}

// startLegionAgentRegistry opens the bucket on conn through openBucket, and, when that fails,
// starts the retry with a backoff doubling from firstWait to maxWait.
func startLegionAgentRegistry(conn *nats.Conn, logger *logging.Logger, openBucket func(*nats.Conn) (*legionagents.Store, error), firstWait, maxWait time.Duration) *legionAgentRegistry {
	ctx, stop := context.WithCancel(context.Background())
	r := &legionAgentRegistry{
		logger:      logger,
		openBucket:  openBucket,
		reconnected: make(chan struct{}, 1),
		firstWait:   firstWait,
		maxWait:     maxWait,
		stop:        stop,
		retried:     make(chan struct{}),
	}
	r.conn.Store(conn)
	if r.open() {
		stop()
		close(r.retried)
		return r
	}
	go r.retry(ctx)
	return r
}

// open opens the bucket on the latest connection, and reports whether the registry has its store.
// A connection event that lands during the open moves the store to the new connection: the event's
// hook stores the connection before it reads the state, and open stores the state before it reads
// the connection, so one of the two sees the other's. A move open makes that fails leaves the
// registry without a store, and the retry opens the bucket again on the latest connection: that
// event's hook found no store to move and only woke the retry, so no later event is owed.
func (r *legionAgentRegistry) open() bool {
	conn := r.conn.Load()
	store, err := r.openBucket(conn)
	if err != nil {
		reason := fmt.Sprintf("envoy: the legion agent registry is unavailable: could not open the %s bucket (a missing grant is the usual cause): %v", bus.LegionAgentBucket, err)
		r.state.Store(&legionAgentState{unavailable: reason})
		if r.failing != "open" {
			r.failing = "open"
			r.logger.Error("legion agent registry unavailable: could not open the "+bus.LegionAgentBucket+" bucket (a missing grant is the usual cause), so the /v1/legion/agents routes answer 503 while every other route serves; the listener retries the open in the background",
				slog.String("bucket", bus.LegionAgentBucket), slog.String("error", err.Error()))
		}
		return false
	}
	r.state.Store(&legionAgentState{store: store})
	if latest := r.conn.Load(); latest != conn {
		if err := store.Reopen(latest); err != nil {
			reason := fmt.Sprintf("envoy: the legion agent registry is unavailable: it opened the %s bucket, then could not move it to the new NATS connection: %v", bus.LegionAgentBucket, err)
			r.state.Store(&legionAgentState{unavailable: reason})
			if r.failing != "move" {
				r.failing = "move"
				r.logger.Error("legion agent registry did not move to the new NATS connection, so the /v1/legion/agents routes answer 503 while every other route serves; the listener retries the open in the background",
					slog.String("bucket", bus.LegionAgentBucket), slog.String("error", err.Error()))
			}
			return false
		}
	}
	r.logger.Info("legion agent registry opened", slog.String("bucket", bus.LegionAgentBucket))
	return true
}

// retry opens the bucket again until it opens or ctx ends: after a backoff doubling from firstWait
// to maxWait, and at once, from firstWait again, after a connection event. It is the one open after
// the first, so no two opens run at once.
func (r *legionAgentRegistry) retry(ctx context.Context) {
	defer close(r.retried)
	wait := r.firstWait
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.reconnected:
			wait = r.firstWait
		case <-timer.C:
			wait = min(wait*2, r.maxWait)
		}
		if r.open() {
			return
		}
		timer.Reset(wait)
	}
}

// close ends the retry. main adds it as a drain hook. It does not wait for an open under way: that
// open takes no context and can run its 10 s, which the drain's hooks, run one after another, cannot
// spend; an open that ends after close only stores the registry's state.
func (r *legionAgentRegistry) close(context.Context) {
	if r.stop == nil {
		return
	}
	r.stop()
}

// reconnect is the registry's reconnect hook. A store it has moves to conn, failing the hook as any
// other store's move does; a registry without one wakes the retry to open the bucket on conn in the
// background, so an open that fails again holds up no other hook for its 10 s, and fails no hook
// run, which the bus would retry without end.
func (r *legionAgentRegistry) reconnect(conn *nats.Conn) error {
	r.conn.Store(conn)
	if store := r.state.Load().store; store != nil {
		return store.Reopen(conn)
	}
	select {
	case r.reconnected <- struct{}{}:
	default:
	}
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
