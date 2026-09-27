// packages/envoy/internal/broker/api/server.go
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

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

type ctxKey int

const (
	ctxEnrollment ctxKey = iota
	ctxLauncher
	ctxHuman
)

func Register(mux *http.ServeMux, deps Deps) {
	limits := DefaultLauncherLimits
	if deps.LauncherLimits != nil {
		limits = *deps.LauncherLimits
	}
	s := &server{deps: deps, launcherLimiter: newLauncherLimiter(limits)}
	for _, r := range routes() {
		r := r
		mux.HandleFunc(r.Method+" "+r.Pattern, func(w http.ResponseWriter, req *http.Request) {
			ctx, ok := s.authenticate(w, req, r.Auth)
			if !ok {
				return
			}
			r.Handler(s, w, req.WithContext(ctx))
		})
	}
}

func (s *server) authenticate(w http.ResponseWriter, r *http.Request, auth routeAuth) (context.Context, bool) {
	ctx := r.Context()
	switch auth {
	case authNone:
		return ctx, true
	case authLauncher:
		cred, err := s.deps.Enroll.AuthenticateLauncher(ctx, bearer(r))
		if err != nil {
			writeError(w, http.StatusUnauthorized, "LAUNCHER_INVALID", "the launcher credential is not valid")
			return nil, false
		}
		return context.WithValue(ctx, ctxLauncher, cred), true
	case authProof:
		id, ok := s.proof(w, r)
		if !ok {
			return nil, false
		}
		return context.WithValue(ctx, ctxEnrollment, id), true
	case authHumanOrProof:
		if r.Header.Get("Proof") != "" {
			id, ok := s.proof(w, r)
			if !ok {
				return nil, false
			}
			return context.WithValue(ctx, ctxEnrollment, id), true
		}
		who, err := s.deps.Dispatch.Whoami(ctx, bearer(r))
		if err != nil {
			writeError(w, http.StatusUnauthorized, "HUMAN_INVALID", "the Dispatch bearer did not identify a human")
			return nil, false
		}
		login := who.Login
		if who.Kind == "agent" && who.Owner != nil {
			login = *who.Owner
		}
		login = dispatch.CanonicalLogin(login)
		if login == "" {
			writeError(w, http.StatusForbidden, "HUMAN_REQUIRED", "this route needs a human identity")
			return nil, false
		}
		return context.WithValue(ctx, ctxHuman, login), true
	}
	return nil, false
}

func (s *server) proof(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, err := s.deps.Proof.Verify(r.Context(), r.Header.Get("Proof"), r.Method, s.deps.PublicURL+r.URL.Path, time.Now())
	if errors.Is(err, proof.ErrInvalid) {
		writeError(w, http.StatusUnauthorized, "PROOF_INVALID", err.Error())
		return "", false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "proof lookup failed")
		return "", false
	}
	return id, true
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

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"code": code, "error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Enroll.Store.Pool.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "DATABASE", "postgres unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
