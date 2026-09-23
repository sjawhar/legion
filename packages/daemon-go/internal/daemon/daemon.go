// Package daemon runs one legion: it opens the store its configuration names, brings the schema
// forward, records the boot, supervises every role claim — the worker stream the shims dial, the
// runtime their processes run under, one machine per claim — serves the API, and stops in the
// order the durable record needs.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const (
	// bootTimeout bounds the work between the plugin gate passing and the API listening: an
	// unreachable Postgres refuses in milliseconds, but a reachable one that never answers must
	// not leave the daemon hanging with nothing on stderr. The gate itself is not bounded by it —
	// it waits out host load for as long as that lasts (pluginGate).
	bootTimeout = 30 * time.Second
	// shutdownTimeout bounds each half of the exit — draining the API, then stamping the boot.
	shutdownTimeout = 10 * time.Second
	// streamSocket is the worker stream's unix socket under the state directory: the one address
	// every pane's shim dials under tmux (decision 2 — no configuration key).
	streamSocket = "worker-stream.sock"
	// orphanSweepInterval and orphanGrace are the shipped periodic reconciliation
	// (packages/daemon/src/daemon/index.ts:79 and processes.ts:309): every minute, a Legion
	// process nothing records is ended once it has idled for two.
	orphanSweepInterval = time.Minute
	orphanGrace         = 2 * time.Minute
	// bootOrphanReconcileAttempts bounds the immediate retry before a previously unrecorded
	// launch may be relaunched. An error is not an absent pane: the claim stays queued and the
	// periodic retry owns it until tmux can say the old pane was reaped or absent.
	bootOrphanReconcileAttempts   = 3
	bootOrphanReconcileRetryDelay = 100 * time.Millisecond
)

// overrides are the parts of a daemon a test replaces; the zero value is the real daemon.
type overrides struct {
	// runtime builds the runtime over the worker stream — its connection directory, and the
	// address every pane's shim dials; nil is the tmux runtime.
	runtime func(ctx context.Context, conns runtime.Conns, streamAddress string) (runtime.Runtime, error)
	// clock is the machines' time; nil is the wall clock.
	clock supervise.Clock
	// getenv is the environment the OMP invocation is resolved against; nil is the process's.
	getenv func(string) string
	// environ is the environment provider keys are resolved under (`secrets get`); nil is the
	// process's.
	environ []string
	// orphanSweep is how often orphans are reconciled; zero is orphanSweepInterval.
	orphanSweep time.Duration
	// gate stands in for the plugin gate when runtime is replaced: nil is none, since a replaced
	// runtime launches no Oh My Pi to gate. With the tmux runtime, the gate is always the real one.
	gate func(ctx context.Context) error
	// workflowTokens replaces the GitHub App token manager in a workflow integration test. The
	// production daemon always mints through appauth.New.
	workflowTokens appauth.Tokens
}

// Run is the daemon. It refuses what it cannot run on before it touches anything — the
// configuration first, then the plugin gate, which holds the Oh My Pi plugin every pane will load
// to this daemon's contract — then opens the store (refusing by the host it could not reach),
// migrates, takes its API listener and its worker stream, builds the runtime, records the boot,
// supervises every claim the store holds, serves the API, and blocks until ctx is done — then
// stops supervising, closes the API, stamps the boot's end, and closes the pool, in that order,
// because the stamp needs the pool.
//
// Everything that can refuse comes before the boot record: a recorded boot is a boot that
// served, and a daemon whose port, socket, or tmux another process holds never ran.
//
// ctx decides one thing: how long the daemon serves. The boot record and its stamp are the
// daemon's own bookkeeping and run on a context the shutdown did not cancel, so a signal that
// arrives mid-startup still leaves a recorded, stamped boot rather than a row with no end. A
// signal that arrives while the plugin gate waits ends the daemon there, cleanly: it has served
// nothing and records nothing.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	return run(ctx, cfg, log, overrides{})
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger, o overrides) error {
	if log == nil {
		log = slog.Default()
	}
	plan, err := prepare(cfg, log, o)
	if err != nil {
		return err
	}
	if plan.gate != nil {
		if err := plan.gate(ctx); err != nil {
			if ctx.Err() != nil {
				log.Info("legion daemon stopped before its plugin gate passed", "project", cfg.Project)
				return nil
			}
			return err
		}
	}

	boot, cancelBoot := context.WithTimeout(context.WithoutCancel(ctx), bootTimeout)
	defer cancelBoot()

	st, err := store.Open(boot, cfg.PostgresDSN)
	if err != nil {
		return err
	}
	applied, err := st.Migrate(boot)
	if err != nil {
		st.Close()
		return err
	}
	if cfg.DispatchURL != "" {
		log.Info("legion workflow boot stage", "stage", "store")
	}
	workflow, err := openWorkflow(boot, cfg, st, plan.project, log, o.workflowTokens)
	if err != nil {
		st.Close()
		return err
	}
	if workflow != nil {
		plan.identity = workflow.identity
	}
	rolesDir, err := prompts.ResolveRolePromptsDir(os.LookupEnv)
	if err != nil {
		workflow.stop()
		st.Close()
		return fmt.Errorf("resolve role prompts: %w", err)
	}
	plan.prompts, err = prompts.New(rolesDir, cfg.StateDir)
	if err != nil {
		workflow.stop()
		st.Close()
		return fmt.Errorf("construct role prompts: %w", err)
	}
	if workflow != nil {
		log.Info("legion workflow boot stage", "stage", "prompts")
	}
	executable, err := os.Executable()
	if err != nil {
		workflow.stop()
		st.Close()
		return fmt.Errorf("resolve this daemon's executable for the pane legion launcher: %w", err)
	}
	if err := tmux.InstallWorkerBin(cfg.StateDir, executable); err != nil {
		workflow.stop()
		st.Close()
		return err
	}
	if workflow != nil {
		log.Info("legion workflow boot stage", "stage", "worker-bin")
	}
	if workflow != nil {
		if err := workflow.bind(cfg); err != nil {
			workflow.stop()
			st.Close()
			return err
		}
		log.Info("legion workflow boot stage", "stage", "dispatch")
	}

	address := net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.Port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		workflow.stop()
		st.Close()
		return fmt.Errorf("listen on %s: %w", address, err)
	}

	s, err := openSupervision(boot, cfg, log, plan, st)
	if err != nil {
		listener.Close()
		workflow.stop()
		st.Close()
		return err
	}
	if workflow != nil {
		if err := workflow.reconcile(boot); err != nil {
			s.stop()
			listener.Close()
			workflow.stop()
			st.Close()
			return err
		}
		log.Info("legion workflow boot stage", "stage", "admission")
		if err := workflow.connect(boot, cfg); err != nil {
			s.stop()
			listener.Close()
			workflow.stop()
			st.Close()
			return err
		}
		workflow.attach(s)
	}

	startedAt := time.Now().UTC()
	bootID, err := st.RecordBoot(boot, cfg.Project, startedAt)
	if err != nil {
		s.stop()
		listener.Close()
		workflow.stop()
		st.Close()
		return err
	}
	superviseErr := s.start(boot)
	if superviseErr == nil && workflow != nil {
		superviseErr = workflow.replayTerminal(boot, s.claims)
		if superviseErr == nil {
			workflow.start(ctx, cfg)
		}
	}
	log.Info("legion daemon started",
		"project", cfg.Project,
		"address", address,
		"workerStream", s.stream.Addr(),
		"runtime", cfg.Runtime.Name,
		"admissionCap", cfg.AdmissionCap,
		"migrationsApplied", applied,
		"claims", s.supervisor.count(),
		"boot", bootID,
	)

	var serveErr error
	if superviseErr == nil {
		if workflow != nil {
			log.Info("legion workflow boot stage", "stage", "api")
		}
		serveErr = serve(ctx, cfg, st, startedAt, listener, s, plan, workflow)
	} else {
		listener.Close()
	}
	s.stop()
	workflow.stop()

	stop, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancelStop()
	stopErr := st.StopBoot(stop, bootID, time.Now().UTC())
	st.Close()
	log.Info("legion daemon stopped", "project", cfg.Project, "boot", bootID)

	return errors.Join(superviseErr, serveErr, stopErr)
}

// plan is what the daemon resolved from its configuration before touching anything.
type plan struct {
	// identity is the role's App bot identity, from the workflow's token source; nil without one.
	identity      func(ctx context.Context, role claim.Role) (runtime.GitIdentity, error)
	project       string
	operatorToken string
	secrets       map[string]string
	instructions  string
	// dispatchTokenFile is the daemon-held Dispatch bearer every pane reads as DISPATCH_TOKEN_FILE.
	dispatchTokenFile string
	prompts           *prompts.Composer
	newRuntime        func(ctx context.Context, conns runtime.Conns, streamAddress string) (runtime.Runtime, error)
	// gate is the plugin gate run before anything is opened (pluginGate); nil only for a replaced
	// runtime without one.
	gate        func(ctx context.Context) error
	clock       supervise.Clock
	orphanSweep time.Duration
}

// prepare is every refusal that needs nothing but the configuration and the machine: the runtime
// this stage supervises under, the operator bearer the spawn surface authenticates against, the
// Envoy bearer every pane is handed, the OMP invocation every pane runs and the plugin gate on
// it, the operator's deployment instructions, written where every pane's prompt reads them, and
// the provider keys, resolved from secretsd into the files every pane's shim reads — all before
// the gate runs and before any pane can launch.
func prepare(cfg config.Config, log *slog.Logger, o overrides) (plan, error) {
	if cfg.Runtime.Name != "tmux" {
		return plan{}, fmt.Errorf("runtime %s: the Go daemon supervises its agents under tmux until Stage 4 models the Sandbox runtime", cfg.Runtime.Name)
	}
	project, err := claim.ProjectToken(cfg.Project)
	if err != nil {
		return plan{}, err
	}
	if cfg.OperatorTokenFile == "" {
		return plan{}, errors.New("operator_token_file is required: the operator routes that spawn and drive claims authenticate against the bearer it names")
	}
	operatorToken, err := config.ReadSecretPointer("operator_token_file", cfg.OperatorTokenFile)
	if err != nil {
		return plan{}, err
	}
	secrets := map[string]string{}
	if cfg.EnvoyTokenFile != "" {
		envoyToken, err := config.ReadSecretPointer("envoy_token_file", cfg.EnvoyTokenFile)
		if err != nil {
			return plan{}, err
		}
		secrets["ENVOY_TOKEN"] = envoyToken
	}

	newRuntime, gate := o.runtime, o.gate
	invocation := ""
	if newRuntime == nil {
		getenv := o.getenv
		if getenv == nil {
			getenv = os.Getenv
		}
		if invocation, err = tmux.ResolveOmpInvocation(cfg.OmpInvocation, getenv); err != nil {
			return plan{}, err
		}
		gate = pluginGate{
			env:        tmux.PaneEnvironment(os.Environ(), cfg.StateDir),
			workDir:    cfg.StateDir,
			invocation: invocation,
			prefix:     cfg.OmpLaunchPrefix,
			timeout:    cfg.SlowCommandTimeout,
			retry:      daemonProbeRetry,
			log:        log,
		}.verify
		log.Info("legion daemon resolved OMP invocation for boot probes and panes", "invocation", invocation)
	}

	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return plan{}, fmt.Errorf("create state directory %s: %w", cfg.StateDir, err)
	}
	dispatchTokenFile := ""
	if cfg.DispatchURL != "" {
		if cfg.DispatchTokenFile == "" {
			return plan{}, errors.New("dispatch_token_file is required when dispatch_url is configured")
		}
		token, err := config.ReadSecretPointer("dispatch_token_file", cfg.DispatchTokenFile)
		if err != nil {
			return plan{}, err
		}
		if dispatchTokenFile, err = tmux.WriteDispatchTokenFile(cfg.StateDir, token); err != nil {
			return plan{}, fmt.Errorf("write the pane Dispatch token file: %w", err)
		}
	}

	instructions := ""
	if cfg.InstructionsPath != "" {
		if instructions, err = config.MaterializeDeploymentInstructions(cfg.InstructionsPath, cfg.StateDir, cfg.Project); err != nil {
			return plan{}, err
		}
	}
	// Last of the refusals: resolving a human-tier key may cost a YubiKey tap, which a
	// configuration refused a line earlier should never have asked for.
	environ := o.environ
	if environ == nil {
		environ = os.Environ()
	}
	providerEnvDir, err := config.MaterializeProviderKeys(cfg.ProviderKeys, cfg.StateDir, environ, log)
	if err != nil {
		return plan{}, err
	}
	if newRuntime == nil {
		newRuntime = tmuxRuntime(cfg, project, invocation, providerEnvDir, dispatchTokenFile, log)
	}

	clock := o.clock
	if clock == nil {
		clock = supervise.RealClock{}
	}
	orphanSweep := o.orphanSweep
	if orphanSweep == 0 {
		orphanSweep = orphanSweepInterval
	}
	return plan{
		project:           project,
		operatorToken:     operatorToken,
		secrets:           secrets,
		instructions:      instructions,
		dispatchTokenFile: dispatchTokenFile,
		newRuntime:        newRuntime,
		gate:              gate,
		clock:             clock,
		orphanSweep:       orphanSweep,
	}, nil
}

// tmuxRuntime builds the tmux runtime over the worker stream: the listener is its connection
// directory, and the listener's address is the `--connect` every pane's shim is started with;
// providerEnvDir, when set, is the `--provider-env-dir` beside it. The private server's
// environment is scrubbed before anything is launched on it.
func tmuxRuntime(cfg config.Config, project, invocation, providerEnvDir, dispatchTokenFile string, log *slog.Logger) func(context.Context, runtime.Conns, string) (runtime.Runtime, error) {
	return func(ctx context.Context, conns runtime.Conns, streamAddress string) (runtime.Runtime, error) {
		rt, err := tmux.New(tmux.Options{
			Project:           project,
			StateDir:          cfg.StateDir,
			StreamAddress:     streamAddress,
			DaemonURL:         cfg.DaemonURL,
			EnvoyURL:          cfg.EnvoyURL,
			NatsURLs:          cfg.NatsURLs,
			DispatchURL:       cfg.DispatchURL,
			DispatchTokenFile: dispatchTokenFile,
			OmpInvocation:     invocation,
			OmpLaunchPrefix:   cfg.OmpLaunchPrefix,
			StopGrace:         cfg.WorkerStopTimeout,
			ProbeInterval:     cfg.ProbeInterval,
			AdoptTimeout:      cfg.SlowCommandTimeout,
			ProviderEnvDir:    providerEnvDir,
			Conns:             conns,
			Log:               log,
		})
		if err != nil {
			return nil, err
		}
		removed, err := rt.ScrubServerEnvironment(ctx)
		if err != nil {
			return nil, fmt.Errorf("scrub the private tmux server's environment: %w", err)
		}
		if len(removed) > 0 {
			log.Info("tmux runtime: removed variables the pane environment does not carry", "removed", removed)
		}
		return rt, nil
	}
}

// supervision is everything that runs a claim: the worker stream, the runtime, and the machines,
// with the goroutines that feed them.
type supervision struct {
	cfg        config.Config
	log        *slog.Logger
	plan       plan
	stream     *stream.Listener
	runtime    runtime.Runtime
	supervisor *supervisor
	tokens     *api.BootTokens
	claims     []supervise.Claim

	cancel       context.CancelFunc
	cancelStream context.CancelFunc
	wg           sync.WaitGroup
	stopOnce     sync.Once
}

// openSupervision reads the claims the store holds, takes the worker stream socket, and builds
// the runtime over it: every step of supervision that can refuse, so a daemon that cannot
// supervise refuses before its boot is recorded.
func openSupervision(boot context.Context, cfg config.Config, log *slog.Logger, p plan, st *store.Store) (*supervision, error) {
	claims, err := st.Claims(boot)
	if err != nil {
		return nil, err
	}
	claims = ofProject(claims, p.project)

	supervising, cancel := context.WithCancel(context.Background())
	streaming, cancelStream := context.WithCancel(context.Background())
	tokens := api.NewBootTokens(st)
	sup := newSupervisor(supervising, st, p.project, cfg.StateDir, log)
	listener, err := stream.Listen(streaming, "unix://"+filepath.Join(cfg.StateDir, streamSocket),
		sup.helloResolver(tokens, cfg.WorkerRPCTimeout), stream.Options{RPCTimeout: cfg.WorkerRPCTimeout, Log: log})
	if err != nil {
		cancel()
		cancelStream()
		return nil, err
	}
	rt, err := p.newRuntime(boot, listener, listener.Addr())
	if err != nil {
		cancel()
		cancelStream()
		return nil, fmt.Errorf("build the %s runtime: %w", cfg.Runtime.Name, err)
	}
	repo := ""
	if configured, ok := cfg.Projects[cfg.Project]; ok {
		repo = configured.Repo
	}

	sup.deps = supervise.Deps{
		Runtime: rt,
		Conns:   listener,
		Store:   pruning(tokens.Recording(st), filepath.Join(cfg.StateDir, secretsDir), log),
		Specs: specs{
			stateDir: cfg.StateDir, project: p.project, instructions: p.instructions, secrets: p.secrets, repo: repo, prompts: p.prompts,
			identity: p.identity,
		},
		Identity: p.identity,
		Clock:    p.clock,
		Log:      log,
		Limits: supervise.Limits{
			LaunchFailures: cfg.LaunchFailureLimit,
			PromptFailures: cfg.PromptFailureLimit,
			PromptRetires:  cfg.PromptRetireLimit,
		},
		Timeouts: supervise.Timeouts{
			Boot:                  cfg.WorkerBootTimeout,
			RegistrationIntervals: cfg.WorkerBootRegistrationDeadlineIntervals,
			RPC:                   cfg.WorkerRPCTimeout,
			Probe:                 cfg.ProbeInterval,
			StopGrace:             cfg.WorkerStopTimeout,
		},
	}
	return &supervision{
		cfg: cfg, log: log, plan: p, stream: listener, runtime: rt, supervisor: sup, tokens: tokens, claims: claims,
		cancel: cancel, cancelStream: cancelStream,
	}, nil
}

// start supervises every claim the store holds. A claim with a live locator is re-adopted — its
// process told to the runtime and observed from now on, never launched again. An unrecorded launch
// is relaunched only after boot orphan reconciliation proves any pre-crash pane absent or reaped;
// a failed listing leaves it queued and uncertain until the bounded retry succeeds. Only then are
// hellos resolved: a shim reconnecting across the restart is admitted by a claim already supervised.
func (s *supervision) start(boot context.Context) error {
	unfinished, err := s.supervisor.restore(boot, s.claims)
	if err != nil {
		return err
	}
	pruneAllBut(filepath.Join(s.cfg.StateDir, secretsDir), s.claims, s.log)
	if s.reconcileBootOrphans(boot) {
		s.launchUnfinished(unfinished)
	} else if len(unfinished) > 0 {
		for _, token := range unfinished {
			s.log.Warn("supervise: unrecorded launch remains uncertain; not relaunching", "claim", token)
		}
		s.retryUnfinished(unfinished)
	}
	close(s.supervisor.restored)

	observations, err := s.runtime.Observe(s.supervisor.ctx)
	if err != nil {
		return fmt.Errorf("observe the runtime: %w", err)
	}
	s.wg.Add(3)
	go func() {
		defer s.wg.Done()
		for observation := range observations {
			s.supervisor.post(observation.Locator.Claim, supervise.RuntimeObservation{Observation: observation})
		}
	}()
	go func() {
		defer s.wg.Done()
		for ev := range s.stream.Events() {
			mapped, err := superviseEvent(ev)
			if err != nil {
				s.log.Error("worker stream: an event no machine can take", "error", err)
				continue
			}
			s.supervisor.post(claimOf(mapped), mapped)
		}
	}()
	go func() {
		defer s.wg.Done()
		s.reconcileOrphans(s.supervisor.ctx)
	}()
	return nil
}

// reconcileBootOrphans retries only the boot reconciliation, boundedly. A listing error does not
// prove an unrecorded launch's pane is gone, so callers must not launch the claim again until this
// returns true.
func (s *supervision) reconcileBootOrphans(ctx context.Context) bool {
	for attempt := 1; attempt <= bootOrphanReconcileAttempts; attempt++ {
		err := s.runtime.ReconcileOrphans(ctx, liveLocators(s.claims), 0)
		if err == nil {
			return true
		}
		if attempt == bootOrphanReconcileAttempts {
			s.log.Warn("reconcile orphans at boot left panes uncertain", "attempts", attempt, "error", err)
			return false
		}
		s.log.Warn("reconcile orphans at boot failed; retrying", "attempt", attempt, "error", err)
		timer := time.NewTimer(bootOrphanReconcileRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	return false
}

// launchUnfinished starts only claims that remain queued with no locator. A still-live old pane can
// reconnect while reconciliation was uncertain; its hello changes the state before a later retry,
// and it must never be joined by a second pane.
func (s *supervision) launchUnfinished(tokens []claim.Token) {
	for _, token := range tokens {
		m, ok := s.supervisor.Machine(token)
		if !ok {
			continue
		}
		released, err := m.ReleaseUncertainLaunch(s.supervisor.ctx)
		if err != nil {
			s.log.Error("supervise: release an uncertain launch", "claim", token, "error", err)
			continue
		}
		if !released {
			c := m.Claim()
			s.log.Info("supervise: unrecorded launch settled without relaunch", "claim", token, "state", c.State)
			continue
		}
		c := m.Claim()
		if c.State != supervise.StateQueued || c.Locator != nil {
			s.log.Info("supervise: unrecorded launch settled without relaunch", "claim", token, "state", c.State)
			continue
		}
		s.log.Warn("supervise: launching again a launch the previous daemon did not finish", "claim", token)
		if err := m.Handle(s.supervisor.ctx, supervise.RequestSpawn{Claim: token}); err != nil {
			s.log.Error("supervise: launch an unfinished launch again", "claim", token, "error", err)
		}
	}
}

// retryUnfinished retries a previously uncertain boot reconciliation on the normal orphan-sweep
// cadence. Each reconciliation itself has the bounded retry above; only a success releases these
// claims to launch.
func (s *supervision) retryUnfinished(tokens []claim.Token) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.plan.orphanSweep)
		defer ticker.Stop()
		for {
			select {
			case <-s.supervisor.ctx.Done():
				return
			case <-ticker.C:
			}
			if !s.reconcileBootOrphans(s.supervisor.ctx) {
				continue
			}
			s.launchUnfinished(tokens)
			return
		}
	}()
}

// reconcileOrphans ends, every sweep interval, the Legion processes on the runtime that no claim
// records and that have idled past the grace.
func (s *supervision) reconcileOrphans(ctx context.Context) {
	ticker := time.NewTicker(s.plan.orphanSweep)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		claims, err := s.supervisor.Claims(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Error("reconcile orphans: read the claims", "error", err)
			}
			continue
		}
		if err := s.runtime.ReconcileOrphans(ctx, liveLocators(claims), orphanGrace); err != nil && ctx.Err() == nil {
			s.log.Error("reconcile orphans", "error", err)
		}
	}
}

// stop ends supervision without ending a single agent: no machine is fed another event, the
// machines' own work is cancelled, the worker stream closes every connection — each shim
// reconnects to the next daemon on its own — and every send already out has its outcome handled
// before the store it writes to closes. The panes keep running; the next boot re-adopts them.
func (s *supervision) stop() {
	s.stopOnce.Do(func() {
		s.supervisor.stop()
		s.cancel()
		s.cancelStream()
		s.supervisor.wait()
		s.wg.Wait()
	})
}

// serve runs the API on the listener the daemon already took until ctx is done or the server
// fails, and returns once it is closed: one goroutine serves, the other shuts down, and the
// shutdown runs on a context of its own so a cancelled ctx still drains the connections it has.
func serve(ctx context.Context, cfg config.Config, st *store.Store, startedAt time.Time, listener net.Listener, s *supervision, p plan, workflow *workflowRuntime) error {
	records := record.Store(record.NewStore())
	var handlers []intake.Handler
	var client dispatch.Client
	var tokens appauth.Tokens
	var grants *credential.Grants
	if workflow != nil {
		records, handlers, client, tokens, grants = workflow.records, workflow.handlers, workflow.dispatch, workflow.tokens, workflow.grants
	}
	server := api.NewServer(cfg.Bind, cfg.Port, api.Options{
		State: &source{
			store:        st,
			records:      records,
			supervisor:   s.supervisor,
			project:      cfg.Project,
			admissionCap: cfg.AdmissionCap,
			startedAt:    startedAt,
		},
		StateTransactions: st,
		Supervisor:        s.supervisor,
		BootTokens:        s.tokens,
		Project:           p.project,
		OperatorToken:     p.operatorToken,
		Log:               s.log,
		Pool:              st.Pool(),
		Handlers:          handlers,
		Record:            records,
		Dispatch:          client,
		Tokens:            tokens,
		GitHubOwner:       githubOwner(cfg),
		Grants:            grants,
	})

	group, serving := errgroup.WithContext(ctx)
	group.Go(func() error {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve the API on %s: %w", server.Addr, err)
		}
		return nil
	})
	group.Go(func() error {
		<-serving.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		return server.Shutdown(shutdown)
	})
	return group.Wait()
}

// source answers the state route out of the daemon's own store: the daemon itself, the cap it
// admits under, and every claim it supervises, filed under the issue it is on.
type source struct {
	store        *store.Store
	records      record.Store
	supervisor   *supervisor
	project      string
	admissionCap int
	startedAt    time.Time
}

// projectRecords scopes the shared daemon database to the daemon's configured project without
// widening record.Store's fixed transaction interface. Project only starts from Issues, so every
// subsequent record read is necessarily within this filtered set.
type projectRecords struct {
	record.Store
	project string
}

func (s projectRecords) Issues(ctx context.Context, tx pgx.Tx) ([]record.Issue, error) {
	all, err := s.Store.Issues(ctx, tx)
	if err != nil {
		return nil, err
	}
	issues := make([]record.Issue, 0, len(all))
	for _, issue := range all {
		if issue.Project == s.project {
			issues = append(issues, issue)
		}
	}
	return issues, nil
}

func (s *source) State(ctx context.Context, tx pgx.Tx) (api.State, error) {
	version, err := s.store.SchemaVersionTx(ctx, tx)
	if err != nil {
		return api.State{}, err
	}
	boots, firstBootAt, err := s.store.BootsTx(ctx, tx, s.project)
	if err != nil {
		return api.State{}, err
	}
	claims, err := s.supervisor.Claims(ctx)
	if err != nil {
		return api.State{}, err
	}
	state, err := projection.Project(ctx, tx, projectRecords{Store: s.records, project: s.project}, claims)
	if err != nil {
		return api.State{}, err
	}
	state.Daemon = api.DaemonInfo{
		Project:       s.project,
		SchemaVersion: version,
		Boots:         boots,
		FirstBootAt:   firstBootAt,
		StartedAt:     s.startedAt,
	}
	state.Admission.Cap = s.admissionCap
	return state, nil
}

// githubOwner is the owner of the configured project's repository: the account both GitHub Apps
// are installed on. A Stage 2 configuration has no repository and so no owner.
func githubOwner(cfg config.Config) string {
	project, ok := cfg.Projects[cfg.Project]
	if !ok {
		return ""
	}
	owner, _, _ := strings.Cut(project.Repo, "/")
	return owner
}
