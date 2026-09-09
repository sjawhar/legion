// Package api serves Dispatch's native workspace HTTP API.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

var (
	projectKeyPattern  = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)
	issueKeyPattern    = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}-[0-9]+$`)
	externalRefPattern = regexp.MustCompile(`^([^/\s]+)/([^/\s#]+)#([1-9][0-9]*)$`)
)

// Deps are the API's application dependencies.
type Deps struct {
	Store        *store.Store
	Identity     identity.Identity
	AgentToken   string
	RepoProjects map[string]string
	ServerURL    string
	Docs         docs.API
	Events       *events.Broker
}

// DepsInput contains raw boot values used to construct API dependencies.
type DepsInput struct {
	Store           *store.Store
	Identity        identity.Identity
	AgentToken      string
	RepoProjectsRaw string
	ServerURL       string
	Docs            docs.API
	Events          *events.Broker
}

// NewDeps parses boot configuration once and returns API dependencies.
func NewDeps(input DepsInput) (Deps, error) {
	repoProjects, err := ParseRepoProjects(input.RepoProjectsRaw)
	if err != nil {
		return Deps{}, err
	}
	if input.Events == nil {
		input.Events = events.NewBroker()
	}
	if input.Docs == nil {
		input.Docs = docs.New(docs.Deps{
			Store:      input.Store,
			Events:     input.Events,
			Identity:   input.Identity,
			AgentToken: input.AgentToken,
		})
	}
	return Deps{
		Store:        input.Store,
		Identity:     input.Identity,
		AgentToken:   input.AgentToken,
		RepoProjects: repoProjects,
		ServerURL:    strings.TrimSuffix(input.ServerURL, "/"),
		Docs:         input.Docs,
		Events:       input.Events,
	}, nil
}

// ParseRepoProjects parses DISPATCH_REPO_PROJECTS (owner/repo=PROJECT,...).
func ParseRepoProjects(raw string) (map[string]string, error) {
	projects := make(map[string]string)
	if strings.TrimSpace(raw) == "" {
		return projects, nil
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		repo, project, ok := strings.Cut(part, "=")
		if !ok || repo == "" || project == "" || strings.Contains(project, "=") {
			return nil, fmt.Errorf("invalid DISPATCH_REPO_PROJECTS entry %q (expected owner/repo=KEY)", part)
		}
		if !externalRefPattern.MatchString(repo+"#1") || !projectKeyPattern.MatchString(project) {
			return nil, fmt.Errorf("invalid DISPATCH_REPO_PROJECTS entry %q (expected owner/repo=KEY)", part)
		}
		if _, exists := projects[repo]; exists {
			return nil, fmt.Errorf("duplicate DISPATCH_REPO_PROJECTS repository %q", repo)
		}
		projects[repo] = project
	}
	return projects, nil
}

type server struct {
	deps Deps
}

// Register mounts every native-workspace route on mux.
func Register(mux *http.ServeMux, deps Deps) {
	s := &server{deps: deps}
	mux.HandleFunc("GET /api/v1/projects", s.listProjects)
	mux.HandleFunc("POST /api/v1/projects", s.createProject)
	mux.HandleFunc("GET /api/v1/issues", s.listIssues)
	mux.HandleFunc("POST /api/v1/issues", s.createIssue)
	mux.HandleFunc("GET /api/v1/issues/resolve", s.resolveIssue)
	mux.HandleFunc("GET /api/v1/issues/{key}", s.getIssue)
	mux.HandleFunc("PATCH /api/v1/issues/{key}", s.patchIssue)
	mux.HandleFunc("GET /api/v1/issues/{key}/events", s.listIssueEvents)
	mux.HandleFunc("GET /api/v1/issues/{key}/artifacts", s.listArtifacts)
	mux.HandleFunc("POST /api/v1/issues/{key}/artifacts", s.uploadArtifact)
	mux.HandleFunc("POST /api/v1/issues/{key}/messages", s.createMessage)
	mux.HandleFunc("GET /api/v1/inbox", s.listInbox)
	mux.HandleFunc("POST /api/v1/issues/{key}/asks", s.createAsk)
	mux.HandleFunc("GET /api/v1/asks/{id}", s.getAsk)
	mux.HandleFunc("POST /api/v1/asks/{id}/answer", s.answerAsk)
	mux.HandleFunc("GET /api/v1/issues/{key}/comments", s.listComments)
	mux.HandleFunc("POST /api/v1/issues/{key}/comments", s.createComment)
	mux.HandleFunc("POST /api/v1/comments/{id}/resolve", s.resolveComment)
	mux.HandleFunc("POST /api/v1/comments/{id}/accept", s.acceptComment)
	mux.HandleFunc("POST /api/v1/comments/{id}/reject", s.rejectComment)
	mux.HandleFunc("GET /api/v1/artifacts/{id}", s.getArtifact)
	mux.HandleFunc("GET /api/v1/artifacts/{id}/text", s.getArtifactText)
	mux.HandleFunc("GET /api/v1/artifacts/{id}/versions/{number}", s.getArtifactVersion)
	mux.HandleFunc("POST /api/v1/artifacts/{id}/versions", s.createNamedVersion)
	mux.HandleFunc("POST /api/v1/artifacts/{id}/edits", s.editArtifact)
	mux.HandleFunc("POST /api/v1/artifacts/{id}/primary", s.setPrimaryArtifact)
	mux.HandleFunc("GET /api/v1/me/state", s.getUserState)
	mux.HandleFunc("PUT /api/v1/me/issues/{key}/state", s.putUserState)
	mux.HandleFunc("GET /api/v1/events", s.streamEvents)
	if websocket, ok := deps.Docs.(interface {
		ServeHTTP(http.ResponseWriter, *http.Request)
	}); ok {
		mux.Handle("GET /ws/doc/{room}", http.HandlerFunc(websocket.ServeHTTP))
	}
}

type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func errorf(status int, code, format string, args ...any) *apiError {
	return &apiError{status: status, code: code, message: fmt.Sprintf(format, args...)}
}

func (s *server) writeHandlerError(w http.ResponseWriter, err error) {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		writeError(w, apiErr.code, apiErr.status, apiErr.message)
		return
	}
	var ambiguous *targetAmbiguousError
	if errors.As(err, &ambiguous) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      ambiguous.Error(),
			"code":       "TARGET_AMBIGUOUS",
			"candidates": ambiguous.candidates,
		})
		return
	}
	var targetAmbiguous *text.ErrTargetAmbiguous
	if errors.As(err, &targetAmbiguous) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      targetAmbiguous.Error(),
			"code":       "TARGET_AMBIGUOUS",
			"candidates": targetAmbiguous.Candidates,
		})
		return
	}
	if errors.Is(err, text.ErrTargetNotFound) {
		writeError(w, "TARGET_NOT_FOUND", http.StatusNotFound, err.Error())
		return
	}
	if errors.Is(err, docs.ErrIssueClosed) {
		writeError(w, "ISSUE_CLOSED", http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, "NOT_FOUND", http.StatusNotFound, "not found")
		return
	}
	writeError(w, "INTERNAL", http.StatusInternalServerError, "internal server error")
	slog.Error("dispatch: API handler failed", "error", err)
}

func (s *server) actorFrom(r *http.Request, supplied *model.Actor) (model.Actor, error) {
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if authorization != "" {
		if s.deps.AgentToken == "" || authorization != "Bearer "+s.deps.AgentToken {
			return model.Actor{}, errorf(http.StatusUnauthorized, "UNAUTHORIZED", "invalid bearer token")
		}
		if supplied == nil || supplied.Kind != "session" || strings.TrimSpace(supplied.ID) == "" {
			return model.Actor{}, errorf(http.StatusBadRequest, "ACTOR_KIND", "bearer callers require actor.kind session")
		}
		return *supplied, nil
	}
	if s.deps.Identity == nil {
		return model.Actor{}, errorf(http.StatusInternalServerError, "IDENTITY_ERROR", "identity service unavailable")
	}
	login, err := s.deps.Identity.Login(r)
	if err != nil {
		return model.Actor{}, err
	}
	return model.Actor{Kind: "user", ID: login}, nil
}

func (s *server) requireActor(w http.ResponseWriter, r *http.Request, supplied *model.Actor) (model.Actor, bool) {
	actor, err := s.actorFrom(r, supplied)
	if err == nil {
		return actor, true
	}
	if errors.Is(err, identity.ErrNoIdentity) || errors.Is(err, identity.ErrLoginNotAllowed) {
		identity.WriteError(w, err)
		return model.Actor{}, false
	}
	s.writeHandlerError(w, err)
	return model.Actor{}, false
}

func (s *server) requireHuman(w http.ResponseWriter, r *http.Request) (model.Actor, bool) {
	if authorization := strings.TrimSpace(r.Header.Get("Authorization")); authorization != "" {
		if s.deps.AgentToken == "" || authorization != "Bearer "+s.deps.AgentToken {
			writeError(w, "UNAUTHORIZED", http.StatusUnauthorized, "invalid bearer token")
			return model.Actor{}, false
		}
		writeError(w, "HUMAN_ONLY", http.StatusForbidden, "only users may perform this action")
		return model.Actor{}, false
	}
	return s.requireActor(w, r, nil)
}

func capExceeded(w http.ResponseWriter, field string, length, limit int) {
	writeError(w, "CAP_EXCEEDED", http.StatusBadRequest, fmt.Sprintf("%s length %d exceeds limit %d", field, length, limit))
}

func decodeJSON(r *http.Request, value any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return errorf(http.StatusBadRequest, "INVALID_JSON", "invalid JSON body: %v", err)
	}
	if decoder.More() {
		return errorf(http.StatusBadRequest, "INVALID_JSON", "request body must contain one JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, code string, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message, "code": code})
}

func encodeJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode JSON: %w", err)
	}
	return data, nil
}

func (s *server) appendEvent(ctx context.Context, tx pgx.Tx, event model.Event) (model.Event, error) {
	return s.deps.Events.Append(ctx, tx, event)
}

func (s *server) publish(events ...model.Event) {
	for _, event := range events {
		s.deps.Events.Publish(event)
	}
}

func (s *server) begin(ctx context.Context) (pgx.Tx, error) {
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return nil, errorf(http.StatusServiceUnavailable, "DATABASE_UNAVAILABLE", "database unavailable")
	}
	tx, err := s.deps.Store.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	return tx, nil
}

func jsonActor(actor model.Actor) ([]byte, error) { return encodeJSON(actor) }

func externalRef(ref string) (repo string, number string, err error) {
	match := externalRefPattern.FindStringSubmatch(ref)
	if match == nil {
		return "", "", errorf(http.StatusBadRequest, "INVALID_ISSUE_REF", "invalid issue reference %q", ref)
	}
	return match[1] + "/" + match[2], match[3], nil
}

func externalURL(repo, number string) string {
	return "https://github.com/" + repo + "/issues/" + number
}
