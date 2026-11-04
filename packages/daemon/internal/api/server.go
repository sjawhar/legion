package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// readHeaderTimeout bounds how long a client may take to send its request headers; without it a
// slow-header client holds a listener slot for as long as it likes.
const readHeaderTimeout = 10 * time.Second

// Options are what the routes answer from.
type Options struct {
	// State answers GET /legion/v1/state.
	State StateSource
	// StateTransactions opens the repeatable-read, read-only snapshot the state projection uses.
	StateTransactions StateTransactions
	// ConsumerLag reads the lag of the daemon's durable consumers from NATS (the daemon's
	// intake.Consumers.Lag), which the state route reads after its transaction commits and serves as
	// `consumers`. Nil, a daemon with no workflow and so no consumers, serves none.
	ConsumerLag func(ctx context.Context) []intake.ConsumerLag
	// Supervisor is the claims the claim and operator routes post their requests to.
	Supervisor Supervisor
	// BootTokens resolves a registration's boot token.
	BootTokens *BootTokens
	// Project and DispatchProject are the two names this one project answers to, and the daemon
	// holds both because a row written under one is invisible to a read with the other. Project is
	// the claim project token (`claim.ProjectToken`): lowercased and alphanumeric, the form every
	// claim token, tree lifecycle and controller record is keyed by. DispatchProject is the
	// project key Dispatch itself files the issues under (config.Config.Project, which Dispatch
	// constrains to `^[A-Z][A-Z0-9]{1,9}$`), and so the key of the issue records and of the
	// blockers mirror the release routes read. The two can never be equal in a deployment, since
	// the token is the lowercase of a key that must start uppercase.
	Project         string
	DispatchProject string
	// Settings is the configuration the server holds, behind the pointer every request reads
	// through: the operator bearer, the design gate, the repository equivalence, the projects'
	// merge rules and the settings route's own admission rule. It is the one way a caller states
	// them. A caller whose settings never change hands NewLiveSettings(HeldSettings{…}) and
	// leaves it at that; the daemon swaps its own when one of them changes (LiveSettings.Store),
	// so the change takes effect without rebinding the listening socket or dropping a request.
	// Nil holds the zero value, which admits no operator bearer.
	Settings *LiveSettings
	// ApplySettings is how `PUT /legion/v1/operator/settings` changes a setting: the daemon's one
	// settings holder and the store of what the route changed (internal/settings' Keeper.Route).
	// Nil, which every caller but the daemon means, answers that route 503.
	ApplySettings ApplySettings
	// Controller is the project's controller record, which the controller secret route mints,
	// the claim registration route registers a session on, and the grants route authenticates
	// the registered session against.
	Controller ControllerStore
	// ControllerLaunched is whether the daemon launches the project's controller itself
	// (`controller: daemon`), read at every request that needs it: one controller runs per
	// project, so the controller secret route refuses the operator and a registration registers
	// the controller only from a launch of the controller's claim. Nil, and a daemon under
	// `controller: operator`, answers false, and only the operator's capability registers it.
	//
	// It is read rather than stated because `controller` is a setting that changes under a
	// running daemon: the keeper's holder answers what the daemon does now (internal/daemon's
	// keeperHolder), so a change of the mode moves this answer with it.
	ControllerLaunched func() bool
	// PluginRelaunchable is supervise.Deps.PluginRelaunchable as the API sees it: whether this
	// daemon can relaunch a stale process onto a plugin at all. False is Kubernetes's degraded
	// mode, where a stale registration is recorded and the process is left running: nothing here
	// refuses it either, since refusing calls with no relaunch under way would stall agents that
	// work today.
	PluginRelaunchable bool
	// NoPluginInstall is supervise.Deps.NoPluginInstall as the API sees it: whether this claim's
	// launch installs no plugin at all, which only the daemon knows, since a role's backend and
	// the runtime it launches into are its configuration (internal/daemon's noPluginInstall — a
	// Claude Code role's plugin in a pod is its image's own claude-envoy). Nothing of this
	// daemon relaunches such a claim for its contract and its registration stands, so the routes
	// serve it on the contract it speaks (staleClaim). Nil is a daemon whose every claim a
	// relaunch can carry.
	NoPluginInstall func(c supervise.Claim) bool
	// CallerPod is the runtime's pod at a request's address: the uid of the project's pod that
	// holds addr, and false for an address no pod of the project holds (the Sandbox runtime's
	// PodAt). GET /legion/v1/state authenticates nobody, so this is how it knows which claims'
	// plugin asked (stateFor). Nil is a runtime that runs no pods, whose every caller is served
	// the state as this contract writes it.
	CallerPod func(addr netip.Addr) (string, bool)
	// Stopping is done once the daemon's stop begins, before the stop closes the worker stream
	// connections no route holds (RouteDecisions): an operator's request that would change a claim
	// is refused from then on (stopped), and a route whose decision fails logs it at Info
	// (logFailure), since the stop is what cut it short. Nil is never.
	Stopping context.Context
	// Drained is done once the daemon's stop has drained the API, or the drain ran out, and ends
	// every decision a route asked of a machine (decision). A decision in flight at the stop's
	// start gets the drain to finish, so an operator's suspension whose process is already exiting
	// is recorded. Nil is never.
	Drained context.Context
	// Decisions is where the routes record the claims whose decisions they are running, for the
	// daemon's stop to read (RouteDecisions). Nil is a set nothing reads.
	Decisions *RouteDecisions
	// Log receives what the routes decide; nil is slog.Default().
	Log *slog.Logger
	// Tokens mints the GitHub App leases credential routes return after redeeming a grant.
	Tokens appauth.Tokens
	// GitHubOwners are the GitHub owners of the project's repositories, every one of which the
	// controller's gh bundle holds a read-only token for (appauth.GitHubCredential).
	GitHubOwners func() []string
	// Releaser releases what the runtime holds for a tree whose operator close reserved and
	// finished its cleanup: the daemon's runtime.
	Releaser store.TreeReleaser
	// Trees is the store's durable tree barrier, which the operator routes open a root's tree
	// through and reserve and finish a closed tree's cleanup through.
	Trees TreeLifecycles
	// GitHubAPI is GitHub's REST root, which the handoff route reads an issue branch's head, its
	// handoff file and READY's checks under; empty, in production, is https://api.github.com, and a
	// test points it at a stand-in.
	GitHubAPI string
	// Grants mints and redeems the daemon-local one-command credential handles.
	Grants   *credential.Grants
	Pool     *pgxpool.Pool
	Handlers []intake.Handler
	// PrepareRepository readies what the workflow reads in memory of a Dispatch issue's repository
	// before release_children applies its view of a child (the daemon's prepareObserved, handed the
	// child as Dispatch's detail shows it, intake.ObservedIssue). Nil prepares nothing.
	PrepareRepository func(ctx context.Context, issue intake.DispatchIssue) (intake.DispatchIssue, error)
	// RepositoryUnread says whether a repository is left out for now because its GitHub App
	// installation read failed (the daemon's repositoryUnread): the daemon recovers the issues on
	// it from Dispatch's listing once GitHub answers. release_children never ends a wave on a todo
	// child it leaves unrecorded for one: it answers the child released and goes on, telling the
	// architect at once when the recovery will not record it (enterTodoChild). Nil leaves none out.
	RepositoryUnread func(resolved string) bool
	// AwaitRecovery leaves a child release_children left unrecorded on an unread repository to that
	// repository's recovery (the daemon's awaitRecovery), which tells the tree's architect of each
	// such child it did not record. Nil leaves none.
	AwaitRecovery func(repo, tree, child, parent string)
	// RoleFor is the role that works a phase (the daemon's workflow.RoleFor, handed in because the
	// workflow's tests import this package). The operator's retry of a failed phase worker applies
	// its issue's held phase only when that phase's role is the claim's (retryClaim); nil retries no
	// held phase, leaving every one to its architect.
	RoleFor  func(phase.Phase) claim.Role
	Record   record.Store
	Dispatch dispatch.Client
	// ClaimReady is told of each claim whose agent's ready the claim took, on the session it
	// registered: a launched or relaunched agent once it took its Envoy role, or a live one that
	// took the role back. A ready the claim refuses tells no one. Nil tells no one. It is called
	// before the route answers the agent, so it returns at once.
	ClaimReady func(c supervise.Claim)
	// PromptAgents is the sorted names of every task agent the role prompts dispatch
	// (prompts.RoleReferences), which the registration answers so a session's capability report
	// measures the agents its role is told to run (LEGION-663). Nil or empty answers an empty list.
	PromptAgents []string
	// CapabilityReported is told, after a ready the claim took, of the capability report that ready
	// carried, normalised (capabilities.Normalize), with the claim as the ready left it. A ready that
	// carries no report — the controller's — tells no one, and so does a ready the claim refuses.
	// Nil tells no one. It may persist the report before it returns — the daemon's writes the
	// store — on a context that outlives the agent's connection, and the route answers the agent
	// once it has returned.
	CapabilityReported func(ctx context.Context, c supervise.Claim, report capabilities.Report)
}

// TreeLifecycles is the store's durable tree barrier (store.Store) as the operator routes use it:
// an operator root opens its tree's lifecycle before its claim is stored, and the tree's close
// reserves the tree's cleanup, then finishes it once every claim of the tree has retired.
type TreeLifecycles interface {
	OpenTreeLifecycle(ctx context.Context, project, tree string, authority treelifecycle.Authority) (treelifecycle.Lifecycle, error)
	ReserveOperatorTreeCleanup(ctx context.Context, project, tree string) (treelifecycle.Lifecycle, bool, error)
	CleanupReservedTree(ctx context.Context, project, tree string, epoch uint64, releaser store.TreeReleaser) error
}

// HeldSettings is the configuration the API server holds: everything it reads from legion.yaml
// and nothing else, in one value, so a settings change swaps one pointer rather than reaching
// into a server that is answering requests. It is not the state's settings report (Settings),
// which is every setting the daemon runs on.
type HeldSettings struct {
	// OperatorToken is the bearer every operator route compares against (`operator_token_file`);
	// empty admits no one.
	OperatorToken string
	// DesignGate is the project's `gates.design`, which the controller secret route answers.
	DesignGate config.DesignGate
	// RepositoryRenames is the deployment's repository equivalence (`repository_renames`): the
	// rule that says which owner's installation holds a repository recorded under the name a
	// transfer moved it away from, which the controller's gh bundle reads
	// (appauth.GitHubCredential) and serving reads for the server's own GitHub calls. The zero
	// value configures none.
	RepositoryRenames ghrepo.Renames
	// MergeRules is the merge policy an issue's project configures. Nil has no special policy.
	MergeRules func(project string) config.MergeRules
	// SettingsRoute is the settings route's projected-token arm: the verifier of the cluster
	// issuer and audience `settings_route` names, and the ServiceAccounts it admits. The zero
	// value leaves that route to the operator bearer alone.
	SettingsRoute SettingsRouteAuth
}

// LiveSettings is HeldSettings behind the pointer every request reads through. The holder hashes
// the operator bearer once per Store, never per request, so a swap costs a request nothing.
type LiveSettings struct {
	current atomic.Pointer[settledSettings]
}

// settledSettings is one HeldSettings with the operator bearer already hashed.
type settledSettings struct {
	HeldSettings
	operatorSet  bool
	operatorHash [sha256.Size]byte
}

// NewLiveSettings holds s, which Store replaces.
func NewLiveSettings(s HeldSettings) *LiveSettings {
	live := &LiveSettings{}
	live.Store(s)
	return live
}

// Store replaces the settings every later request reads. A request already in a route keeps the
// ones it began on: it read the pointer once.
func (l *LiveSettings) Store(s HeldSettings) {
	next := &settledSettings{HeldSettings: s}
	if s.OperatorToken != "" {
		next.operatorSet, next.operatorHash = true, sha256.Sum256([]byte(s.OperatorToken))
	}
	l.current.Store(next)
}

// load is the settings this request reads.
func (l *LiveSettings) load() *settledSettings { return l.current.Load() }

type server struct {
	state             StateSource
	stateTransactions StateTransactions
	consumerLag       func(ctx context.Context) []intake.ConsumerLag
	callerPod         func(addr netip.Addr) (string, bool)
	supervisor        Supervisor
	bootTokens        *BootTokens
	// project and dispatchProject are Options.Project and Options.DispatchProject.
	project         string
	dispatchProject string
	// settings is everything the server reads from legion.yaml, behind a pointer every request
	// reads through (Options.Settings): the daemon swaps them when one of them changes, so the
	// server answers the new value without rebinding its socket or dropping a request.
	settings   *LiveSettings
	controller ControllerStore
	// controllerLaunched is Options.ControllerLaunched, read at every request that needs it and
	// never nil here: NewServer fills a caller's nil with the daemon that launches no controller.
	controllerLaunched func() bool
	// pluginRelaunchable is Options.PluginRelaunchable, noPluginInstall Options.NoPluginInstall.
	pluginRelaunchable bool
	noPluginInstall    func(c supervise.Claim) bool
	// controllerMu orders a capability mint against a registration and a controller grant, so a
	// grant the replaced registration authorised is never recorded after the mint revoked them.
	controllerMu sync.Mutex
	tokens       appauth.Tokens
	githubOwners func() []string
	githubAPI    string
	grants       *credential.Grants
	releaser     store.TreeReleaser
	trees        TreeLifecycles
	pool         *pgxpool.Pool
	handlers     []intake.Handler
	prepare      func(ctx context.Context, issue intake.DispatchIssue) (intake.DispatchIssue, error)
	unread       func(resolved string) bool
	await        func(repo, tree, child, parent string)
	roleFor      func(phase.Phase) claim.Role
	records      record.Store
	dispatch     dispatch.Client
	claimReady   func(c supervise.Claim)
	// promptAgents and capabilityReported are Options.PromptAgents and Options.CapabilityReported.
	promptAgents       []string
	capabilityReported func(ctx context.Context, c supervise.Claim, report capabilities.Report)
	// applySettings is Options.ApplySettings, the settings route's one way into the daemon's
	// settings holder.
	applySettings ApplySettings
	// stopping and drained are Options.Stopping and Options.Drained, decisions Options.Decisions.
	stopping  context.Context
	drained   context.Context
	decisions *RouteDecisions
	log       *slog.Logger
	// beforeForgetSessionHandle pauses the route's preflight in its concurrency regression test.
	beforeForgetSessionHandle func()
}

// NewServer builds the daemon's HTTP server on bind:port, the configured address: every interface
// only when bind is 0.0.0.0 or ::, as a daemon that runs as a pod binds. The caller owns its
// lifecycle (ListenAndServe, Shutdown).
//
// Three audiences, three kinds of route: the state everyone reads; the claim lifecycle an agent's
// plugin drives (register, ready, exit), authenticated by its pane's boot token and then by the
// secret its registration was issued; and the operator's spawn surface, authenticated by the
// operator bearer.
func NewServer(bind string, port int, opts Options) *http.Server {
	s := &server{
		state:              opts.State,
		stateTransactions:  opts.StateTransactions,
		consumerLag:        opts.ConsumerLag,
		callerPod:          opts.CallerPod,
		supervisor:         opts.Supervisor,
		bootTokens:         opts.BootTokens,
		project:            opts.Project,
		dispatchProject:    opts.DispatchProject,
		controller:         opts.Controller,
		settings:           opts.Settings,
		applySettings:      opts.ApplySettings,
		controllerLaunched: opts.ControllerLaunched,
		pluginRelaunchable: opts.PluginRelaunchable,
		noPluginInstall:    opts.NoPluginInstall,
		tokens:             opts.Tokens,
		githubOwners:       opts.GitHubOwners,
		githubAPI:          opts.GitHubAPI,
		releaser:           opts.Releaser,
		trees:              opts.Trees,
		grants:             opts.Grants,
		pool:               opts.Pool,
		handlers:           opts.Handlers,
		prepare:            opts.PrepareRepository,
		unread:             opts.RepositoryUnread,
		await:              opts.AwaitRecovery,
		roleFor:            opts.RoleFor,
		records:            opts.Record,
		dispatch:           opts.Dispatch,
		claimReady:         opts.ClaimReady,
		promptAgents:       opts.PromptAgents,
		capabilityReported: opts.CapabilityReported,
		log:                opts.Log,
		stopping:           opts.Stopping,
		drained:            opts.Drained,
		decisions:          opts.Decisions,
	}
	if s.settings == nil {
		s.settings = NewLiveSettings(HeldSettings{})
	}
	if s.controllerLaunched == nil {
		s.controllerLaunched = func() bool { return false }
	}
	if s.grants == nil {
		s.grants = credential.New(nil)
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.stopping == nil {
		s.stopping = context.Background()
	}
	if s.drained == nil {
		s.drained = context.Background()
	}
	if s.decisions == nil {
		s.decisions = NewRouteDecisions()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /legion/v1/state", s.stateRoute)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("POST /legion/v1/claims/register", s.register)
	mux.HandleFunc("POST /legion/v1/claims/ready", s.ready)
	mux.HandleFunc("POST /legion/v1/claims/ready-for-update", s.readyForUpdate)
	mux.HandleFunc("POST /legion/v1/claims/exit", s.exit)
	mux.HandleFunc("POST /legion/v1/grants", s.grant)
	mux.HandleFunc("POST /legion/v1/controller/secret", s.controllerSecret)
	mux.HandleFunc("POST /legion/v1/controller/github-credential", s.controllerGitHubCredential)
	mux.HandleFunc("POST /legion/v1/handoff/complete", s.handoffComplete)
	mux.HandleFunc("POST /legion/v1/issues/status", s.issueStatus)
	mux.HandleFunc("POST /legion/v1/gates/register", s.gateRegister)
	mux.HandleFunc("POST /legion/v1/waves/release", s.waveRelease)
	mux.HandleFunc("POST /legion/v1/phase/backward", s.phaseBackward)
	mux.HandleFunc("POST /legion/v1/phase/retry", s.phaseRetry)
	mux.HandleFunc("POST /legion/v1/signoff", s.signOff)
	mux.HandleFunc("POST /legion/v1/roots/close", s.closeRoot)
	mux.HandleFunc("POST /legion/v1/children/park", s.parkChild)
	mux.HandleFunc("POST /legion/v1/children/rerun", s.rerunChild)

	mux.HandleFunc("POST /legion/v1/operator/claims", s.operator(s.spawn))
	mux.HandleFunc("GET /legion/v1/operator/claims", s.operator(s.list))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/deliver", s.operator(s.claimRequest("deliver", s.deliverEvent)))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/suspend", s.operator(s.claimRequest("suspend", suspendEvent)))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/resume", s.operator(s.claimRequest("resume", resumeEvent)))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/stop", s.operator(s.claimRequest("stop", stopEvent)))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/retry", s.operator(s.retryClaim))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/forget-session", s.operator(s.forgetSession))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/close", s.operator(s.closeTree))
	// The settings route authenticates its caller itself (settingsChange): the operator bearer,
	// or a projected ServiceAccount token this deployment names, which is how Dispatch calls it
	// without holding a copy of the operator token.
	mux.HandleFunc("PUT /legion/v1/operator/settings", s.settingsChange)

	// The catch-all answers every unrouted request, including a known path asked for with the
	// wrong method: "no route" is the whole story the plugin, the operator's CLI, or a proof
	// script needs.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no route"})
	})

	return &http.Server{
		Addr:              net.JoinHostPort(bind, strconv.Itoa(port)),
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}
}

// designGate, renames and mergeRulesOf are the settings this request reads: the one place each is
// read from, so the daemon's swap of them (LiveSettings.Store) reaches every route.
func (s *server) designGate() config.DesignGate { return s.settings.load().DesignGate }

func (s *server) renames() ghrepo.Renames { return s.settings.load().RepositoryRenames }

// mergeRulesOf is the merge policy project configures, the zero policy when none is configured.
func (s *server) mergeRulesOf(project string) config.MergeRules {
	rules := s.settings.load().MergeRules
	if rules == nil {
		return config.MergeRules{}
	}
	return rules(project)
}

// operatorBearer is whether a request's Authorization header carries the operator's bearer:
// given is what followed "Bearer ", and carried whether the header had that prefix at all. The
// two hashes are compared in constant time, so neither the token nor its length leaks through
// the comparison, and a daemon holding no operator token admits nobody.
func (s *server) operatorBearer(given string, carried bool) bool {
	current := s.settings.load()
	return current.operatorSet && carried && secureBearerEqual(current.operatorHash, given)
}

// serving is the repository GitHub holds a recorded one under now (ghrepo.Renames.Serving): the
// other name of its configured pair when the project's owners make that name's owner the one
// whose installation holds it, and the recorded repository itself otherwise. It is the daemon's
// one rule, read here over the owners this server already holds (Options.GitHubOwners) rather
// than written a second time.
func (s *server) serving(recorded ghrepo.Repository) ghrepo.Repository {
	return s.renames().Serving(recorded, s.ownsOwner)
}

// ownsOwner is whether owner is an owner of the project's repositories, compared without case as
// GitHub compares owners. A server built with no owners — a test's, a daemon with no GitHub Apps
// — owns none.
func (s *server) ownsOwner(owner string) bool {
	if s.githubOwners == nil {
		return false
	}
	return slices.ContainsFunc(s.githubOwners(), func(o string) bool { return strings.EqualFold(o, owner) })
}

// decision is the context a route runs a decision of token's claim on. It outlives the request,
// so a caller that hangs up mid-request does not leave a registration or a stop half done, and it
// ends once the daemon's stop has drained the API (Options.Drained). Until the caller calls the
// returned function, which it does once the decision is made, the claim is recorded as decided by
// a route (RouteDecisions). A route that decides other claims too records each of them (begin).
func (s *server) decision(r *http.Request, token claim.Token) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	stop := context.AfterFunc(s.drained, cancel)
	ended := s.decisions.begin([]claim.Token{token})
	return ctx, func() {
		ended()
		stop()
		cancel()
	}
}

// RouteDecisions is the claims whose decisions an API route is running now (server.decision).
// The daemon's stop keeps the worker stream connections of these claims open through the API's
// drain and closes every other one: a route's suspension or stop sends its process the shutdown
// frame over that connection.
type RouteDecisions struct {
	mu     sync.Mutex
	claims map[claim.Token]int
}

// NewRouteDecisions is a set no route has recorded a claim in yet.
func NewRouteDecisions() *RouteDecisions {
	return &RouteDecisions{claims: map[claim.Token]int{}}
}

// Holds is whether a route is running a decision of token's claim now.
func (d *RouteDecisions) Holds(token claim.Token) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.claims[token] > 0
}

// begin records tokens as decided by a route until the returned function runs.
func (d *RouteDecisions) begin(tokens []claim.Token) func() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, token := range tokens {
		d.claims[token]++
	}
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, token := range tokens {
			if d.claims[token]--; d.claims[token] == 0 {
				delete(d.claims, token)
			}
		}
	}
}

// logFailure logs a route's failure at Error, or at Info once the daemon's stop has begun
// (Options.Stopping): the stop is what cut it short, and the next boot takes the claim up.
func (s *server) logFailure(msg string, args ...any) {
	if s.stopping.Err() != nil {
		s.log.Info(msg, args...)
		return
	}
	s.log.Error(msg, args...)
}

// stopped answers 503 and reports true once the daemon's stop has begun (Options.Stopping): an
// operator's request that would change a claim is refused then, rather than decided without the
// claim's worker stream connection, which the stop closes for every claim no route holds. The
// caller asks only after it has recorded its claims (decision, RouteDecisions.begin), so a request
// recorded too late for the stop to keep its connections always finds the stop begun; closeTree's
// second record, after the root's close, is the one exception, and says why. A request reaches
// this once the stop has begun only if the server accepted it before the stop began, or while the
// halt waits on the supervisor's lock before the daemon's serve shuts the server down, which closes
// its listener.
func (s *server) stopped(w http.ResponseWriter, request string) bool {
	if s.stopping.Err() == nil {
		return false
	}
	s.log.Info("api: refused an operator request: the daemon is stopping", "request", request)
	writeJSON(w, http.StatusServiceUnavailable, errorBody(request+" refused: the daemon is stopping; ask again once it is back"))
	return true
}

func (s *server) stateRoute(w http.ResponseWriter, r *http.Request) {
	if s.stateTransactions == nil {
		s.log.Error("api: state transaction source is unset")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "state unavailable"})
		return
	}
	tx, err := s.stateTransactions.BeginTx(r.Context(), pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		s.log.Error("api: begin state transaction failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "state unavailable"})
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	state, err := s.state.State(r.Context(), tx)
	if err != nil {
		s.log.Error("api: state source failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "state unavailable"})
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.log.Error("api: commit state transaction failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "state unavailable"})
		return
	}
	// The consumers' lag is read from NATS after the commit, so a slow or silent NATS server holds
	// no snapshot open; what it does not answer is each consumer's `unavailable`, never a refusal.
	if s.consumerLag != nil {
		state.Consumers = ConsumerLagViewsOf(s.consumerLag(r.Context()))
	}
	writeJSON(w, http.StatusOK, s.stateFor(r, state))
}

// stateFor is state as the plugin that asked for it reads it. The route authenticates nobody, so
// the caller is known by its address alone: a pod of the project (Options.CallerPod), whose claims
// record the plugin contract each registration declared. A plugin reads the state with a strict
// schema and refuses the whole body for one key it does not know, and an update round serves a
// claim of the contract before it until its pod is replaced (version.go's 46). So a caller in a pod
// whose claims record a contract below supervise.PodUpdateContract, the contract that added
// `updateRound` and `updateHead` to the claim views, is served the state without them
// (withoutUpdateRounds), which is the body that contract's schema describes. Every later reader
// takes the two as optional, so the body this serves when the claims cannot be read is the one
// both read. Any other caller — a pod at this contract, the operator's `legion state`, a tmux
// pane — is served the state as this contract writes it.
func (s *server) stateFor(r *http.Request, state State) State {
	// A state with no update round on any claim view is the body every contract reads, so it is
	// served as it is with no read of the claims: outside a roll, that is every request.
	if s.callerPod == nil || s.supervisor == nil || !carriesUpdateRounds(state) {
		return state
	}
	caller, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return state
	}
	pod, inPod := s.callerPod(caller.Addr().Unmap())
	if !inPod {
		return state
	}
	claims, err := s.supervisor.Claims(r.Context())
	if err != nil {
		s.log.Warn("api: the claims of the pod that asked for the state could not be read; it is served the state without the update round fields, which every plugin contract since 45 reads",
			"pod", pod, "error", err)
		return withoutUpdateRounds(state)
	}
	if contract := podContract(claims, pod); contract == 0 || contract >= supervise.PodUpdateContract {
		return state
	}
	return withoutUpdateRounds(state)
}

// carriesUpdateRounds is whether any claim view of state carries `updateRound` or `updateHead`.
func carriesUpdateRounds(state State) bool {
	carries := func(v ClaimView) bool { return v.UpdateRound != "" || v.UpdateHead != "" }
	for _, issue := range state.Issues {
		if issue.Architect != nil && carries(*issue.Architect) {
			return true
		}
		for _, worker := range issue.Workers {
			if carries(worker.Claim) {
				return true
			}
		}
	}
	return false
}

// podContract is the lowest plugin contract a claim whose process runs in pod records, and 0 when
// none records one: every role of a pod is served by the plugin that pod runs, and the body the
// lowest of them reads is one every higher contract reads too.
func podContract(claims []supervise.Claim, pod string) int {
	lowest := 0
	for _, c := range claims {
		if c.Locator == nil || c.Locator.Sandbox == nil || c.Locator.Sandbox.PodUID != pod || c.PluginContract == 0 {
			continue
		}
		if lowest == 0 || c.PluginContract < lowest {
			lowest = c.PluginContract
		}
	}
	return lowest
}

// withoutUpdateRounds is state with no claim view carrying `updateRound` or `updateHead`, the two
// fields supervise.PodUpdateContract added to the state; nothing else of that contract is on the
// route but `settings` keys, which every plugin reads loosely. The issue maps are copied, so the
// state the source answered is left as it was.
func withoutUpdateRounds(state State) State {
	issues := make(map[string]Issue, len(state.Issues))
	for key, issue := range state.Issues {
		if issue.Architect != nil {
			architect := *issue.Architect
			architect.UpdateRound, architect.UpdateHead = "", ""
			issue.Architect = &architect
		}
		if issue.Workers != nil {
			workers := make(map[claim.Role]PhaseView, len(issue.Workers))
			for role, worker := range issue.Workers {
				worker.Claim.UpdateRound, worker.Claim.UpdateHead = "", ""
				workers[role] = worker
			}
			issue.Workers = workers
		}
		issues[key] = issue
	}
	state.Issues = issues
	return state
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		slog.Error("api: encode response", "error", err)
		http.Error(w, `{"error":"encode failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(append(encoded, '\n')); err != nil {
		slog.Debug("api: write response", "error", err)
	}
}
