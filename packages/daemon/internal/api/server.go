package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
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
	// Supervisor is the claims the claim and operator routes post their requests to.
	Supervisor Supervisor
	// BootTokens resolves a registration's boot token.
	BootTokens *BootTokens
	// Project is the project token new claims are filed under (`claim.ProjectToken`).
	Project string
	// OperatorToken is the bearer every operator route compares against; empty admits no one.
	OperatorToken string
	// Controller is the project's controller record, which the controller secret route mints,
	// the claim registration route registers a session on, and the grants route authenticates
	// the registered session against.
	Controller ControllerStore
	// DesignGate is the project's `gates.design`, which the controller secret route answers.
	DesignGate config.DesignGate
	// ControllerLaunched is the daemon launching the project's controller itself (`controller:
	// daemon`): one controller runs per project, so the controller secret route refuses the operator
	// and a registration registers the controller only from a launch of the controller's claim.
	// Unset (`controller: operator`), only the operator's capability registers it.
	ControllerLaunched bool
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
	// GitHubOwner is the configured repository's owner: the account both Apps are installed on,
	// whose installation every credential route mints for.
	GitHubOwner string
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
	// Repository is the repository an issue's project works in (`projects.<KEY>.repo`), false for a
	// project the configuration has no repository for; the handoff route reads the issue branch
	// there. Nil answers no project.
	Repository func(project string) (ghrepo.Repository, bool)
	// Grants mints and redeems the daemon-local one-command credential handles.
	Grants   *credential.Grants
	Pool     *pgxpool.Pool
	Handlers []intake.Handler
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

type server struct {
	state             StateSource
	stateTransactions StateTransactions
	supervisor        Supervisor
	bootTokens        *BootTokens
	project           string
	operatorSet       bool
	operatorHash      [sha256.Size]byte
	controller        ControllerStore
	designGate        config.DesignGate
	// controllerLaunched is Options.ControllerLaunched.
	controllerLaunched bool
	// controllerMu orders a capability mint against a registration and a controller grant, so a
	// grant the replaced registration authorised is never recorded after the mint revoked them.
	controllerMu sync.Mutex
	tokens       appauth.Tokens
	githubOwner  string
	githubAPI    string
	repository   func(project string) (ghrepo.Repository, bool)
	grants       *credential.Grants
	releaser     store.TreeReleaser
	trees        TreeLifecycles
	pool         *pgxpool.Pool
	handlers     []intake.Handler
	records      record.Store
	dispatch     dispatch.Client
	claimReady   func(c supervise.Claim)
	// promptAgents and capabilityReported are Options.PromptAgents and Options.CapabilityReported.
	promptAgents       []string
	capabilityReported func(ctx context.Context, c supervise.Claim, report capabilities.Report)
	// stopping and drained are Options.Stopping and Options.Drained, decisions Options.Decisions.
	stopping  context.Context
	drained   context.Context
	decisions *RouteDecisions
	log       *slog.Logger
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
		supervisor:         opts.Supervisor,
		bootTokens:         opts.BootTokens,
		project:            opts.Project,
		controller:         opts.Controller,
		designGate:         opts.DesignGate,
		controllerLaunched: opts.ControllerLaunched,
		tokens:             opts.Tokens,
		githubOwner:        opts.GitHubOwner,
		githubAPI:          opts.GitHubAPI,
		repository:         opts.Repository,
		releaser:           opts.Releaser,
		trees:              opts.Trees,
		grants:             opts.Grants,
		pool:               opts.Pool,
		handlers:           opts.Handlers,
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
	if opts.OperatorToken != "" {
		s.operatorSet, s.operatorHash = true, sha256.Sum256([]byte(opts.OperatorToken))
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
	mux.HandleFunc("POST /legion/v1/claims/exit", s.exit)
	mux.HandleFunc("POST /legion/v1/grants", s.grant)
	mux.HandleFunc("POST /legion/v1/controller/secret", s.controllerSecret)
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
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/deliver", s.operator(s.claimRequest("deliver", deliverEvent)))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/suspend", s.operator(s.claimRequest("suspend", suspendEvent)))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/resume", s.operator(s.claimRequest("resume", resumeEvent)))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/stop", s.operator(s.claimRequest("stop", stopEvent)))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/close", s.operator(s.closeTree))

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
	writeJSON(w, http.StatusOK, state)
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
