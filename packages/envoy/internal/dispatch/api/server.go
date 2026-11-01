// Package api serves Dispatch's native workspace HTTP API.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/agentsecrets"
	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/dispatch/architecture"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"github.com/sjawhar/envoy/internal/oidc"
)

var (
	projectKeyPattern  = regexp.MustCompile(contracts.ProjectKeyPattern)
	issueKeyPattern    = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}-[0-9]+$`)
	externalRefPattern = regexp.MustCompile(`^([^/\s]+)/([^/\s#]+)#([1-9][0-9]*)$`)
)

// projectKeyProblem is every route's refusal of a value that is not a project key: the field it
// came in and the rule, worded once in contracts (PROJECT_KEY_RULE) so the dispatch command and
// the server say the same thing.
func projectKeyProblem(field, value string) string {
	return fmt.Sprintf("%s %q is not a project key: %s", field, value, contracts.ProjectKeyRule)
}

// repoLabelPrefix marks an issue created through DISPATCH_DEFAULT_PROJECT
// with the external repository it came from, so the repository stays
// visible and filterable once it's no longer named by the project key.
const repoLabelPrefix = "repo:"

// ServiceTokenVerifier verifies a Kubernetes pod's projected service-account token.
// *oidc.LazyVerifier is the one Dispatch serves with: until its issuer is discovered every Verify
// fails with an error matching oidc.ErrIssuerUnavailable.
type ServiceTokenVerifier interface {
	Verify(ctx context.Context, raw string) (oidc.Claims, error)
}

// Deps are the API's application dependencies.
type Deps struct {
	Store          *store.Store
	Identity       identity.Identity
	AgentTokens    *auth.SharedAgentTokens
	DefaultProject string
	ServerURL      string
	Docs           docs.API
	// Embedder embeds a search request's query for meaning search (LEGION-549), and its Identity
	// is the only one search compares queries with (INFRA-393); nil means Dispatch has no
	// embedder, and search answers keyword-only on every request, always setting
	// SearchResponse.Degraded.
	Embedder embed.Embedder
	Envoy    *envoy.Client
	Events   *events.Broker
	// GitHub calls the GitHub App API for architecture-source access checks and the web app's
	// GitHub reads; nil is the "no app credentials yet" state and answers ErrNoAppKey.
	GitHub *githubapp.Client
	// Architecture imports a project's architecture model from its configured
	// source; the ticker, the Refresh route, and the sync tool share it so one
	// project's syncs stay serialized.
	Architecture *architecture.Importer
	// OIDC verifies a JWT-shaped bearer as a Kubernetes pod's projected
	// service-account token; nil is the unconfigured deployment, where a
	// JWT-shaped bearer is only ever an unknown personal token. A configured
	// verifier stays non-nil while its issuer is undiscovered, so such a bearer
	// is answered 503 then, never taken for a personal token.
	OIDC ServiceTokenVerifier
	// BrokerTokens decides a caller presenting an access token the secrets broker issued, under
	// the DPoP scheme (brokerTokenActor); nil is a deployment with no broker, which has no such
	// credential kind and answers a DPoP presentation exactly as it always has.
	BrokerTokens *brokertoken.Verifier
	// AgentStream relays a live session's own conversation to a human watching it; nil is a
	// deployment with no NATS, where an agent view is refused STREAM_UNAVAILABLE.
	AgentStream agentstream.Source
	// AgentSecrets relays the credential-request UI to the secrets broker; nil (the broker URL
	// is unconfigured) means the feature is off: the pending list answers null, and every other
	// handler that needs it answers 404 FEATURE_OFF.
	AgentSecrets *agentsecrets.Client
	// Files holds uploaded files' bytes outside Postgres (cmd/dispatch: DISPATCH_FILE_STORE_BUCKET);
	// nil keeps them in each version's row, and a row that still holds bytes is served from the
	// row either way (files.BackfillRows moves them).
	Files            files.Store
	TestHooksEnabled bool
	// StreamHeartbeat is how often a document socket and a live socket resolve their caller again
	// (whileCallerResolves), each closing once the caller no longer resolves, and a live socket
	// pings its page and sends it a heartbeat frame.
	StreamHeartbeat time.Duration
	// Admission admits every table route's request but the test hooks' (register); nil is a
	// Limiter at admission.DefaultLimits. A test sets a small one to fill it.
	Admission *admission.Limiter
	// ReadDeadline bounds every table route's read (GET, HEAD) that the route itself does not bound
	// otherwise (apiRoute.Deadline); zero is defaultReadDeadline. A test sets a short one. A write
	// has no deadline.
	ReadDeadline time.Duration
	// LiveSocketAge is how long a GET /ws/live socket lives before it closes with 4001 (max age),
	// drawn as it opens. Nil draws it uniformly from [contracts.LiveSocketMinAge,
	// contracts.LiveSocketMaxAge), so the sockets a deploy opened together do not all end
	// together; a test returns a fixed age.
	LiveSocketAge func() time.Duration
	// ListenerRosterUncached is for end-to-end test processes only: every read of the listener's
	// session list (route_status on an issue read, GET /api/v1/agents) asks the listener rather
	// than sharing the process's five-second copy, so a test that changes the listener's sessions
	// sees the change at its next read. A deployment leaves it false.
	ListenerRosterUncached bool
	// DashboardEntry is the entry chunk (`/assets/index-*.js`) of the dashboard build this process
	// serves, "" when it serves none; GET /api/v1/dashboard/builds answers it as serving.
	DashboardEntry string
}

// defaultStreamHeartbeat is StreamHeartbeat when DepsInput leaves it zero.
const defaultStreamHeartbeat = 15 * time.Second

// DepsInput contains raw boot values used to construct API dependencies.
type DepsInput struct {
	Store          *store.Store
	Identity       identity.Identity
	AgentTokens    *auth.SharedAgentTokens
	DefaultProject string
	ServerURL      string
	EnvoyURL       string
	// EnvoyToken is the bearer the listener client sends (cmd/dispatch: ENVOY_TOKEN); empty sends
	// none.
	EnvoyToken string
	// EnvoyTimeout replaces the listener client's window. Zero keeps the client's own default;
	// a test exercising a receipt timeout sets a short one rather than waiting that out.
	EnvoyTimeout time.Duration
	Docs         docs.API
	// Embedder embeds a search request's query for meaning search (LEGION-549); nil turns it off.
	Embedder embed.Embedder
	Events   *events.Broker

	// App is the loaded GitHub App credentials (nil when unconfigured);
	// GitHubAPIBase overrides the GitHub API origin (DISPATCH_GITHUB_API_BASE).
	App           *auth.AppConfig
	GitHubAPIBase string
	OIDC          ServiceTokenVerifier
	// BrokerTokens decides a broker-issued DPoP token; nil where no broker is configured. See
	// Deps.BrokerTokens; cmd/dispatch builds it (brokerTokenVerifier).
	BrokerTokens *brokertoken.Verifier
	// AgentStream is the live agent conversation relay; nil where the deployment has no NATS.
	AgentStream agentstream.Source
	// AgentSecretsURL is the secrets broker's API base URL (DISPATCH_AGENT_SECRETS_URL); empty
	// means the feature is off. AgentSecretsTokenFile is the file holding Dispatch's own
	// projected Kubernetes ServiceAccount token, which the client presents on every broker call.
	AgentSecretsURL       string
	AgentSecretsTokenFile string
	// Files is the uploaded-file store; nil keeps files in Postgres. See Deps.Files.
	Files            files.Store
	TestHooksEnabled bool
	// StreamHeartbeat replaces the heartbeat of the document and live sockets
	// (Deps.StreamHeartbeat). Zero keeps fifteen seconds; a test proving a connection closes
	// sets a short one.
	StreamHeartbeat time.Duration
	// Admission and ReadDeadline are Deps.Admission and Deps.ReadDeadline.
	Admission    *admission.Limiter
	ReadDeadline time.Duration
	// ListenerRosterUncached is for end-to-end test processes only; see
	// Deps.ListenerRosterUncached.
	ListenerRosterUncached bool
	// DashboardEntry is this process's dashboard entry chunk; see Deps.DashboardEntry.
	DashboardEntry string
}

// NewDeps parses boot configuration once and returns API dependencies.
func NewDeps(input DepsInput) (Deps, error) {
	defaultProject := strings.TrimSpace(input.DefaultProject)
	if defaultProject != "" && !projectKeyPattern.MatchString(defaultProject) {
		return Deps{}, fmt.Errorf("invalid DISPATCH_DEFAULT_PROJECT %q (expected project key)", defaultProject)
	}
	if input.Events == nil {
		input.Events = events.NewBroker()
	}
	if input.Docs == nil {
		input.Docs = docs.New(docs.Deps{
			Store:     input.Store,
			Events:    input.Events,
			Identity:  input.Identity,
			ServerURL: input.ServerURL,
		})
	}
	var envoyClient *envoy.Client
	if envoyURL := strings.TrimSpace(input.EnvoyURL); envoyURL != "" {
		options := []envoy.Option{envoy.WithToken(input.EnvoyToken)}
		if input.EnvoyTimeout > 0 {
			options = append(options, envoy.WithTimeout(input.EnvoyTimeout))
		}
		envoyClient = envoy.New(envoyURL, options...)
	}
	github, err := githubapp.New(input.App, input.GitHubAPIBase)
	if err != nil {
		return Deps{}, err
	}
	var agentSecretsClient *agentsecrets.Client
	if url := strings.TrimSpace(input.AgentSecretsURL); url != "" {
		agentSecretsClient = agentsecrets.New(url, input.AgentSecretsTokenFile)
	}
	heartbeat := input.StreamHeartbeat
	if heartbeat == 0 {
		heartbeat = defaultStreamHeartbeat
	}
	return Deps{
		Store:                  input.Store,
		Identity:               input.Identity,
		AgentTokens:            input.AgentTokens,
		DefaultProject:         defaultProject,
		ServerURL:              strings.TrimSuffix(input.ServerURL, "/"),
		Docs:                   input.Docs,
		Embedder:               input.Embedder,
		Envoy:                  envoyClient,
		Events:                 input.Events,
		GitHub:                 github,
		Architecture:           architecture.NewImporter(input.Store, github, input.Events),
		OIDC:                   input.OIDC,
		BrokerTokens:           input.BrokerTokens,
		AgentStream:            input.AgentStream,
		AgentSecrets:           agentSecretsClient,
		Files:                  input.Files,
		TestHooksEnabled:       input.TestHooksEnabled,
		StreamHeartbeat:        heartbeat,
		Admission:              input.Admission,
		ReadDeadline:           input.ReadDeadline,
		ListenerRosterUncached: input.ListenerRosterUncached,
		DashboardEntry:         input.DashboardEntry,
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
	// lifetime bounds every live socket and the loops that serve them: it is the process's own
	// context, cancelled when the server is shutting down, so a deploy closes each live socket
	// with 1012, ending every subscription it carries, and http.Server.Shutdown is not held for
	// its whole budget by a connection that never goes idle. The delivery hub and the event
	// stream relay run on it. It bounds no delivery: a send a request leaves running (a
	// broadcast's recipients) finishes across the signal, and the wait Register returns holds
	// shutdown for it (detached). Register sets it from its lifetime parameter and refuses a nil one, since nothing would
	// ever end the work it bounds; a test passes its own t.Context(). A server a test builds
	// directly has none unless the test sets it (directServer sets context.Background()).
	lifetime        context.Context
	routeIndex      []routeIndexEntry
	adviceQueryHook func(context.Context, pgx.Tx, string) error
	// suggestionTimeout overrides the allowance the related-item check runs under; zero, which
	// every server the deployment builds leaves it, means relatedCheckAllowance. Only tests set it
	// (newFailingAdviceHandler, and the check's bound tests), so a case's outcome is its own choice
	// rather than its machine's speed.
	suggestionTimeout time.Duration
	// relatedCheckEntered runs at the start of every related-item check, before any of its work.
	// Nil outside tests, and set by a test before the requests it drives, so that a test can hold
	// a write's post-commit check where production has only a slow one.
	relatedCheckEntered func()
	// meaningSearchEntered runs inside the related-item check's meaning stage, with the
	// request-pool connection that stage's read holds and before its query; nil, which every
	// server the deployment builds leaves it, calls nothing. Only a test sets it, to hold the
	// stage where production has only a slow one.
	meaningSearchEntered func()
	// relatedSearchEntered runs inside each of the related-item check's searches, with the
	// connection its rows hold and before they are read; nil, which every server the deployment
	// builds leaves it, calls nothing. Only a test sets it, to hold a stage where production has
	// only a slow query.
	relatedSearchEntered func()
	// broadcastResolveBudget overrides the budget a broadcast's recipient resolution runs under;
	// zero, which every server the deployment builds leaves it, means broadcastResolveTimeout.
	// Only a test sets it, so a budget that ends before every selected session is read is the
	// test's choice rather than a race against five seconds of wall clock (INFRA-249).
	broadcastResolveBudget time.Duration
	// rosters is the process's copy of the listener's live-session list that issue reads judge
	// routes against and the agents list lists (issue_route_status.go). Only register gives it
	// non-zero TTLs.
	rosters rosterCache
	// deliveryHub keeps the open Delivery and Pipeline pages current (LEGION-690); every server
	// register mounts has one, and only a server a test builds directly (directServer) has none.
	deliveryHub *deliveryHub
	// liveSockets is every open GET /ws/live socket, which the test hooks close.
	liveSockets liveSocketSet
	// liveKinds starts a live socket's subscriptions to the views beside the delivery views, by
	// kind (live_subscriptions.go); a kind it does not hold is refused.
	liveKinds map[string]liveKind
	// liveWriterBeforePick is each live socket's beforePick (live_socket.go): nil outside tests,
	// and set by a test before the socket it drives opens.
	liveWriterBeforePick func(socket *liveSocket, takesStream bool)
	// agentWatchInterval is how often an agent view re-arms its session: agentStreamWatchInterval,
	// which a test replaces.
	agentWatchInterval time.Duration
	// eventRelay is the live feed every events subscription reads (INFRA-66), on every server
	// register mounts: it starts its listening connection on the first subscription.
	eventRelay *eventStreamRelay
	// eventHorizon is the server's one events.Horizon: the safe horizon every reader that pages
	// the event log by a global id cursor reads up to, shared through horizon() so one
	// observation in flight serves them all. horizonOnce builds it on first use rather than in
	// register, which must not touch the Store.
	horizonOnce  sync.Once
	eventHorizon *events.Horizon
	// bootMark closes once the server's boot mark has been taken (takeBootMark), which the marks
	// in eventMarks begin with; nil on a server with no Store.
	bootMark chan struct{}
	// eventMarks is the event log marks the relay's first start and GET /auth/whoami bound the ids
	// a writer can still hold open by (event_marks.go).
	eventMarks eventMarks
	// detached is everything a request started that runs on past its client: every keyed write's
	// handler (keyedWrites, which runs it on a context the client's going away does not cancel) and
	// every delivery a handler started and did not wait for (a broadcast's recipients, a sweep's
	// resumed attempts). Each runs on a context the request's end does not cancel, and shutdown
	// waits for them (waitBackground) before the database pool closes, so a deploy finishes them as
	// http.Server.Shutdown finishes the requests in flight. A send bound to lifetime instead
	// was cut off by SIGTERM, which cancels that context before the HTTP drain even begins.
	detached backgroundWork
	// requestKeyLease, requestKeyRenewal and requestKeyRunLimit replace a keyed request's lease
	// (IdempotencyLease, a minute), how often a running handler renews it (keyedLeaseRenewal) and
	// how long a handler runs (keyedHandlerLimit); zero, which every server the deployment builds
	// leaves them, keeps those. Only a test sets them, so a handler that outlives its lease or its
	// limit is the test's choice rather than minutes of wall clock.
	requestKeyLease, requestKeyRenewal, requestKeyRunLimit time.Duration
	// transfer replaces the bounds a request's body arrives under and its answer goes out under
	// (defaultTransferBounds: no silence longer than 20 s, and 32 KiB/s after a minute's grace);
	// the zero value, which every server the deployment builds leaves it, keeps those. Only a
	// test sets it, so a body or an answer that stalls is the test's choice rather than twenty
	// seconds of wall clock.
	transfer transferBounds
	// repeatFoundInFlight is called when the keyed layer finds a repeat's key held by a first
	// request still running, before it reads a byte of the repeat's body; nil, which every server
	// the deployment builds leaves it, calls nothing. Only a test sets it, to send the rest of a
	// body it held back once the repeat is waiting on it.
	repeatFoundInFlight func()
	// beforePublish is called when a handler is about to publish what it committed (publish),
	// before the first of its events goes out; nil, which every server the deployment builds
	// leaves it, calls nothing. Only a test sets it, to hold a write between its commit and its
	// events and watch what another writer of the same document may do meanwhile.
	beforePublish func()
	// slowWrites is the slow-write test hook's delay for each keyed method and path it was armed
	// for (POST /api/v1/writes/_test/slow), by "METHOD path"; it holds nothing on a server without
	// TestHooksEnabled, which mounts no hook.
	slowWrites sync.Map
	// warnedUnmatchedRuns holds a warnedProductionRun for each deploy run productionApplies has
	// warned holds no production job or no configured apply step, so a read that judges a run again
	// does not warn about it again. Nothing evicts an entry: the map holds one per configured job
	// names and apply step that it has warned about, until the configuration is fixed or the process
	// restarts.
	warnedUnmatchedRuns sync.Map
	// termStats is the per-project term-statistics refreshes a related-item check started
	// (ensureSearchTermStats), keyed by project, so a project's many checks start one.
	termStats singleflight.Group
	// relatedRecordRetrying is how many related_checks records being retried in the background
	// (recordRelatedChecks, run as detached work) hold or wait on a pool connection right now, at
	// most relatedRecordRetryLimit.
	relatedRecordRetrying atomic.Int32
}

// warnedProductionRun keys server.warnedUnmatchedRuns: one deploy run under one configured set of
// production job names and optional apply step.
type warnedProductionRun struct {
	repo      string
	runID     int64
	jobNames  string
	applyStep string
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
// already holds one of its transactions (store.ErrNestedAcquire): one caller, one connection is
// what keeps the pool from deadlocking, and a handler that breaks it fails here instead of in
// production.
//
// Every route refuses a path or query parameter holding U+0000 or a byte that is not UTF-8 before
// its handler runs (refuseUnstorableParameters), as decodeJSON refuses a U+0000 in a body.
//
// Every table route but the test hooks is admitted by Deps.Admission before its handler runs, and
// a read is bounded by a deadline too (serveRoute), so past capacity a request is refused at once
// with a retryable 429 instead of queueing without end. The websockets are mounted outside the
// table and are neither.
//
// Every POST and PATCH route takes an Idempotency-Key, and a repeat under one is answered from the
// key rather than run again (keyedWrites, request_keys.go). Every write route - a POST, PATCH, PUT
// or DELETE - that names its presser in Dispatch-Login runs only as that person
// (refuseChangedLogin).
//
// The server holds the delivery hub (LEGION-690), which starts its goroutines on the first
// subscription and runs them until lifetime ends, and mounts GET /ws/live beside the document
// websocket. The live socket takes a bearer as well as a cookie, so it runs under
// decideBrokerToken; the document websocket takes a cookie alone and does not. It also holds the
// event stream relay (INFRA-66), which every events subscription reads and which starts its own
// listening connection on the first of them, also on lifetime. lifetime is the process's own
// context, cancelled when the server is shutting down (server.lifetime says what else it ends).
// The returned Background's Wait blocks until every keyed write's handler and every delivery a
// handler detached from its request (a broadcast's recipients) has settled, and until the hub,
// every live socket (and every subscription it ran) and the relay have ended. Call it only after
// lifetime has been cancelled and before the database pool closes.
//
// Register panics when lifetime is nil: the hub and the relay would run on no context that ever
// ends, so they would outlive the process's shutdown, or the test that built the server, and the
// returned wait would block for ever once an events subscription had started the relay.
func Register(mux *http.ServeMux, lifetime context.Context, deps Deps) Background {
	s := register(mux, lifetime, deps)
	return Background{Wait: s.waitBackground, ResumeDeliveries: s.resumeOwedDeliveries, EventHead: s.eventFeedHead, EventHorizon: s.horizon}
}

// Background is the work a registered server does beside answering its requests, for the process
// that serves it to drive.
type Background struct {
	// Wait is the wait Register describes, for shutdown.
	Wait func()
	// ResumeDeliveries is one pass of the delivery sweep (resumeOwedDeliveries): every delivery a
	// process that died left owed is sent, once, under its own attempt number. The maintenance loop
	// runs it (RunMaintenanceSweep).
	ResumeDeliveries func(context.Context) error
	// EventHead is the event_head GET /auth/whoami answers (eventFeedHead): an id at or below which
	// every event is already final, which a page resumes its first events subscription from.
	EventHead func(context.Context) (int64, error)
	// EventHorizon answers the server's one events.Horizon (horizon(), event_stream_relay.go),
	// built on the first call: the safe horizon every reader that pages the event log by a global
	// id cursor reads up to. The suggestion outcome sweep takes it (RunSuggestionOutcomeSweep) so
	// it trails that horizon as the live event feed does, rather than observing on a Horizon of
	// its own: one per process is what lets its readers share an observation. Only a server
	// registered with a Store has an event log to answer for.
	EventHorizon func() *events.Horizon
}

// waitBackground waits for every keyed handler and every delivery a request left running past
// its client, then for every live socket and the subscriptions it ran, then for the hub's
// goroutines and the relay's. The detached work comes first because it is the only work lifetime
// does not stop: a keyed handler is bounded by keyedHandlerLimit, a delivery by the listener
// client's receipt timeout per recipient, and each reads and writes through the database pool this
// wait is holding open for it.
func (s *server) waitBackground() {
	s.detached.wait()
	s.liveSockets.wait()
	s.deliveryHub.wait()
	s.eventRelay.wait()
}

// backgroundWork counts the work a server runs past its requests' clients, and waits for none to
// be left. Unlike a sync.WaitGroup, work may start while wait is already waiting: a request still
// in flight when shutdown stops waiting for the HTTP drain starts its keyed run then, and wait
// returns only once it sees nothing running.
type backgroundWork struct {
	mu      sync.Mutex
	running int
	// idle is closed when running last fell to zero, and replaced when work starts again.
	idle chan struct{}
}

func (b *backgroundWork) start() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running == 0 {
		b.idle = make(chan struct{})
	}
	b.running++
}

func (b *backgroundWork) done() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.running--
	if b.running == 0 {
		close(b.idle)
	}
}

// goDetached runs work on a goroutine of its own, counted until it returns.
func (b *backgroundWork) goDetached(work func()) {
	b.start()
	go func() {
		defer b.done()
		work()
	}()
}

func (b *backgroundWork) wait() {
	for {
		b.mu.Lock()
		running, idle := b.running, b.idle
		b.mu.Unlock()
		if running == 0 {
			return
		}
		<-idle
	}
}

// register is Register, answering the server it mounted, whose delivery hub and event stream
// relay a test waits out before its database closes. It is also called with an empty Deps, as
// TestTheDeploymentsSuggestionAllowanceIsTheDocumentedBound does, so it must stay cheap and must
// not dial or read the nil Store: whatever reaches the database or the network starts later, from
// a handler, the relay's first subscriber or the hub's, or on a goroutine of its own, as the event
// log's boot mark does (takeBootMark), for a server that has a Store.
func register(mux *http.ServeMux, lifetime context.Context, deps Deps) *server {
	if lifetime == nil {
		panic("api.Register: the lifetime parameter is required: it is the context that ends the delivery hub, the event stream relay and every live socket (a test passes t.Context())")
	}
	s := &server{deps: deps, lifetime: lifetime, agentWatchInterval: agentStreamWatchInterval, rosters: rosterCache{ttl: routeRosterTTL, silenceTTL: routeRosterSilenceTTL}}
	s.liveKinds = map[string]liveKind{
		liveEventsView: {resumes: true, start: s.startEventsView},
		liveAgentView:  {start: s.startAgentView},
	}
	s.eventRelay = newEventStreamRelay(s, lifetime)
	s.takeBootMark()
	s.deliveryHub = newDeliveryHub(s, lifetime, deliveryHubClock{})
	routes := s.routes()
	if undecidable := s.undecidableWrites(routes); len(undecidable) > 0 {
		// A mutating row the project check cannot decide would serve a project-scoped caller a
		// write nothing judged, so the table is refused rather than mounted (project_scope.go).
		panic("api.Register: the route table holds a mutating row the project-scope check cannot decide: " + strings.Join(undecidable, "; "))
	}
	s.routeIndex = routeIndexEntries(routes)
	limiter := deps.Admission
	if limiter == nil {
		limiter = admission.New(admission.DefaultLimits())
	}
	deadline := deps.ReadDeadline
	if deadline == 0 {
		deadline = defaultReadDeadline
	}
	for _, route := range routes {
		mux.HandleFunc(route.Method+" "+route.Pattern, accesslog.HandlerFunc(route.Pattern, s.decideBrokerToken(s.serveRoute(route, limiter, deadline))))
	}
	if websocket, ok := deps.Docs.(interface {
		ServeHTTP(http.ResponseWriter, *http.Request)
	}); ok {
		const pattern = "/ws/doc/{room}"
		mux.Handle("GET "+pattern, accesslog.Handler(pattern, s.whileCallerResolves(trackTransactions(s.refuseUnstorableParameters(pattern, websocket.ServeHTTP)))))
	}
	const livePattern = "/ws/live"
	mux.Handle("GET "+livePattern, accesslog.Handler(livePattern, s.whileCallerResolves(s.decideBrokerToken(trackTransactions(s.refuseUnstorableParameters(livePattern, s.serveLiveSocket))))))
	return s
}

// defaultReadDeadline bounds an ordinary read (GET, HEAD). Twenty seconds is below the thirty the
// load generator and the web app's pages wait before they give up and the sixty the dispatch
// command waits, so the server stops a read's work before its client abandons it, rather than
// finishing work nobody is waiting for while the requests behind it queue. The writing of the
// answer is bounded apart from it (servedAsRead, boundedAnswer), on the silence between its bytes
// rather than on its length, so a client slow to read does not hold what the read holds past it
// and one that keeps reading is answered in full. A write has no deadline: once admitted it runs
// to completion, since an upload takes a minute or more and an architecture import up to two per
// source, and a write cancelled partway rolls back and makes its page retry it for ever. Its
// connection acquires stay bounded by each pool's acquire bound, and its body by the bounds every
// request's body arrives under (defaultTransferBounds).
const defaultReadDeadline = 20 * time.Second

// serveRoute is route's handler as register mounts it, inside the access log and the broker
// token's decision (decideBrokerToken), its layers outermost first: the transaction tracking,
// the drain a write's early answer waits behind, admission, a read's deadline and pinned
// connection, the parameter refusal every route has, the changed-login check, a write's project
// and session scope, an upload's closed-issue refusal, and the keyed layer.
//
// Admission comes before the deadline so a request's wait for room is bounded by admission's own
// wait, and the deadline counts only the work the read was admitted to do; the pinned connection
// comes inside both, so only an admitted read takes one and it is back in the pool before the
// read's admission unit is. A refused request never reaches the keyed layer, so it claims no key -
// and the drain is outside admission so that admission's own 429 waits for the body as every other
// early answer does (drainBeforeAnswer), rather than draining it a second way. The drain is also
// where a write's body gets the bounds it arrives under (drainedBody): a write with no room to
// run in reads its body there before it waits for room (admitted), and one admitted at once
// parks its units at that body's first read, so a body a client trickles holds none of them
// either way. A test hook is neither admitted nor bounded: it is a test's own.
func (s *server) serveRoute(route apiRoute, limiter *admission.Limiter, deadline time.Duration) http.HandlerFunc {
	handler := route.Handler
	if keyedMethod(route.Method) {
		handler = s.keyedWrites(route, handler)
	}
	if route.RefusesClosedIssue {
		handler = s.refuseUploadToAClosedIssue(handler)
	}
	if route.Method != http.MethodGet {
		// Outside the keyed layer and the upload's spool, so a write to another project's
		// object, or into another session's own rows, claims no idempotency key and costs
		// no body (withinProjectScope, withinSessionScope).
		handler = s.withinProjectScope(route, handler)
		handler = s.withinSessionScope(route, handler)
		handler = s.refuseChangedLogin(handler)
	}
	handler = s.refuseUnstorableParameters(route.Pattern, handler)
	if !route.TestHook {
		if route.Method == http.MethodGet {
			if route.Deadline != 0 {
				deadline = route.Deadline
			}
			handler = servedAsRead(deadline, s.transfer.orDefault(), route.pinsConnection(), handler)
		}
		handler = admitted(limiter, route.Weight, handler)
	}
	if route.Method != http.MethodGet {
		// Every layer below may answer before the body is read - admission's refusal included;
		// the answer waits for the body.
		//
		// A client that asked to be told to continue before it sends its body (Expect:
		// 100-continue) and is answered early sends none of it, and its connection closes after
		// the answer, as long as nothing reads that body: net/http sends 100 Continue at the
		// body's first read, and otherwise closes the connection once the answer begins rather
		// than wait on a body that never comes. It decides from the expect-continue reader its
		// own response keeps (go1.27's response.ecReader and reqBody), not from the request's
		// Body, so it is not that trackTransactions hands this layer a copy of the request: the
		// handling holds with the drain's body put on net/http's own request. What keeps the body
		// unread is the drain's guard (drainedBody.askedContinue) and the keyed layer's
		// (answerDrainedRepeat), which TestAnEarlyAnswerToAClientWaitingToContinueAsksForNoBody
		// pins: without either, the client is told to continue and the answer waits on its body.
		handler = s.drainBeforeAnswer(route, handler)
	}
	// The access-log line, which register puts outside this, records what every layer answered -
	// a keyed replay, a refused login, an early answer the drain held, admission's refusal - and
	// the caller the first of them resolved.
	return trackTransactions(handler)
}

// admitted runs handler once limiter admits the request at weight, with the ticket on the
// request's context so a wait elsewhere can park it (admission.Ticket.Park), and answers a
// refusal 429 OVERLOADED.
//
// The request takes its place in admission's arrival order first (admission.Limiter.Arrive) and
// is admitted there and then where the server has room for it. Where it has none, a write whose
// client is already sending reads its whole body off the connection before it joins the queue
// (drainedBody.readWhole) and waits holding no unit: with the body off the connection net/http's
// own read of it runs, so a client that hangs up while its write is queued cancels the request
// there and then and is recorded as the 499 it is, where a queued write with an unread body held
// its slot for its whole wait and was answered a 429 nobody read. The queue serves the write at
// the place it arrived, ahead of every write that arrived while its body did, and the wait it
// then has is the one wait the queue costs it, so no write is refused for waiting twice.
//
// The ticket goes to the drain's body as well as to the context: a body still arriving after its
// request was admitted parks it, so a client still sending holds none of the units
// (drainedBody.park). A body that did not arrive is answered by the drain
// (drainingWriter.refuseUnreadBody) and asks for no room at all.
//
// Such a body asks for the units again at its end, and admission may refuse it there: the
// handler is then cancelled with that refusal as its context's cause (drainedBody.resume), so
// its statements and its commit fail where they stand and nothing of the request is written,
// whether or not the handler reads the refusal from the read that ended its body. Its answer is
// the refusal, which admission.WriteUnserved writes from the cause: the client is still there,
// so this cancellation is never the 499 of a client that went away.
//
// A request whose client went away while it waited is answered nothing, since nobody is left to
// read it, and its line carries 499 rather than the 200 of a handler that wrote nothing: under
// load a client that gives up while queued is the most frequent way a request ends unanswered.
func admitted(limiter *admission.Limiter, weight int, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		arrival := limiter.Arrive(admission.ClassOf(r.Method), weight)
		ticket := arrival.AdmitNow()
		if ticket == nil {
			if body, drained := r.Body.(*drainedBody); drained && !body.readWhole() {
				return
			}
			var err error
			if ticket, err = arrival.Admit(r.Context()); err != nil {
				var refusal *admission.Refusal
				if errors.As(err, &refusal) {
					admission.WriteRefused(w, refusal)
					return
				}
				accesslog.ClientGone(r.Context())
				return
			}
		}
		defer ticket.Release()
		ctx := admission.WithTicket(r.Context(), ticket)
		if body, drained := r.Body.(*drainedBody); drained {
			body.ticket = ticket
			var refused context.CancelCauseFunc
			ctx, refused = context.WithCancelCause(ctx)
			defer refused(nil)
			body.refuse = refused
		}
		handler(w, r.WithContext(ctx))
	}
}

// servedAsRead runs a read's handler under deadline (admission.WithDeadline), whose passing
// writeHandlerError answers 503 REQUEST_TIMEOUT, and, where pin is set, with every request-pool
// statement it runs on the one connection its first statement takes, given back when the handler
// returns (store.PinConnection, apiRoute.pinsConnection). The answer is written under bounds of
// its own (boundedAnswer).
func servedAsRead(deadline time.Duration, bounds transferBounds, pin bool, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := admission.WithDeadline(r.Context(), deadline)
		defer cancel()
		control := http.NewResponseController(w)
		answer := &boundedAnswer{
			ResponseWriter: w,
			movingDeadline: movingDeadline{set: control.SetWriteDeadline, bounds: bounds},
			control:        control,
		}
		defer answer.lift()
		if pin {
			var unpin func()
			ctx, unpin = store.PinConnection(ctx)
			defer unpin()
		}
		handler(answer, r.WithContext(ctx))
	}
}

func trackTransactions(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handler(w, r.WithContext(store.WithTransactionTracking(r.Context())))
	}
}

// whileCallerResolves serves a connection that outlives its request, a websocket, only while its
// caller resolves: on every StreamHeartbeat it resolves the caller again, and once the caller no
// longer resolves (a logout, a membership the sign-in issuer no longer confirms, a revoked token)
// it cancels the connection's context, on which the document server and the live socket close the
// socket. The check runs beside the handler, which holds pooled connections of its own while it
// admits the socket, so it marks a context of its own for the pool (store.ErrNestedAcquire), and
// it resolves on a context detached from the access-log line, which names the caller the socket
// was admitted as (accesslog.Detached).
func (s *server) whileCallerResolves(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, closeConnection := context.WithCancel(r.Context())
		defer closeConnection()
		check := r.WithContext(store.WithTransactionTracking(accesslog.Detached(ctx)))
		go func() {
			heartbeat := time.NewTicker(s.deps.StreamHeartbeat)
			defer heartbeat.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-heartbeat.C:
					// The check resolves the credential the request was admitted with, from a
					// detached copy of that request, which for a broker-issued token asks the
					// broker's answer alone rather than the proof the upgrade already spent
					// (callerStillResolves). Every other credential is resolved exactly as it
					// is on a request.
					if err := s.callerStillResolves(check); err != nil {
						closeConnection()
						return
					}
				}
			}
		}()
		handler(w, r.WithContext(ctx))
	}
}

type apiError struct {
	status  int
	code    string
	message string
	// retryAfter is the Retry-After the answer carries, in whole seconds, and 0 for an answer
	// that carries none. Only a refusal that knows when the caller may come back sets it
	// (brokerTokenActor's 429); writeHandlerError writes the header.
	retryAfter int
}

func (e *apiError) Error() string { return e.message }

func errorf(status int, code, format string, args ...any) *apiError {
	return &apiError{status: status, code: code, message: fmt.Sprintf(format, args...)}
}

// parseQueryInt reads one query parameter as an integer within [min, max]. present is false
// when the caller did not supply the parameter at all; a parameter repeated, not an integer, or
// outside the bound is present with valid false. issue_page.go's parseIssuePage and search.go's
// handler each format their own code and message around a false valid, since the two routes
// disagree on them.
func parseQueryInt(query url.Values, field string, min, max int) (value int, present, valid bool) {
	values, ok := query[field]
	if !ok {
		return 0, false, false
	}
	parsed, err := strconv.Atoi(values[0])
	if len(values) != 1 || err != nil || parsed < min || parsed > max {
		return 0, true, false
	}
	return parsed, true, true
}

// The codes writeHandlerError answers a document it cannot read or serve and an unclassified
// failure with, which a comment's or ask's single read also carries as anchor_block_error
// (anchorBlock).
const (
	codeDocSchema             = "DOC_SCHEMA"
	codeDocServiceUnavailable = "DOC_SERVICE_UNAVAILABLE"
	codeDocumentUnloadable    = "DOCUMENT_UNLOADABLE"
	codeInternal              = "INTERNAL"
	// codeCommitOutcomeUnknown is a commit the database did not answer within its bound
	// (store.ErrCommitTimeout), answered 500. It is not a 503: a 503 with Retry-After tells a
	// client to send the request again, and this one may have committed - the server does not
	// know - so a blind retry would write it twice. A code of its own tells it from the
	// INTERNAL of a failure that wrote nothing, so a client reads what the write would have
	// written, or sends it again under its idempotency key, which answers a replay rather than
	// writing again.
	codeCommitOutcomeUnknown = "COMMIT_OUTCOME_UNKNOWN"
)

// documentErrorCode names an error a document operation returns when it could not read or serve
// the document, and is empty for any other error. writeHandlerError and anchorBlock both name a
// document error by it, and both in one order, so the two cannot name one error differently.
//
// A room or store that could not serve the document is DOC_SERVICE_UNAVAILABLE whatever failed it,
// so both take that code before any branch that reads the error's cause. A failed room carries the
// error another operation failed it with - settlement's schema refusal, a settlement that failed
// three times (its warm-up refused because the issue had closed, among others), a writer's commit
// that failed or its client cancelled, a store write or load that failed, a history that did not
// decode - and that error says nothing of this request: the caller retries once the room is
// evicted, and its retry meets the document itself.
//
// A stored history this request itself could not decode is DOCUMENT_UNLOADABLE, the state a
// rebuild repairs. A tree outside the schema is DOC_SCHEMA, which both take only after the
// branches that name the caller's own input or a block the document does not hold, so a refusal
// whose reason the renderer gave (an ask block that cannot render) stays that refusal.
func documentErrorCode(err error) string {
	switch {
	case errors.Is(err, docs.ErrServiceUnavailable):
		return codeDocServiceUnavailable
	case errors.Is(err, docs.ErrDocumentUnloadable):
		return codeDocumentUnloadable
	case errors.Is(err, docs.ErrDocSchema):
		return codeDocSchema
	}
	return ""
}

// writeHandlerError answers r, whose handler failed with err. A commit the database did not
// answer within its bound is told first: its outcome, unlike every other failure's, is unknown,
// so the operator's log carries the ERROR line whether or not a client is still there to be
// told. A request the server could not serve - its deadline passed, its client went away, or no
// connection of a pool came free within its acquire bound - comes next, as admission.WriteUnserved
// answers it: a retryable 503, or nothing for a client that is gone, and never an ERROR line,
// since under load every request may meet one. Only then is err mapped to the failure it names.
func (s *server) writeHandlerError(w http.ResponseWriter, r *http.Request, err error) {
	// The transaction may have committed: the server sent the COMMIT and never heard back. The
	// operator's log says so at ERROR, since a database that takes ten seconds to answer a commit
	// is a fault, not load the server is meant to absorb. A write runs on its client's context
	// unless it is keyed, and only the commit itself is detached (store.CommitContext), so a
	// client that leaves while the commit runs out its bound is the likeliest way this is met:
	// the 500 then goes to nobody, and the line records the 499 that it is, but the unknown
	// outcome is reported either way.
	if store.IsCommitTimeout(err) {
		slog.Error("dispatch: API write commit ran out its bound; its outcome is unknown and the transaction may have committed", "error", err)
		if admission.WriteUnserved(w, r.Context(), err) {
			return
		}
		writeError(w, codeCommitOutcomeUnknown, http.StatusInternalServerError,
			"the database did not answer this write's commit in time; its outcome is unknown, so read what it wrote before sending it again")
		return
	}
	if admission.WriteUnserved(w, r.Context(), err) {
		return
	}
	// A write that waited for a document's writer gate gave its admission units back while it
	// waited (docs.Service.AcquireDocumentWriter) and found no room to take them back: refused
	// as admission refuses a request, before its transaction opened.
	var refusal *admission.Refusal
	if errors.As(err, &refusal) {
		admission.WriteRefused(w, refusal)
		return
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		if apiErr.retryAfter > 0 {
			// Before the body: writeError writes the status and the headers go with it.
			w.Header().Set("Retry-After", strconv.Itoa(apiErr.retryAfter))
		}
		writeError(w, apiErr.code, apiErr.status, apiErr.message)
		// A 500 the handler named is a server fault like any other, so the operator's log shows it.
		if apiErr.status == http.StatusInternalServerError {
			slog.Error("dispatch: API handler failed", "code", apiErr.code, "error", err)
		}
		return
	}
	documentCode := documentErrorCode(err)
	switch documentCode {
	case codeDocServiceUnavailable:
		writeError(w, documentCode, http.StatusServiceUnavailable, docs.ErrServiceUnavailable.Error())
		return
	case codeDocumentUnloadable:
		writeError(w, documentCode, http.StatusConflict, docs.ErrDocumentUnloadable.Error())
		slog.Warn("dispatch: API document history cannot load", "error", err)
		return
	}
	// The edit route's own ambiguity error names the operation and the quote; the bare pmdoc one
	// still serves every other caller of FindQuote.
	var ambiguousQuote *docs.ErrAnchorAmbiguous
	if errors.As(err, &ambiguousQuote) {
		writeAmbiguousTarget(w, ambiguousQuote.Error(), ambiguousQuote.Candidates)
		return
	}
	var ambiguous *pmdoc.ErrTargetAmbiguous
	if errors.As(err, &ambiguous) {
		writeAmbiguousTarget(w, ambiguous.Error(), ambiguous.Candidates)
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
	// A write whose text a concurrent browser change removed before the write could be versioned
	// (LEGION-269). Nothing was written, so this reads like PRECONDITION_FAILED: re-read the
	// document and decide again.
	var lostEdit *docs.ErrEditLost
	if errors.As(err, &lostEdit) {
		body := map[string]any{
			"error":        lostEdit.Error(),
			"code":         "EDIT_LOST_TO_CONCURRENT_CHANGE",
			"participants": participantsOrEmpty(lostEdit.Participants),
		}
		if lostEdit.Suggestion != "" {
			body["suggestion"] = lostEdit.Suggestion
		} else {
			body["lost_ops"] = lostEdit.Ops
		}
		WriteJSON(w, http.StatusConflict, body)
		return
	}
	var invalidPrecondition *docs.ErrInvalidPrecondition
	if errors.As(err, &invalidPrecondition) {
		writeError(w, "INVALID_PRECONDITION", http.StatusBadRequest, invalidPrecondition.Error())
		return
	}
	if errors.Is(err, docs.ErrPreconditionBusy) {
		// Refused before its transaction opened, so nothing ran and the writer may retry.
		w.Header().Set("Retry-After", admission.RetryAfter)
		writeError(w, "EDIT_QUEUE_FULL", http.StatusTooManyRequests, docs.ErrPreconditionBusy.Error())
		return
	}
	var invalidAskBlock *docs.ErrInvalidAskBlock
	if errors.As(err, &invalidAskBlock) {
		writeError(w, "INVALID_ASK_BLOCK", http.StatusBadRequest, invalidAskBlock.Error())
		return
	}
	var unrepresentableAsk *docs.ErrAskBlockUnrepresentable
	if errors.As(err, &unrepresentableAsk) {
		writeError(w, "ASK_BLOCK_TEXT", http.StatusBadRequest, unrepresentableAsk.Error())
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
	if docs.IsTooLarge(err) {
		writeError(w, "CAP_EXCEEDED", http.StatusRequestEntityTooLarge, err.Error())
		slog.Warn("dispatch: API refused a document too large to store", "error", err)
		return
	}
	// A tree outside the schema that the document holds answers 409 with its one message, the same
	// on every route whatever the route wrapped it in (docs.OutsideSchemaError). Any other schema
	// refusal is a tree an operation produced, which is the server's fault.
	if documentCode == codeDocSchema {
		var outside *docs.OutsideSchemaError
		if errors.As(err, &outside) {
			writeError(w, documentCode, http.StatusConflict, outside.Error())
			slog.Warn("dispatch: API document outside Proof schema", "error", err)
			return
		}
		writeError(w, documentCode, http.StatusInternalServerError, err.Error())
		slog.Error("dispatch: API document outside Proof schema", "error", err)
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
	writeError(w, codeInternal, http.StatusInternalServerError, "internal server error")
	slog.Error("dispatch: API handler failed", "error", err)
}

// writeAmbiguousTarget serves a quote that resolved in several places: the caller picks one by
// the candidates' positions, or narrows the quote.
func writeAmbiguousTarget(w http.ResponseWriter, message string, candidates []pmdoc.Candidate) {
	WriteJSON(w, http.StatusConflict, map[string]any{
		"error":      message,
		"code":       "TARGET_AMBIGUOUS",
		"candidates": candidates,
	})
}

// refuseChangedLogin runs a write route's handler only for the person who pressed the write, when
// the request names one in DispatchLoginHeader: the page sends the login of whoever was signed in
// at the press with every write, retries included, and another tab may sign someone else in in
// place, so a retry goes out on the new cookie before this tab knows. A request whose caller is
// another person, or no person at all (a bearer), is answered 409 LOGIN_CHANGED before anything
// runs - before its Idempotency-Key is read, so its key is untouched. A caller that resolves to
// nobody (a lapsed cookie) is left to the route's own refusal, a 401, so a write a session lapsed
// under waits for the sign-in that follows. A request without the header - the dispatch command,
// an agent, a script - is not checked. Logins are compared as model.CanonicalLogin compares them.
func (s *server) refuseChangedLogin(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pressed := r.Header.Values(contracts.DispatchLoginHeader)
		if len(pressed) == 0 {
			handler(w, r)
			return
		}
		actor, human, err := s.optionalActor(r)
		if err != nil {
			handler(w, r)
			return
		}
		// The caller is resolved once a request: the key layer and the handler read this.
		r = r.WithContext(context.WithValue(r.Context(), resolvedCallerKey{}, resolvedCaller{actor: actor, human: human}))
		login := strings.TrimSpace(pressed[0])
		if len(pressed) == 1 && login != "" && human && model.CanonicalLogin(actor.ID) == model.CanonicalLogin(login) {
			handler(w, r)
			return
		}
		writeError(w, contracts.DispatchLoginChanged, http.StatusConflict, fmt.Sprintf(
			"this write was pressed by %q and its request is not signed in as them: nothing ran; it goes only as the person who pressed it",
			login))
	}
}

// optionalActor resolves the request's caller. The authentication scheme decides the credential
// before anything reads a token: RFC 9449 Section 7.2 refuses a DPoP-bound token presented under
// any other scheme, so a broker-issued token reaches Dispatch under DPoP alone and a DPoP
// presentation is never retried as another kind. Each branch names that caller on the request's
// access-log line (accesslog.Note), which costs the line what the authentication already
// resolved and never a lookup of its own.
func (s *server) optionalActor(r *http.Request) (model.Actor, bool, error) {
	// The caller may already be decided: a keyed write resolves before its handler
	// (keyedWrites), and a DPoP presentation is decided once a request however many layers
	// resolve it (decideBrokerToken), which is why a refusal rides along — a proof spent on
	// the first resolution cannot answer the second.
	if caller, ok := r.Context().Value(resolvedCallerKey{}).(resolvedCaller); ok {
		return caller.actor, caller.human, caller.err
	}
	if _, presented := brokertoken.Presented(r); presented {
		return s.brokerTokenActor(r)
	}
	if token, present := auth.BearerToken(r); present {
		if token == "" {
			return model.Actor{}, false, errorf(http.StatusUnauthorized, "UNAUTHORIZED", "invalid bearer token")
		}
		if auth.MatchesSharedAgentToken(r, token, s.deps.AgentTokens) {
			accesslog.Note(r.Context(), accesslog.KindSharedToken, "")
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
		accesslog.Note(r.Context(), accesslog.KindToken, *actor.Owner)
		return actor, false, nil
	}
	if s.deps.Identity == nil {
		return model.Actor{}, false, errorf(http.StatusInternalServerError, "IDENTITY_ERROR", "identity service unavailable")
	}
	login, err := s.deps.Identity.Login(r)
	if err != nil {
		return model.Actor{}, false, err
	}
	accesslog.Note(r.Context(), accesslog.KindUser, login)
	return model.Actor{Kind: "user", ID: login}, true, nil
}

// serviceTokenActor authenticates a JWT-shaped bearer as a Kubernetes pod's
// projected service-account token. A rejected token is a 401 of its own: it
// never falls through to the personal-token lookup, so OIDC_TOKEN_INVALID tells
// an operator the token was verified and refused rather than simply unknown. A
// token the verifier could not judge because its issuer has not been discovered
// is a 503 OIDC_ISSUER_UNAVAILABLE, never a 401: the caller retries rather than
// reads its token as refused.
func (s *server) serviceTokenActor(r *http.Request, token string) (model.Actor, bool, error) {
	claims, err := s.deps.OIDC.Verify(r.Context(), token)
	if errors.Is(err, oidc.ErrIssuerUnavailable) {
		return model.Actor{}, false, errorf(http.StatusServiceUnavailable, "OIDC_ISSUER_UNAVAILABLE",
			"the service-account token issuer cannot be reached; try again in a minute")
	}
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
	accesslog.Note(r.Context(), accesslog.KindService, claims.Subject)
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
	return bearerSessionActor(r, actor, supplied)
}

// bearerSessionActor resolves the acting session for a bearer caller: the caller names its own
// session in the request body, and what the token itself proved — a personal token's owner, a
// service token's verified subject — stays attached for attribution. Neither is ever taken from
// the body, which a caller controls. The session is the caller the access-log line names, since
// it is the one a reader of the log is looking for; the token's own proof is already noted.
//
// A credential that proved a session of its own — a broker-issued token, which the broker binds
// to one session — decides it instead, so such a caller need not name itself at all. A body
// actor is then only an agreement: one that names the same session is taken, for its Origin,
// which nothing else carries; one that names any other writer is 403 ACTOR_SESSION_MISMATCH,
// naming the session the credential proved, rather than silently recording the write as a
// session the caller merely asked for. Every credential that proves no session — a cookie, a
// personal token, a Kubernetes service-account token — keeps the rule above exactly.
func bearerSessionActor(r *http.Request, authenticated model.Actor, supplied *model.Actor) (model.Actor, error) {
	if authenticated.Kind == "session" && authenticated.ID != "" {
		// A body that names no writer at all leaves the session to the credential; one that names
		// anything else than that same session is the mismatch.
		named := supplied != nil && (strings.TrimSpace(supplied.ID) != "" || strings.TrimSpace(supplied.Kind) != "")
		if named && (supplied.Kind != "session" || strings.TrimSpace(supplied.ID) != authenticated.ID) {
			return model.Actor{}, errorf(http.StatusForbidden, "ACTOR_SESSION_MISMATCH",
				"this request's credential acts for session %s; its actor names another writer", authenticated.ID)
		}
		actor := authenticated
		if supplied != nil {
			actor.Origin = supplied.Origin
		}
		accesslog.Note(r.Context(), accesslog.KindSession, actor.ID)
		return actor, nil
	}
	if supplied == nil || supplied.Kind != "session" || strings.TrimSpace(supplied.ID) == "" {
		return model.Actor{}, errorf(http.StatusBadRequest, "ACTOR_KIND", "bearer callers require actor.kind session")
	}
	accesslog.Note(r.Context(), accesslog.KindSession, supplied.ID)
	return model.Actor{
		Kind:    "session",
		ID:      supplied.ID,
		Origin:  supplied.Origin,
		Owner:   authenticated.Owner,
		Service: authenticated.Service,
	}, nil
}

// writeAuthenticationError answers a caller that did not resolve: 401 NO_IDENTITY, or 503
// SIGNIN_UNAVAILABLE for a person whose lapsed confirmation the sign-in issuer could not renew,
// as identity.WriteError renders both; anything else as writeHandlerError does. Only identity's
// own sentinels reach identity.WriteError, so a service-account token's issuer outage
// (serviceTokenActor's OIDC_ISSUER_UNAVAILABLE) never reads as the sign-in issuer's.
func (s *server) writeAuthenticationError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, identity.ErrNoIdentity) || errors.Is(err, identity.ErrSignInUnavailable) {
		identity.WriteError(w, r, err)
		return
	}
	s.writeHandlerError(w, r, err)
}

func (s *server) requireAuthenticated(w http.ResponseWriter, r *http.Request) bool {
	if _, _, err := s.optionalActor(r); err != nil {
		s.writeAuthenticationError(w, r, err)
		return false
	}
	return true
}

func (s *server) requireActor(w http.ResponseWriter, r *http.Request, supplied *model.Actor) (model.Actor, bool) {
	actor, err := s.actorFrom(r, supplied)
	if err != nil {
		s.writeAuthenticationError(w, r, err)
		return model.Actor{}, false
	}
	return actor, true
}

// decodeOptionalJSON decodes the request body into dst when the request carries one, so a route
// whose body is optional (a cookie caller sends none; a bearer names its session in `actor`)
// accepts both. It writes the decode error and returns false on a malformed body.
func (s *server) decodeOptionalJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.ContentLength == 0 {
		return true
	}
	if err := decodeJSON(r, dst); err != nil {
		s.writeHandlerError(w, r, err)
		return false
	}
	return true
}

func (s *server) requireHuman(w http.ResponseWriter, r *http.Request) (model.Actor, bool) {
	actor, present, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, r, err)
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

// decodeJSON decodes r's body, one JSON value of at most maxJSONRequestBytes, into value, which
// declares every member the body may carry. A string the body holds anywhere that carries U+0000
// is refused, naming where it stands (unstorableJSON).
func decodeJSON(r *http.Request, value any) error {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (contentType != "application/json" && !strings.HasSuffix(contentType, "+json")) {
		return errorf(http.StatusUnsupportedMediaType, "JSON_CONTENT_TYPE", "JSON mutations require Content-Type application/json")
	}
	r.Body = http.MaxBytesReader(maxBytesDiscarder{}, r.Body, maxJSONRequestBytes)
	var read bytes.Buffer
	decoder := json.NewDecoder(io.TeeReader(r.Body, &read))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return errJSONTooLarge
		}
		return errorf(http.StatusBadRequest, "INVALID_JSON", "invalid JSON body: %v", err)
	}
	if decoder.More() {
		return errorf(http.StatusBadRequest, "INVALID_JSON", "request body must contain one JSON value")
	}
	if refusal := unstorableJSON("", read.Bytes()); refusal != nil {
		return refusal
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

// publish fans out what a handler committed, after its transaction committed and while it still
// holds whatever it took to write - a document's writer gate among them, which ends only once
// these events are out (editArtifact, storeArtifact).
func (s *server) publish(events ...model.Event) {
	if s.beforePublish != nil {
		s.beforePublish()
	}
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

// requireOpenIssue locks an issue row and returns its lifecycle status, rejecting mutations
// after completion without a second issue query.
func (s *server) requireOpenIssue(ctx context.Context, tx pgx.Tx, key string) (string, error) {
	return issueOpenStatus(ctx, tx, key, ownerRowLock)
}

// issueOpenStatus is that same read and refusal made through q under lock, so the check a
// comment makes from the pool before its transaction opens refuses exactly what the locked
// check will.
func issueOpenStatus(ctx context.Context, q queryer, key, lock string) (string, error) {
	var status string
	var open bool
	if err := q.QueryRow(ctx, `
		select status, closed_at is null from issues where key = $1`+lock, key).Scan(&status, &open); err != nil {
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

func externalURLs(repo, number string) []string {
	return []string{
		externalURL(repo, number),
		"https://github.com/" + repo + "/pull/" + number,
	}
}
