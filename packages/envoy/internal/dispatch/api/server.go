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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/architecture"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"github.com/sjawhar/envoy/internal/oidc"
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
	Store    *store.Store
	Identity identity.Identity
	// AllowedLogins is the lowercase sign-in allowlist (DISPATCH_ALLOWED_LOGINS): the humans an
	// issue may be assigned to, and the option list GET /users returns.
	AllowedLogins  map[string]struct{}
	AgentToken     string
	DefaultProject string
	ServerURL      string
	Docs           docs.API
	Envoy          *envoy.Client
	Events         *events.Broker
	// GitHub calls the GitHub App API for architecture-source access checks;
	// nil is the "no app credentials yet" state and answers ErrNoAppKey.
	GitHub *githubapp.Client
	// Architecture imports a project's architecture model from its configured
	// source; the ticker, the Refresh route, and the sync tool share it so one
	// project's syncs stay serialized.
	Architecture *architecture.Importer
	// OIDC verifies a JWT-shaped bearer as a Kubernetes pod's projected
	// service-account token; nil is the unconfigured deployment, where a
	// JWT-shaped bearer is only ever an unknown personal token.
	OIDC             *oidc.Verifier
	TestHooksEnabled bool
}

// DepsInput contains raw boot values used to construct API dependencies.
type DepsInput struct {
	Store           *store.Store
	Identity        identity.Identity
	AllowedLogins   map[string]struct{}
	AgentToken      string
	RepoProjectsRaw string
	DefaultProject  string
	ServerURL       string
	EnvoyURL        string
	Docs            docs.API
	Events          *events.Broker
	// App is the loaded GitHub App credentials (nil when unconfigured);
	// GitHubAPIBase overrides the GitHub API origin (DISPATCH_GITHUB_API_BASE).
	App              *auth.AppConfig
	GitHubAPIBase    string
	OIDC             *oidc.Verifier
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
	github, err := githubapp.New(input.App, input.GitHubAPIBase)
	if err != nil {
		return Deps{}, err
	}
	return Deps{
		Store:            input.Store,
		Identity:         input.Identity,
		AllowedLogins:    input.AllowedLogins,
		AgentToken:       input.AgentToken,
		DefaultProject:   defaultProject,
		ServerURL:        strings.TrimSuffix(input.ServerURL, "/"),
		Docs:             input.Docs,
		Envoy:            envoyClient,
		Events:           input.Events,
		GitHub:           github,
		Architecture:     architecture.NewImporter(input.Store, github, input.Events),
		OIDC:             input.OIDC,
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
	deps            Deps
	routeIndex      []routeIndexEntry
	adviceQueryHook func(context.Context, pgx.Tx, string) error
}

// queryer is the pgx surface shared by *pgxpool.Pool and pgx.Tx, so one loader
// serves both the plain-read and in-transaction paths.
type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Register mounts every native-workspace route on mux. The routes live in routes_table.go; the
// same table answers GET /api/v1.
//
// Every route is marked so the shared pool can refuse a second connection to a handler that
// already holds one of its transactions (store.ErrNestedAcquire): one transaction, one
// connection is what keeps the pool from deadlocking, and a handler that breaks it fails here
// instead of in production.
func Register(mux *http.ServeMux, deps Deps) {
	s := &server{deps: deps}
	routes := s.routes()
	s.routeIndex = routeIndexEntries(routes)
	for _, route := range routes {
		mux.HandleFunc(route.Method+" "+route.Pattern, trackTransactions(route.Handler))
	}
	if websocket, ok := deps.Docs.(interface {
		ServeHTTP(http.ResponseWriter, *http.Request)
	}); ok {
		mux.Handle("GET /ws/doc/{room}", trackTransactions(websocket.ServeHTTP))
	}
}

func trackTransactions(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handler(w, r.WithContext(store.WithTransactionTracking(r.Context())))
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
		WriteJSON(w, http.StatusConflict, map[string]any{
			"error":      ambiguous.Error(),
			"code":       "TARGET_AMBIGUOUS",
			"candidates": ambiguous.Candidates,
		})
		return
	}

	var preconditionFailed *docs.ErrPreconditionFailed
	if errors.As(err, &preconditionFailed) {
		currentBlocks := make([]model.EditBlockPrecondition, 0)
		for _, mismatch := range preconditionFailed.Mismatches {
			if mismatch.Scope == "block" && mismatch.Current != nil {
				currentBlocks = append(currentBlocks, model.EditBlockPrecondition{
					ID: mismatch.BlockID, Token: *mismatch.Current,
				})
			}
		}
		WriteJSON(w, http.StatusConflict, map[string]any{
			"error":      preconditionFailed.Error(),
			"code":       "PRECONDITION_FAILED",
			"current":    map[string]any{"document": preconditionFailed.CurrentDocument, "blocks": currentBlocks},
			"mismatches": preconditionFailed.Mismatches,
		})
		return
	}
	var invalidPrecondition *docs.ErrInvalidPrecondition
	if errors.As(err, &invalidPrecondition) {
		writeError(w, "INVALID_PRECONDITION", http.StatusBadRequest, invalidPrecondition.Error())
		return
	}
	if errors.Is(err, docs.ErrPreconditionBusy) {
		writeError(w, "EDIT_QUEUE_FULL", http.StatusTooManyRequests, docs.ErrPreconditionBusy.Error())
		return
	}
	var invalidAskBlock *docs.ErrInvalidAskBlock
	if errors.As(err, &invalidAskBlock) {
		writeError(w, "INVALID_ASK_BLOCK", http.StatusBadRequest, invalidAskBlock.Error())
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
		if s.deps.OIDC != nil && oidc.LooksLikeJWT(token) {
			return s.serviceTokenActor(r, token)
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

// serviceTokenActor authenticates a JWT-shaped bearer as a Kubernetes pod's
// projected service-account token. A rejected token is a 401 of its own: it
// never falls through to the personal-token lookup, so OIDC_TOKEN_INVALID tells
// an operator the token was verified and refused rather than simply unknown.
func (s *server) serviceTokenActor(r *http.Request, token string) (model.Actor, bool, error) {
	claims, err := s.deps.OIDC.Verify(r.Context(), token)
	if err != nil {
		reason := oidc.Reason(err)
		// The class only. The error carries the token's own unverified iss and
		// aud, which an unauthenticated caller chooses: optionalActor runs
		// before any authorization, so logging them lets anyone write what they
		// like into the operator's log on every request.
		slog.Warn("dispatch: service-account token rejected", "reason", reason)
		return model.Actor{}, false, errorf(http.StatusUnauthorized, "OIDC_TOKEN_INVALID",
			"service-account token rejected (%s)", reason)
	}
	return model.Actor{Service: &claims.Subject}, false, nil
}

func (s *server) actorFrom(r *http.Request, supplied *model.Actor) (model.Actor, error) {
	actor, present, err := s.optionalActor(r)
	if err != nil {
		return model.Actor{}, err
	}
	if present {
		return actor, nil
	}
	return bearerSessionActor(actor, supplied)
}

// bearerSessionActor resolves the acting session for a bearer caller: the caller names its own
// session in the request body, and what the token itself proved — a personal token's owner, a
// service token's verified subject — stays attached for attribution. Neither is ever taken from
// the body, which a caller controls.
func bearerSessionActor(authenticated model.Actor, supplied *model.Actor) (model.Actor, error) {
	if supplied == nil || supplied.Kind != "session" || strings.TrimSpace(supplied.ID) == "" {
		return model.Actor{}, errorf(http.StatusBadRequest, "ACTOR_KIND", "bearer callers require actor.kind session")
	}
	return model.Actor{
		Kind:    "session",
		ID:      supplied.ID,
		Origin:  supplied.Origin,
		Owner:   authenticated.Owner,
		Service: authenticated.Service,
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

// capExceededError names how far a text field is over its cap so one trim lands:
// "<field> is N characters over the M-character limit (L/M)". Lengths are UTF-16 units (len16).
func capExceededError(field string, length, limit int) *apiError {
	return errorf(http.StatusBadRequest, "CAP_EXCEEDED",
		"%s is %d characters over the %d-character limit (%d/%d)", field, length-limit, limit, length, limit)
}

// countExceededError is capExceededError for item counts (options, labels).
func countExceededError(code, field string, count, limit int) *apiError {
	return errorf(http.StatusBadRequest, code,
		"%s is %d over the %d-item limit (%d/%d)", field, count-limit, limit, count, limit)
}

func capExceeded(w http.ResponseWriter, field string, length, limit int) {
	err := capExceededError(field, length, limit)
	writeError(w, err.code, err.status, err.message)
}

// requireUUIDPath rejects a non-uuid `{id}` path value with 400 <KIND>_ID_INPUT before it
// reaches a uuid column (where pgx would surface it as a 500). kind is "ask", "comment", or "message".
func requireUUIDPath(w http.ResponseWriter, r *http.Request, kind string) bool {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		if kind == "ask" {
			writeError(w, "ASK_ID_INPUT", http.StatusBadRequest, "ask IDs are UUIDs; use the full ask ID")
		} else {
			writeError(w, strings.ToUpper(kind)+"_ID_INPUT", http.StatusBadRequest, kind+" id must be a full uuid")
		}
		return false
	}
	return true
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

// WriteJSON writes body as the JSON response with the given status.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Warn("dispatch: write json failed", "error", err)
	}
}

func writeError(w http.ResponseWriter, code string, status int, message string) {
	WriteJSON(w, status, map[string]string{"error": message, "code": code})
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

// documentMutationContext joins a document operation to an API transaction and
// retains document-generated events until this handler publishes after commit.
func documentMutationContext(ctx context.Context, tx pgx.Tx) (context.Context, *docs.EventCollector) {
	collector := docs.NewEventCollector()
	return docs.WithEventCollector(docs.WithTx(ctx, tx), collector), collector
}

func (s *server) publishDocumentEvents(collector *docs.EventCollector, events ...model.Event) {
	s.publish(collector.Events()...)
	s.publish(events...)
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

// requireOpenIssue locks an issue row and returns its lifecycle status, rejecting mutations
// after completion without a second issue query.
func (s *server) requireOpenIssue(ctx context.Context, tx pgx.Tx, key string) (string, error) {
	var status string
	var open bool
	if err := tx.QueryRow(ctx, `
		select status, closed_at is null from issues where key = $1 for no key update
	`, key).Scan(&status, &open); err != nil {
		return "", err
	}
	if !open {
		return "", errorf(http.StatusConflict, "ISSUE_CLOSED", "issue is closed")
	}
	return status, nil
}

// A version event is built by docs.ArtifactVersionEventPayload wherever it is appended; a
// document's creation is the one other artifact payload, and it states what the write moved
// through the same helper, so both shapes spell the reference keys once.
func artifactCreatedEventPayload(artifact model.Artifact, changes model.ReferenceChanges) map[string]any {
	payload := map[string]any{"artifact": artifact}
	model.NameReferenceChanges(payload, changes)
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
