// Package api is the broker's HTTP surface: routes_table.go lists every route with the
// authentication it needs, and this file authenticates callers and writes responses.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/broker/enroll"
	"github.com/sjawhar/envoy/internal/broker/machine"
	"github.com/sjawhar/envoy/internal/broker/proof"
	"github.com/sjawhar/envoy/internal/broker/record"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

type Deps struct {
	PublicURL string
	// UIToken is BROKER_UI_TOKEN: the shared bearer uiAuth compares against (constant-time),
	// authenticating Dispatch's server, which vouches for the approver login it sends.
	UIToken      string
	Enroll       *enroll.Service
	Machine      *requests.Machine
	MachineLogin *machine.Service
	Proof        *proof.Verifier
	// LauncherLimits bounds POST /v1/launcher-credentials; nil means DefaultLauncherLimits.
	LauncherLimits *LauncherLimits
	// TrustedProxyHeader names a request header (e.g. "X-Forwarded-For") the launcher-credential
	// rate limiter's per-address bucket trusts for the real client address; empty means keying on
	// r.RemoteAddr, correct only when the broker is reached directly rather than through a
	// reverse proxy or load balancer. See limits.go's clientAddress.
	TrustedProxyHeader string
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
	s := &server{deps: deps, launcherLimiter: newLauncherLimiter(limits, deps.TrustedProxyHeader)}
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
// credential is a 401; a store that cannot answer is a 503 naming it, never mistaken for a bad
// credential.
func (s *server) authenticate(w http.ResponseWriter, r *http.Request, auth routeAuth) (caller, bool) {
	switch auth {
	case authNone:
		return caller{}, true
	case authUI:
		token := bearer(r)
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.deps.UIToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "UI_INVALID", "the UI bearer token is not valid")
			return caller{}, false
		}
		return caller{}, true
	case authLauncher:
		subject, err := s.proofSubject(r)
		switch {
		case errors.Is(err, proof.ErrInvalid):
			writeError(w, http.StatusUnauthorized, "LAUNCHER_INVALID", "the launcher credential is not valid")
			return caller{}, false
		case err != nil:
			writeUnavailable(w, "DATABASE_UNAVAILABLE", "verify launcher proof", err)
			return caller{}, false
		}
		if subject.LauncherID == "" {
			writeError(w, http.StatusUnauthorized, "LAUNCHER_INVALID", "this route needs a launcher proof, not a session proof")
			return caller{}, false
		}
		cred, err := s.deps.Enroll.Credential(r.Context(), subject.LauncherID)
		if err != nil {
			writeUnavailable(w, "DATABASE_UNAVAILABLE", "read launcher credential", err)
			return caller{}, false
		}
		return caller{launcher: cred}, true
	case authProof:
		subject, err := s.proofSubject(r)
		switch {
		case errors.Is(err, proof.ErrInvalid):
			writeError(w, http.StatusUnauthorized, "PROOF_INVALID", err.Error())
			return caller{}, false
		case err != nil:
			writeUnavailable(w, "DATABASE_UNAVAILABLE", "verify proof", err)
			return caller{}, false
		}
		if subject.EnrollmentID == "" {
			writeError(w, http.StatusUnauthorized, "PROOF_INVALID", "this route needs a session proof, not a launcher proof")
			return caller{}, false
		}
		return caller{enrollment: subject.EnrollmentID}, true
	}
	writeInternal(w, "authenticate", fmt.Errorf("route has unknown authentication %d", auth))
	return caller{}, false
}

// proofSubject verifies r's Proof header and returns who it authenticates, unwrapped: authLauncher
// and authProof each translate a failure into their own vocabulary (LAUNCHER_INVALID vs
// PROOF_INVALID, Authentication items 1 and 2 of the shared broker contract), so this reports
// only the raw error.
func (s *server) proofSubject(r *http.Request) (proof.Subject, error) {
	return s.deps.Proof.Verify(r.Context(), r.Header.Get("Proof"), r.Method, s.deps.PublicURL+r.URL.Path, time.Now())
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

// recordIDPattern matches a credential-request record id: content-addressed lowercase-hex
// SHA-256, never a UUID (record.Body.ID).
var recordIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// pathRecordID reads the {name} path segment as a credential-request record id, or refuses it
// with 400 code: record ids are lowercase-hex sha256 hashes, so pathUUID's check would wrongly
// reject every valid one.
func pathRecordID(w http.ResponseWriter, r *http.Request, name, code string) (string, bool) {
	id := r.PathValue(name)
	if !recordIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, code, "record ids are lowercase-hex sha256 hashes")
		return "", false
	}
	return id, true
}

// requireApprover refuses an approver login that canonicalizes to nothing with 400
// APPROVER_REQUIRED: every UI route names the human it acts for, whether a decision's or a
// revoke's approver field or a list's ?approver= query.
func requireApprover(w http.ResponseWriter, login string) bool {
	if record.CanonicalLogin(login) == "" {
		writeError(w, http.StatusBadRequest, "APPROVER_REQUIRED", "approver is required")
		return false
	}
	return true
}

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

// writeJSON answers status with v, which is one of this package's named response types: the
// broker's generated HTTP reference (cmd/broker-refgen) documents each route's answer
// from that type's fields and refuses a value it cannot name.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// stateResponse is what a denial, a cancel or a revoke answers.
type stateResponse struct {
	// The state the call left the request, record or grant in: "denied", "cancelled" or "revoked".
	State string `json:"state"`
}

// healthResponse is GET /healthz's answer while the broker can reach Postgres.
type healthResponse struct {
	// "ok".
	Status string `json:"status"`
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Enroll.Store.Pool.Ping(r.Context()); err != nil {
		writeUnavailable(w, "DATABASE_UNAVAILABLE", "ping Postgres", err)
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

// strPtr is nil for "" and &s otherwise, for an optional wire field that is null rather than "".
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
