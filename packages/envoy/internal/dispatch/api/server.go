// Package api serves Dispatch's native workspace HTTP API.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

var (
	projectKeyPattern  = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)
	issueKeyPattern    = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}-[0-9]+$`)
	externalRefPattern = regexp.MustCompile(`^([^/\s]+)/([^/\s#]+)#([1-9][0-9]*)$`)
)

// repoLabelPrefix marks an issue created through DISPATCH_DEFAULT_PROJECT
// with the external repository it came from, so the repository stays
// visible and filterable once it's no longer named by the project key.
const repoLabelPrefix = "repo:"

// Deps are the API's application dependencies.
type Deps struct {
	Store            *store.Store
	Identity         identity.Identity
	AgentToken       string
	DefaultProject   string
	ServerURL        string
	Docs             docs.API
	Envoy            *envoy.Client
	Events           *events.Broker
	TestHooksEnabled bool
}

// DepsInput contains raw boot values used to construct API dependencies.
type DepsInput struct {
	Store            *store.Store
	Identity         identity.Identity
	AgentToken       string
	RepoProjectsRaw  string
	DefaultProject   string
	ServerURL        string
	EnvoyURL         string
	Docs             docs.API
	Events           *events.Broker
	TestHooksEnabled bool
}

// NewDeps parses boot configuration once and returns API dependencies.
func NewDeps(input DepsInput) (Deps, error) {
	if _, err := ParseRepoProjects(input.RepoProjectsRaw); err != nil {
		return Deps{}, err
	}
	defaultProject := strings.TrimSpace(input.DefaultProject)
	if defaultProject != "" && !projectKeyPattern.MatchString(defaultProject) {
		return Deps{}, fmt.Errorf("invalid DISPATCH_DEFAULT_PROJECT %q (expected project key)", defaultProject)
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
			ServerURL:  input.ServerURL,
		})
	}
	var envoyClient *envoy.Client
	if envoyURL := strings.TrimSpace(input.EnvoyURL); envoyURL != "" {
		envoyClient = envoy.New(envoyURL)
	}
	return Deps{
		Store:            input.Store,
		Identity:         input.Identity,
		AgentToken:       input.AgentToken,
		DefaultProject:   defaultProject,
		ServerURL:        strings.TrimSuffix(input.ServerURL, "/"),
		Docs:             input.Docs,
		Envoy:            envoyClient,
		Events:           input.Events,
		TestHooksEnabled: input.TestHooksEnabled,
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
		project = strings.TrimSpace(project)
		if !ok || repo == "" || project == "" || strings.Contains(project, "=") || !projectKeyPattern.MatchString(project) {
			return nil, fmt.Errorf("invalid DISPATCH_REPO_PROJECTS entry %q (expected owner/repo=KEY)", part)
		}
		canonical, _, err := externalRef(repo + "#1")
		if err != nil {
			return nil, fmt.Errorf("invalid DISPATCH_REPO_PROJECTS entry %q (expected owner/repo=KEY)", part)
		}
		if _, exists := projects[canonical]; exists {
			return nil, fmt.Errorf("duplicate DISPATCH_REPO_PROJECTS repository %q", canonical)
		}
		projects[canonical] = project
	}
	return projects, nil
}

type server struct {
	deps Deps
}

// queryer is the pgx surface shared by *pgxpool.Pool and pgx.Tx, so one loader
// serves both the plain-read and in-transaction paths.
type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Register mounts every native-workspace route on mux.
func Register(mux *http.ServeMux, deps Deps) {
	s := &server{deps: deps}
	mux.HandleFunc("GET /api/v1/projects", s.listProjects)
	mux.HandleFunc("POST /api/v1/projects", s.createProject)
	mux.HandleFunc("GET /api/v1/projects/{key}/artifacts", s.listProjectArtifacts)
	mux.HandleFunc("POST /api/v1/projects/{key}/artifacts", s.uploadProjectArtifact)
	mux.HandleFunc("GET /api/v1/settings/repo-projects", s.listRepoProjects)
	mux.HandleFunc("PUT /api/v1/settings/repo-projects/{owner}/{repo}", s.putRepoProject)
	mux.HandleFunc("DELETE /api/v1/settings/repo-projects/{owner}/{repo}", s.deleteRepoProject)
	mux.HandleFunc("GET /api/v1/me/agent-tokens", s.listAgentTokens)
	mux.HandleFunc("POST /api/v1/me/agent-tokens", s.createAgentToken)
	mux.HandleFunc("DELETE /api/v1/me/agent-tokens/{id}", s.revokeAgentToken)
	mux.HandleFunc("GET /api/v1/issues", s.listIssues)
	mux.HandleFunc("POST /api/v1/issues", s.createIssue)
	mux.HandleFunc("GET /api/v1/issues/resolve", s.resolveIssue)
	mux.HandleFunc("GET /api/v1/issues/{key}", s.getIssue)
	mux.HandleFunc("PATCH /api/v1/issues/{key}", s.patchIssue)
	mux.HandleFunc("GET /api/v1/issues/{key}/events", s.listIssueEvents)
	mux.HandleFunc("GET /api/v1/issues/{key}/references", s.getIssueReferences)
	mux.HandleFunc("GET /api/v1/issues/{key}/subscribers", s.listIssueSubscribers)
	mux.HandleFunc("DELETE /api/v1/issues/{key}/subscribers/{session_id}", s.unsubscribeIssueSession)
	mux.HandleFunc("GET /api/v1/issues/{key}/artifacts", s.listArtifacts)
	mux.HandleFunc("POST /api/v1/issues/{key}/artifacts", s.uploadArtifact)
	mux.HandleFunc("POST /api/v1/issues/{key}/messages", s.createMessage)
	mux.HandleFunc("GET /api/v1/issues/{key}/messages/{id}", s.getMessage)
	mux.HandleFunc("GET /api/v1/inbox", s.listInbox)
	mux.HandleFunc("GET /api/v1/search", s.search)
	mux.HandleFunc("GET /api/v1/agents", s.listAgents)
	mux.HandleFunc("POST /api/v1/issues/{key}/asks", s.createAsk)
	mux.HandleFunc("GET /api/v1/issues/{key}/asks", s.listIssueAsks)
	mux.HandleFunc("GET /api/v1/asks/{id}", s.getAsk)
	mux.HandleFunc("PATCH /api/v1/asks/{id}", s.editAsk)
	mux.HandleFunc("POST /api/v1/asks/{id}/answer", s.answerAsk)
	mux.HandleFunc("POST /api/v1/asks/{id}/resolve", s.resolveAsk)
	mux.HandleFunc("GET /api/v1/issues/{key}/comments", s.listComments)
	mux.HandleFunc("POST /api/v1/issues/{key}/comments", s.createComment)
	mux.HandleFunc("POST /api/v1/comments/{id}/resolve", s.resolveComment)
	mux.HandleFunc("POST /api/v1/comments/{id}/reopen", s.reopenComment)
	mux.HandleFunc("GET /api/v1/comments/{id}", s.getComment)
	mux.HandleFunc("POST /api/v1/comments/{id}/accept", s.acceptComment)
	mux.HandleFunc("PATCH /api/v1/comments/{id}", s.editComment)
	mux.HandleFunc("POST /api/v1/comments/{id}/reject", s.rejectComment)
	mux.HandleFunc("GET /api/v1/artifacts/{id}/asks", s.listArtifactAsks)
	mux.HandleFunc("POST /api/v1/artifacts/{id}/asks", s.createArtifactAsk)
	mux.HandleFunc("GET /api/v1/artifacts/{id}/comments", s.listArtifactComments)
	mux.HandleFunc("POST /api/v1/artifacts/{id}/comments", s.createArtifactComment)
	mux.HandleFunc("GET /api/v1/artifacts/{id}/events", s.listArtifactEvents)
	mux.HandleFunc("GET /api/v1/artifacts/{id}/references", s.getArtifactReferences)
	mux.HandleFunc("GET /api/v1/artifacts/{id}/subscribers", s.listArtifactSubscribers)
	mux.HandleFunc("DELETE /api/v1/artifacts/{id}/subscribers/{session_id}", s.unsubscribeArtifactSession)
	mux.HandleFunc("GET /api/v1/artifacts/{id}", s.getArtifact)
	mux.HandleFunc("GET /api/v1/artifacts/{id}/reviews", s.listArtifactReviews)
	mux.HandleFunc("POST /api/v1/artifacts/{id}/reviews", s.createArtifactReview)
	mux.HandleFunc("POST /api/v1/artifacts/{id}/approval-requests", s.requestArtifactApproval)
	mux.HandleFunc("GET /api/v1/artifacts/{id}/text", s.getArtifactText)
	mux.HandleFunc("GET /api/v1/artifacts/{id}/versions/{number}", s.getArtifactVersion)
	mux.HandleFunc("POST /api/v1/artifacts/{id}/versions", s.createNamedVersion)
	mux.HandleFunc("POST /api/v1/artifacts/{id}/edits", s.editArtifact)
	mux.HandleFunc("GET /api/v1/issues/{key}/artifacts/{slug}", s.getArtifact)
	mux.HandleFunc("GET /api/v1/issues/{key}/artifacts/{slug}/text", s.getArtifactText)
	mux.HandleFunc("GET /api/v1/issues/{key}/artifacts/{slug}/versions/{number}", s.getArtifactVersion)
	mux.HandleFunc("POST /api/v1/issues/{key}/artifacts/{slug}/versions", s.createNamedVersion)
	mux.HandleFunc("POST /api/v1/issues/{key}/artifacts/{slug}/edits", s.editArtifact)
	mux.HandleFunc("GET /api/v1/projects/{key}/artifacts/{slug}", s.getArtifact)
	mux.HandleFunc("GET /api/v1/projects/{key}/artifacts/{slug}/text", s.getArtifactText)
	mux.HandleFunc("GET /api/v1/projects/{key}/artifacts/{slug}/versions/{number}", s.getArtifactVersion)
	mux.HandleFunc("POST /api/v1/projects/{key}/artifacts/{slug}/versions", s.createNamedVersion)
	mux.HandleFunc("POST /api/v1/projects/{key}/artifacts/{slug}/edits", s.editArtifact)
	mux.HandleFunc("GET /api/v1/me/state", s.getUserState)
	mux.HandleFunc("PUT /api/v1/me/issues/{key}/state", s.putUserState)
	mux.HandleFunc("GET /api/v1/events", s.streamEvents)
	if deps.TestHooksEnabled {
		mux.HandleFunc("POST /api/v1/events/_test/disconnect", s.disconnectAllStreams)
	}
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
	var ambiguous *pmdoc.ErrTargetAmbiguous
	if errors.As(err, &ambiguous) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":      ambiguous.Error(),
			"code":       "TARGET_AMBIGUOUS",
			"candidates": ambiguous.Candidates,
		})
		return
	}
	var invalidOp *docs.ErrInvalidOp
	if errors.As(err, &invalidOp) {
		writeError(w, "INVALID_OP", http.StatusBadRequest, invalidOp.Error())
		return
	}
	if errors.Is(err, pmdoc.ErrTargetSpansBlocks) {
		writeError(w, "TARGET_SPANS_BLOCKS", http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, pmdoc.ErrTableWidth) {
		writeError(w, "TABLE_WIDTH", http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, docs.ErrAnchorMissing) {
		writeError(w, "ANCHOR_MISSING", http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, docs.ErrAnchorOrphaned) {
		writeError(w, "ANCHOR_ORPHANED", http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, pmdoc.ErrTargetNotFound) {
		writeError(w, "TARGET_NOT_FOUND", http.StatusNotFound, err.Error())
		return
	}
	if errors.Is(err, docs.ErrInvalidMarkdown) {
		writeError(w, "INVALID_MARKDOWN", http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, docs.ErrDocSchema) {
		writeError(w, "DOC_SCHEMA", http.StatusInternalServerError, err.Error())
		slog.Error("dispatch: API document outside Proof schema", "error", err)
		return
	}
	if errors.Is(err, docs.ErrIssueClosed) {
		writeError(w, "ISSUE_CLOSED", http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, docs.ErrServiceUnavailable) {
		writeError(w, "DOC_SERVICE_UNAVAILABLE", http.StatusServiceUnavailable, docs.ErrServiceUnavailable.Error())
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, "NOT_FOUND", http.StatusNotFound, "not found")
		return
	}
	writeError(w, "INTERNAL", http.StatusInternalServerError, "internal server error")
	slog.Error("dispatch: API handler failed", "error", err)
}

func (s *server) optionalActor(r *http.Request) (model.Actor, bool, error) {
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if authorization != "" {
		token, ok := strings.CutPrefix(authorization, "Bearer ")
		if !ok || token == "" {
			return model.Actor{}, false, errorf(http.StatusUnauthorized, "UNAUTHORIZED", "invalid bearer token")
		}
		if matchesSharedAgentToken(token, s.deps.AgentToken) {
			return model.Actor{}, false, nil
		}
		actor, err := s.personalTokenActor(r.Context(), token)
		if err != nil {
			return model.Actor{}, false, err
		}
		if actor.Owner == nil {
			return model.Actor{}, false, errorf(http.StatusUnauthorized, "UNAUTHORIZED", "invalid bearer token")
		}
		return actor, false, nil
	}
	if s.deps.Identity == nil {
		return model.Actor{}, false, errorf(http.StatusInternalServerError, "IDENTITY_ERROR", "identity service unavailable")
	}
	login, err := s.deps.Identity.Login(r)
	if err != nil {
		return model.Actor{}, false, err
	}
	return model.Actor{Kind: "user", ID: login}, true, nil
}

func matchesSharedAgentToken(token, configured string) bool {
	return configured != "" && subtle.ConstantTimeCompare([]byte(token), []byte(configured)) == 1
}

func (s *server) actorFrom(r *http.Request, supplied *model.Actor) (model.Actor, error) {
	actor, present, err := s.optionalActor(r)
	if err != nil {
		return model.Actor{}, err
	}
	if present {
		return actor, nil
	}
	if supplied == nil || supplied.Kind != "session" || strings.TrimSpace(supplied.ID) == "" {
		return model.Actor{}, errorf(http.StatusBadRequest, "ACTOR_KIND", "bearer callers require actor.kind session")
	}
	return model.Actor{
		Kind:   "session",
		ID:     supplied.ID,
		Origin: supplied.Origin,
		Owner:  actor.Owner,
	}, nil
}

func (s *server) writeAuthenticationError(w http.ResponseWriter, err error) {
	if errors.Is(err, identity.ErrNoIdentity) || errors.Is(err, identity.ErrLoginNotAllowed) {
		identity.WriteError(w, err)
		return
	}
	s.writeHandlerError(w, err)
}

func (s *server) requireAuthenticated(w http.ResponseWriter, r *http.Request) bool {
	if _, _, err := s.optionalActor(r); err != nil {
		s.writeAuthenticationError(w, err)
		return false
	}
	return true
}

func (s *server) requireActor(w http.ResponseWriter, r *http.Request, supplied *model.Actor) (model.Actor, bool) {
	actor, err := s.actorFrom(r, supplied)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return model.Actor{}, false
	}
	return actor, true
}

func (s *server) requireHuman(w http.ResponseWriter, r *http.Request) (model.Actor, bool) {
	actor, present, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return model.Actor{}, false
	}
	if !present {
		writeError(w, "HUMAN_ONLY", http.StatusForbidden, "only users may perform this action")
		return model.Actor{}, false
	}
	return actor, true
}

func capExceeded(w http.ResponseWriter, field string, length, limit int) {
	writeError(w, "CAP_EXCEEDED", http.StatusBadRequest, fmt.Sprintf("%s length %d exceeds limit %d", field, length, limit))
}

func len16(value string) int {
	length := 0
	for _, rune := range value {
		length++
		if rune > 0xffff {
			length++
		}
	}
	return length
}

const maxJSONRequestBytes int64 = 1 << 20

type maxBytesDiscarder struct{}

func (maxBytesDiscarder) Header() http.Header             { return nil }
func (maxBytesDiscarder) Write(value []byte) (int, error) { return len(value), nil }
func (maxBytesDiscarder) WriteHeader(int)                 {}

func decodeJSON(r *http.Request, value any) error {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (contentType != "application/json" && !strings.HasSuffix(contentType, "+json")) {
		return errorf(http.StatusUnsupportedMediaType, "JSON_CONTENT_TYPE", "JSON mutations require Content-Type application/json")
	}
	r.Body = http.MaxBytesReader(maxBytesDiscarder{}, r.Body, maxJSONRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return errorf(http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE", "request body exceeds %d bytes", maxJSONRequestBytes)
		}
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

// requireOpenIssue locks an issue row and rejects mutations after completion.
func (s *server) requireOpenIssue(ctx context.Context, tx pgx.Tx, key string) error {
	var open bool
	if err := tx.QueryRow(ctx, `select closed_at is null from issues where key = $1 for update`, key).Scan(&open); err != nil {
		return err
	}
	if !open {
		return errorf(http.StatusConflict, "ISSUE_CLOSED", "issue is closed")
	}
	return nil
}

func versionEventPayload(artifactID, name string, version model.Version, diff *string) map[string]any {
	payload := map[string]any{"artifact_id": artifactID, "name": name, "version": version}
	if diff != nil {
		payload["diff"] = *diff
	}
	return payload
}

func (s *server) namedVersionDiff(ctx context.Context, tx pgx.Tx, artifactID string, version model.Version) (*string, error) {
	if !version.Named || version.Number < 2 {
		return nil, nil
	}
	var previous, current string
	if err := tx.QueryRow(ctx, `
		select markdown from artifact_versions where artifact_id = $1 and number = $2
	`, artifactID, version.Number-1).Scan(&previous); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("load previous artifact version: %w", err)
	}
	if err := tx.QueryRow(ctx, `
		select markdown from artifact_versions where artifact_id = $1 and number = $2
	`, artifactID, version.Number).Scan(&current); err != nil {
		return nil, fmt.Errorf("load current artifact version: %w", err)
	}
	diff := text.UnifiedDiff(previous, current)
	return &diff, nil
}

func externalRef(ref string) (repo string, number string, err error) {
	match := externalRefPattern.FindStringSubmatch(strings.TrimSpace(ref))
	if match == nil {
		return "", "", errorf(http.StatusBadRequest, "INVALID_ISSUE_REF", "invalid issue reference %q", ref)
	}
	return canonicalRepo(match[1], match[2]), match[3], nil
}

func canonicalRepo(owner, repo string) string {
	owner = strings.ToLower(strings.TrimSpace(owner))
	repo = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(repo)), ".git")
	return owner + "/" + repo
}

func externalURL(repo, number string) string {
	return "https://github.com/" + repo + "/issues/" + number
}
