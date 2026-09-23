package api

import (
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
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
	// Log receives what the routes decide; nil is slog.Default().
	Log *slog.Logger
	// Tokens mints the GitHub App leases credential routes return after redeeming a grant.
	Tokens appauth.Tokens
	// GitHubOwner is the configured repository's owner: the account both Apps are installed on,
	// whose installation every credential route mints for.
	GitHubOwner string
	// Grants mints and redeems the daemon-local one-command credential handles.
	Grants   *credential.Grants
	Pool     *pgxpool.Pool
	Handlers []intake.Handler
	Record   record.Store
	Dispatch dispatch.Client
}

type server struct {
	state             StateSource
	stateTransactions StateTransactions
	supervisor        Supervisor
	bootTokens        *BootTokens
	project           string
	operatorSet       bool
	operatorHash      [sha256.Size]byte
	tokens            appauth.Tokens
	githubOwner       string
	grants            *credential.Grants
	pool              *pgxpool.Pool
	handlers          []intake.Handler
	records           record.Store
	dispatch          dispatch.Client
	log               *slog.Logger
}

// NewServer builds the daemon's HTTP server on bind:port — the configured address only, never
// every interface. The caller owns its lifecycle (ListenAndServe, Shutdown).
//
// Three audiences, three kinds of route: the state everyone reads; the claim lifecycle an agent's
// plugin drives (register, ready, exit), authenticated by its pane's boot token and then by the
// secret its registration was issued; and the operator's spawn surface, authenticated by the
// operator bearer.
func NewServer(bind string, port int, opts Options) *http.Server {
	s := &server{
		state:             opts.State,
		stateTransactions: opts.StateTransactions,
		supervisor:        opts.Supervisor,
		bootTokens:        opts.BootTokens,
		project:           opts.Project,
		tokens:            opts.Tokens,
		githubOwner:       opts.GitHubOwner,
		grants:            opts.Grants,
		pool:              opts.Pool,
		handlers:          opts.Handlers,
		records:           opts.Record,
		dispatch:          opts.Dispatch,
		log:               opts.Log,
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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /legion/v1/state", s.stateRoute)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("POST /legion/v1/claims/register", s.register)
	mux.HandleFunc("POST /legion/v1/claims/ready", s.ready)
	mux.HandleFunc("POST /legion/v1/claims/exit", s.exit)
	mux.HandleFunc("POST /legion/v1/grants", s.grant)
	mux.HandleFunc("POST /legion/v1/gh-token", s.githubToken)
	mux.HandleFunc("POST /legion/v1/git-credential", s.gitCredential)
	mux.HandleFunc("POST /legion/v1/provisioning-credential", s.provisioningCredential)
	mux.HandleFunc("POST /legion/v1/handoff/complete", s.handoffComplete)
	mux.HandleFunc("POST /legion/v1/issues/status", s.issueStatus)
	mux.HandleFunc("POST /legion/v1/gates/register", s.gateRegister)
	mux.HandleFunc("POST /legion/v1/waves/release", s.waveRelease)
	mux.HandleFunc("POST /legion/v1/phase/backward", s.phaseBackward)
	mux.HandleFunc("POST /legion/v1/phase/retry", s.phaseRetry)
	mux.HandleFunc("POST /legion/v1/signoff", s.signOff)

	mux.HandleFunc("POST /legion/v1/operator/claims", s.operator(s.spawn))
	mux.HandleFunc("GET /legion/v1/operator/claims", s.operator(s.list))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/deliver", s.operator(s.claimRequest("deliver", deliverEvent)))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/suspend", s.operator(s.claimRequest("suspend", suspendEvent)))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/resume", s.operator(s.claimRequest("resume", resumeEvent)))
	mux.HandleFunc("POST /legion/v1/operator/claims/{token}/stop", s.operator(s.claimRequest("stop", stopEvent)))

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
