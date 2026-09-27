// Package api is the broker's HTTP surface: routes_table.go lists every route with the
// authentication it needs, and this file authenticates callers and writes responses.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/dispatch"
	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/launcher"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

type Deps struct {
	PublicURL string
	Enroll    *enroll.Service
	Machine   *requests.Machine
	Proof     *proof.Verifier
	Dispatch  *dispatch.Client
	Launcher  *launcher.Service
	// LauncherLimits bounds POST /v1/launcher-credentials; nil means DefaultLauncherLimits.
	LauncherLimits *LauncherLimits
}

type server struct {
	deps            Deps
	launcherLimiter *launcherLimiter
}

func Register(mux *http.ServeMux, deps Deps) {
	limits := DefaultLauncherLimits
	if deps.LauncherLimits != nil {
		limits = *deps.LauncherLimits
	}
	s := &server{deps: deps, launcherLimiter: newLauncherLimiter(limits)}
	for _, route := range routes() {
		mux.HandleFunc(route.Method+" "+route.Pattern, func(w http.ResponseWriter, r *http.Request) {
			who, ok := s.authenticate(w, r, route.Handler.auth)
			if !ok {
				return
			}
			route.Handler.serve(s, w, r, who)
		})
	}
}

// authenticate proves who r comes from, as auth requires, or writes the refusal. A bad or missing
// credential is a 401; a store or Dispatch that cannot answer is a 503 naming it, never mistaken
// for a bad credential.
func (s *server) authenticate(w http.ResponseWriter, r *http.Request, auth routeAuth) (caller, bool) {
	switch auth {
	case authNone:
		return caller{}, true
	case authLauncher:
		cred, err := s.deps.Enroll.AuthenticateLauncher(r.Context(), bearer(r))
		switch {
		case errors.Is(err, enroll.ErrUnauthenticated):
			writeError(w, http.StatusUnauthorized, "LAUNCHER_INVALID", "the launcher credential is not valid")
			return caller{}, false
		case err != nil:
			writeUnavailable(w, "DATABASE_UNAVAILABLE", "authenticate launcher credential", err)
			return caller{}, false
		}
		return caller{launcher: cred}, true
	case authProof:
		id, ok := s.proof(w, r)
		return caller{enrollment: id}, ok
	case authHumanOrProof:
		if r.Header.Get("Proof") != "" {
			id, ok := s.proof(w, r)
			return caller{enrollment: id}, ok
		}
		login, ok := s.human(w, r)
		return caller{human: login}, ok
	}
	writeInternal(w, "authenticate", fmt.Errorf("route has unknown authentication %d", auth))
	return caller{}, false
}

func (s *server) proof(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, err := s.deps.Proof.Verify(r.Context(), r.Header.Get("Proof"), r.Method, s.deps.PublicURL+r.URL.Path, time.Now())
	switch {
	case errors.Is(err, proof.ErrInvalid):
		writeError(w, http.StatusUnauthorized, "PROOF_INVALID", err.Error())
		return "", false
	case err != nil:
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "verify proof", err)
		return "", false
	}
	return id, true
}

// human resolves the Dispatch bearer on r to the canonical login of the human it acts for: a
// signed-in user, or the owner of a personal agent token.
func (s *server) human(w http.ResponseWriter, r *http.Request) (string, bool) {
	token := bearer(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "HUMAN_INVALID", "this route needs a proof or a Dispatch bearer")
		return "", false
	}
	who, err := s.deps.Dispatch.Whoami(r.Context(), token)
	if dispatchErr, ok := dispatch.AsError(err); ok && (dispatchErr.Status == http.StatusUnauthorized || dispatchErr.Status == http.StatusForbidden) {
		writeError(w, http.StatusUnauthorized, "HUMAN_INVALID", "the Dispatch bearer did not identify a human")
		return "", false
	}
	if err != nil {
		writeDispatchFailure(w, "resolve the Dispatch bearer", err)
		return "", false
	}
	login := who.Login
	if who.Kind == "agent" && who.Owner != nil {
		login = *who.Owner
	}
	login = dispatch.CanonicalLogin(login)
	if login == "" {
		writeError(w, http.StatusForbidden, "HUMAN_REQUIRED", "this route needs a human identity")
		return "", false
	}
	return login, true
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// maxJSONBodyBytes caps every JSON request body the broker reads, as Dispatch's own decodeJSON does.
const maxJSONBodyBytes = 1 << 20

// readJSON decodes r's body as exactly one JSON value into v: at most maxJSONBodyBytes (413
// REQUEST_TOO_LARGE), no field v does not declare and nothing after the value (400 with
// invalidCode). It writes the refusal itself and reports whether decoding succeeded.
func readJSON(w http.ResponseWriter, r *http.Request, v any, invalidCode string) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(v)
	if err == nil && decoder.More() {
		err = errors.New("body must contain one JSON value")
	}
	var tooLarge *http.MaxBytesError
	switch {
	case err == nil:
		return true
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", fmt.Sprintf("request body exceeds %d bytes", maxJSONBodyBytes))
	default:
		writeError(w, http.StatusBadRequest, invalidCode, "body must be one valid JSON object: "+err.Error())
	}
	return false
}

// pathUUID reads the {name} path segment as a UUID, or refuses it with 400 code: an id that is not
// a UUID names nothing, and must never reach a uuid column as a Postgres type error.
func pathUUID(w http.ResponseWriter, r *http.Request, name, code, what string) (string, bool) {
	id := r.PathValue(name)
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, code, what+" ids are UUIDs")
		return "", false
	}
	return id, true
}

// issueKey is a Dispatch issue key (internal/dispatch/api's issueKeyPattern). Every key the broker
// puts into a Dispatch URL path from a request body is checked against it first.
var issueKey = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}-[0-9]+$`)

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"code": code, "error": msg})
}

// writeInternal logs a failed operation with its error and answers 500 INTERNAL naming the
// operation. Errors on these paths carry no secret values; the log line is the only place the
// cause of a 500 is recorded.
func writeInternal(w http.ResponseWriter, op string, err error) {
	slog.Error("broker: "+op+" failed", "error", err)
	writeError(w, http.StatusInternalServerError, "INTERNAL", op+" failed")
}

// writeUnavailable logs a dependency that could not answer and answers 503 with code naming it.
func writeUnavailable(w http.ResponseWriter, code, op string, err error) {
	slog.Error("broker: "+op+" failed", "error", err)
	writeError(w, http.StatusServiceUnavailable, code, op+" failed: a dependency the broker needs is unavailable")
}

// writeDispatchFailure answers a failed Dispatch call: 503 DISPATCH_UNAVAILABLE when Dispatch
// could not answer at all, 502 DISPATCH_ERROR when it answered with a refusal. Any other error is
// the broker's own and answers 500.
func writeDispatchFailure(w http.ResponseWriter, op string, err error) {
	dispatchErr, ok := dispatch.AsError(err)
	switch {
	case !ok:
		writeInternal(w, op, err)
	case dispatchErr.Unavailable():
		slog.Error("broker: "+op+" failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "DISPATCH_UNAVAILABLE", op+" failed: Dispatch could not be reached")
	default:
		slog.Error("broker: "+op+" failed", "error", err)
		writeError(w, http.StatusBadGateway, "DISPATCH_ERROR", fmt.Sprintf("%s failed: Dispatch answered %d %s", op, dispatchErr.Status, dispatchErr.Code))
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Enroll.Store.Pool.Ping(r.Context()); err != nil {
		writeUnavailable(w, "DATABASE", "ping Postgres", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
