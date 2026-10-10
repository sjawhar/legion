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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/agentsecrets"
	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/omplaunch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/projection"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/stream"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const (
	// bootTimeout bounds the work between the plugin gate passing and the API listening: an
	// unreachable Postgres refuses in milliseconds, but a reachable one that never answers must
	// not leave the daemon hanging with nothing on stderr. The gate is not bounded by it — it waits
	// out host load for as long as that lasts (pluginGate) — nor are the GitHub App tokens' mint
	// and the image probe, which wait out GitHub's and the cluster's transient trouble within
	// retries of their own (appMintRetry, imageProbeRetry); the work after each has a budget anew.
	bootTimeout = 30 * time.Second
	// stopBudget bounds how long the daemon's stop waits for its own work, from the moment the stop
	// begins (supervision.halt: at the signal, at serve's end, or when supervision fails to start,
	// once the boot is recorded) until the daemon stamps its boot. The halt cancels every decision
	// the daemon makes itself — the machines' own events, the workflow's outbox, the controller
	// keeper — and the boot's own steps, so work that answers to its context ends at once. The API's
	// drain, which starts with the stop, gets the budget's first four fifths (8 s): a decision a
	// route asked for runs on until the drain ends, so an operator's suspension whose process is
	// already exiting is recorded rather than undone at the next boot. The last fifth leaves room
	// for the work the drain's end cuts short to return before the budget runs out. Work still
	// running at the budget's end, a call into a process that does not answer, is logged with each
	// claim still deciding and not waited on: the next boot re-adopts each process a claim records
	// and relaunches each launch left unrecorded. stampTimeout bounds the stamp, and storeCloseTimeout
	// the store's close after it, so the daemon returns at most
	// stopBudget+stampTimeout+storeCloseTimeout (22 s) after its stop begins, inside the 30 seconds a
	// pod is given between SIGTERM and SIGKILL. Before the boot is recorded, a signal ends the daemon
	// through each boot step's own bounds, as Run says.
	stopBudget        = 10 * time.Second
	stampTimeout      = 10 * time.Second
	storeCloseTimeout = 2 * time.Second
	// streamSocket is the worker stream's unix socket under the state directory: the one address
	// every pane's shim dials under tmux (decision 2 — no configuration key).
	streamSocket = "worker-stream.sock"
	// orphanSweepInterval and orphanGrace are the periodic reconciliation: every minute, a Legion
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
	// runtime builds the runtime over the worker stream in place of the configured one; nil is the
	// configured runtime.
	runtime runtimeFactory
	// clock is the machines' time; nil is the wall clock.
	clock supervise.Clock
	// getenv is the environment the OMP invocation is resolved against; nil is the process's.
	getenv func(string) string
	// environ is the environment provider keys are resolved under (`secrets get`) and the NATS
	// nkey seed is read from when the configuration names no file; nil is the process's.
	environ []string
	// orphanSweep is how often orphans are reconciled; zero is orphanSweepInterval.
	orphanSweep time.Duration
	// controllerRetry is the first wait before a failed daemon-launched controller is retried; zero
	// is controllerRetryFirst.
	controllerRetry time.Duration
	// stopBudget is how long the daemon's stop waits for its own work once it begins; zero is
	// stopBudget.
	stopBudget time.Duration
	// gate stands in for the plugin gate when runtime is replaced: nil is none, since a replaced
	// runtime launches no Oh My Pi to gate. With the tmux runtime, the gate is always the real one.
	gate func(ctx context.Context) error
	// probe stands in for the worker image probe when runtime is replaced under kubernetes: nil is
	// none. With the Agent Sandbox runtime, the probe is always the real one.
	probe func(ctx context.Context, rt runtime.Runtime) (bootprobe.ImageReport, error)
	// workflowTokens replaces the GitHub App token manager in a workflow integration test. The
	// production daemon always mints through appauth.New.
	workflowTokens appauth.Tokens
	// githubAPI is the GitHub REST root a workflow integration test points the workflow at (its
	// required-checks reads and its issue branches' creates); empty, in production, is
	// https://api.github.com.
	githubAPI string
	// listen opens the API listener; nil is net.Listen.
	listen func(network, address string) (net.Listener, error)
}

// Run is the daemon. It refuses what it cannot run on before it touches anything — the
// configuration first, then, under tmux, the plugin gate, which holds the Oh My Pi plugin every
// pane will load to this daemon's contract — then opens the store (refusing by the host it could
// not reach), migrates, takes its API listener and its worker stream, builds the runtime (under
// kubernetes, once Agent Sandbox's install check passes), proves the worker image under
// kubernetes, records the boot, supervises every claim the store holds, serves the API, and blocks
// until ctx is done. Then it stops: it says so, cancels the decisions it makes itself, drains the
// API and the decisions its routes asked for, ends the workflow and supervision, stamps the boot's
// end and closes the pool, in that order, because the stamp needs the pool. The waits before the
// stamp share one budget, which a boot that has not finished supervising its claims shares too
// (stopBudget).
//
// Everything that can refuse comes before the boot record: a recorded boot is a boot that
// served, and a daemon whose port, socket, tmux, cluster, or image refused it never ran.
//
// ctx decides one thing: how long the daemon serves. The boot record and its stamp are the
// daemon's own bookkeeping and run on a context the shutdown did not cancel, so a signal that
// arrives mid-startup still leaves a recorded, stamped boot rather than a row with no end. A
// signal that arrives while the plugin gate, the GitHub App tokens' mint, or the image probe waits
// ends the daemon there, cleanly: it has served nothing and records nothing.
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
	if cfg.DispatchURL != "" {
		log.Info("legion workflow boot stage", "stage", "prompts")
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
	// The cluster's refusals run before the store opens, so before any schema write, image probe or
	// reconcile: Agent Sandbox must be installed, and no per-claim Sandbox of the layout before issue
	// pods may remain. The claims' half of that layout fence runs once the store opens, before it
	// migrates.
	if plan.clusterCheck != nil {
		if err := plan.clusterCheck(boot); err != nil {
			return err
		}
	}

	st, err := store.Open(boot, cfg.PostgresDSN)
	if err != nil {
		return err
	}
	if plan.claimsCheck != nil {
		if err := plan.claimsCheck(boot, st); err != nil {
			st.Close()
			return err
		}
	}
	applied, err := st.Migrate(boot)
	if err != nil {
		st.Close()
		return err
	}
	if cfg.DispatchURL != "" {
		log.Info("legion workflow boot stage", "stage", "store")
	}
	// The App mint waits out GitHub's transient failures, which the boot budget does not bound, as
	// it does not bound the plugin gate or the image probe: the work after it has a budget of its own.
	workflow, err := openWorkflow(ctx, cfg, st, plan.project, log, o.workflowTokens, o.githubAPI)
	if err != nil {
		st.Close()
		if ctx.Err() != nil {
			log.Info("legion daemon stopped before its GitHub App tokens were minted", "project", cfg.Project)
			return nil
		}
		return err
	}
	if workflow != nil {
		var cancelAfterMint context.CancelFunc
		boot, cancelAfterMint = context.WithTimeout(context.WithoutCancel(ctx), bootTimeout)
		defer cancelAfterMint()
		plan.identity = workflow.identity
		// Only a daemon with the workflow configured has issue records to read a phase or a tree
		// from; Stage 2's supervision runs on claims alone, where every delivery holds and every
		// tree closes.
		plan.phaseHolds, plan.treeClosable = workflow.phaseHolds, workflow.treeClosable
	}
	if cfg.Runtime.Name == "tmux" {
		executable, err := os.Executable()
		if err != nil {
			workflow.stop()
			st.Close()
			return fmt.Errorf("resolve this daemon's executable for the pane legion launcher: %w", err)
		}
		if err := workerbin.Install(cfg.StateDir, executable); err != nil {
			workflow.stop()
			st.Close()
			return err
		}
		if err := reconfigureCloneCredential(boot, cfg, plan.tools, log); err != nil {
			workflow.stop()
			st.Close()
			return err
		}
		if workflow != nil {
			log.Info("legion workflow boot stage", "stage", "launcher")
		}
	}
	if workflow != nil {
		workflow.bind(cfg.DispatchURL, plan.dispatchToken)
		log.Info("legion workflow boot stage", "stage", "dispatch")
	}

	address := net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.Port))
	listen := net.Listen
	if o.listen != nil {
		listen = o.listen
	}
	listener, err := listen("tcp", address)
	if err != nil {
		workflow.stop()
		st.Close()
		return fmt.Errorf("listen on %s: %w", address, err)
	}

	var apps appauth.Tokens
	if workflow != nil {
		apps = workflow.tokens
	}
	s, err := openSupervision(boot, cfg, log, plan, st, apps)
	if err != nil {
		listener.Close()
		workflow.stop()
		st.Close()
		return err
	}
	if plan.probe != nil {
		report, err := plan.probe(ctx, s.runtime)
		if err != nil {
			s.stop()
			listener.Close()
			workflow.stop()
			st.Close()
			if ctx.Err() != nil {
				log.Info("legion daemon stopped before its worker image passed its probe", "project", cfg.Project)
				return nil
			}
			return err
		}
		s.imageReport, s.probed = report, true
		// The probe waits out a cold node and an image pull, which the boot budget does not bound,
		// as it does not bound the plugin gate: the work after the probe has a budget of its own.
		var cancelAfterProbe context.CancelFunc
		boot, cancelAfterProbe = context.WithTimeout(context.WithoutCancel(ctx), bootTimeout)
		defer cancelAfterProbe()
	} else if plan.modelFallback != nil {
		// The tmux counterpart of the probe's model-fallback mark, read from the host's Oh My Pi
		// under the gate's environment now that the gate has passed.
		s.imageReport.ModelFallback = plan.modelFallback(ctx)
	}
	// The deployment's capability report (LEGION-578, "The check"): a gap is logged here, shown in
	// `legion state` and named to the controller on every tick, and never refused.
	s.reportCapabilities()
	if workflow != nil {
		workflow.admission.ReportCapabilities(s.reportCapabilities)
		// The durable consumers exist before the listing is read: a consumer created now delivers
		// only what is published after it, so everything earlier is the listing's, and what the
		// listing misses (a move published while it is read) the consumer delivers. Both wait out
		// an unreachable dependency on ctx, not the bounded boot budget just spent on everything
		// before them, through the same bootprobe.Run mechanism mintAtBoot already uses, here with
		// Attempts left at its unbounded zero. natsauth.Unreachable and dispatch.Unreachable each
		// judge their own dependency; their own docs are the record of what each one waits on and
		// what it still refuses loud. reconcile's own Postgres transaction is judged by neither: a
		// design choice, not an inability to tell its failures apart from NATS's or Dispatch's — an
		// unreachable Postgres refuses earlier, at store.Open
		// (TestRunRefusesAnUnreachablePostgresByHostAndNotByPassword, scripts/e2e/stage1-skeleton.sh).
		plan.nats.log(log)
		err := bootprobe.Run(ctx, "connect Envoy NATS", readinessRetry, log,
			readinessAttempt(func(attempt context.Context) error {
				return workflow.connect(attempt, cfg, plan.nats)
			}, natsauth.Unreachable))
		if err == nil {
			err = bootprobe.Run(ctx, "list Dispatch issues for admission", readinessRetry, log,
				readinessAttempt(workflow.reconcile, dispatch.Unreachable))
		}
		if err != nil {
			s.stop()
			listener.Close()
			workflow.stop()
			st.Close()
			if ctx.Err() != nil {
				log.Info("legion daemon stopped before its workflow dependencies were reachable", "project", cfg.Project)
				return nil
			}
			return err
		}
		log.Info("legion workflow boot stage", "stage", "admission")
		workflow.attach(s)
		// Both waited out whatever they waited out on ctx, which the boot budget does not bound:
		// what follows gets a budget of its own, as the App mint and the image probe's callers
		// already do.
		var cancelAfterReady context.CancelFunc
		boot, cancelAfterReady = context.WithTimeout(context.WithoutCancel(ctx), bootTimeout)
		defer cancelAfterReady()
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
	// From here every way the daemon ends halts supervision before anything waits: the signal, at
	// once, serve's end, or a start that failed. The signal also ends the boot's own steps, which run
	// on starting. Everything after the boot record runs on a goroutine of its own, so run waits for
	// it only until the stop's budget runs out (awaitStop), and then stamps the boot whatever is
	// still running.
	starting, endStarting := context.WithCancel(boot)
	defer endStarting()
	halting := context.AfterFunc(ctx, func() {
		s.halt(context.Cause(ctx))
		endStarting()
	})
	defer halting()
	failed := make(chan error, 1)
	lived := make(chan error, 1)
	go func() {
		err := s.start(starting)
		if err == nil && workflow != nil {
			err = workflow.replayTerminal(starting, s.claims)
		}
		switch {
		case ctx.Err() != nil:
			// The signal came while the daemon booted, and cut its own steps short or would have: the
			// daemon is stopping, not failing to start, and it never started.
			log.Info("legion daemon stopped its boot short", "project", cfg.Project, "boot", bootID, "error", err)
			err = nil
			listener.Close()
		case err == nil:
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
			if workflow != nil {
				log.Info("legion workflow boot stage", "stage", "api")
			}
			err = serve(ctx, cfg, st, startedAt, listener, s, plan, workflow)
		default:
			failed <- err
			s.halt(err)
			listener.Close()
		}
		s.stop()
		workflow.stop()
		lived <- err
	}()
	liveErr := s.awaitStop(lived, failed, bootID)

	// The stop cancelled store queries in flight, and a pooled connection one of them ran on can
	// still carry the read deadline that cancellation set, which failed the stamp with `stop boot N:
	// timeout: read tcp …: i/o timeout` (a socket deadline, not the stamp's context). The stamp runs
	// on a connection opened after the reset.
	st.Pool().Reset()
	stamp, cancelStamp := context.WithTimeout(context.WithoutCancel(ctx), stampTimeout)
	defer cancelStamp()
	stopErr := st.StopBoot(stamp, bootID, time.Now().UTC())
	closeStore(st, log)
	log.Info("legion daemon stopped", "project", cfg.Project, "boot", bootID)

	return errors.Join(liveErr, stopErr)
}

// awaitStop waits, once the stop has begun (stopping), for the daemon's own work to end and report
// on lived, until the stop's budget runs out (stopBy). Then it logs each claim still deciding and
// leaves it to the next boot; a start that failed, reported on failed before the stop began, is
// returned either way.
func (s *supervision) awaitStop(lived, failed <-chan error, bootID int64) error {
	<-s.stopping.Done()
	deadline := time.NewTimer(time.Until(s.stopBy))
	defer deadline.Stop()
	select {
	case err := <-lived:
		return err
	case <-deadline.C:
	}
	s.log.Warn("legion daemon stopped waiting for its work", "project", s.cfg.Project, "boot", bootID,
		"after", s.plan.stopBudget.String(), "deciding", s.supervisor.inDecision())
	select {
	case err := <-failed:
		return err
	default:
		return nil
	}
}

// closeStore closes the store, waiting for it up to storeCloseTimeout: the pool's close waits for
// every connection a query holds, and a query the stop stopped waiting for may be waiting on
// Postgres.
func closeStore(st *store.Store, log *slog.Logger) {
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		st.Close()
	}()
	timer := time.NewTimer(storeCloseTimeout)
	defer timer.Stop()
	select {
	case <-closed:
	case <-timer.C:
		log.Warn("legion daemon stopped waiting for its store to close", "after", storeCloseTimeout.String())
	}
}

// plan is what the daemon resolved from its configuration before touching anything.
type plan struct {
	// identity is the role's App bot identity, from the workflow's token source; nil without one.
	identity func(ctx context.Context, role claim.Role) (runtime.GitIdentity, error)
	// phaseHolds and treeClosable are the workflow's answers to the supervisor's two predicates;
	// nil without a workflow, where every delivery holds and every tree closes.
	phaseHolds   func(ctx context.Context, issue string, p phase.Phase) (bool, error)
	treeClosable func(ctx context.Context, c supervise.Claim) (bool, error)
	// tools are the git and jj boot resolved on the host, by name, for workspace provisioning;
	// nil without a repository, and under a runtime whose agents run the worker image's own.
	tools         map[string]string
	project       string
	operatorToken string
	secrets       map[string]string
	// nats is the user the daemon's own NATS connection authenticates as (natsConnection); no launch
	// carries its seed unless it is the pane seed.
	nats         natsConnection
	instructions string
	// dispatchToken is the Dispatch bearer dispatch_token_file names; "" without Dispatch.
	dispatchToken string
	prompts       *prompts.Composer
	// roleReferences are the task agents and skills the shared role prompts name
	// (prompts.RoleReferences), which the gate on either runtime resolves beside the plugin's own.
	roleReferences promptrefs.Names
	// stream is the worker stream's address: the listener binds it, and every agent's shim dials
	// it, or advertise_host at its port when the file sets one (shimAddress).
	stream     string
	newRuntime runtimeFactory
	// gate is the plugin gate run before anything is opened (pluginGate); nil under a runtime with
	// no host Oh My Pi, and for a replaced runtime without one.
	gate func(ctx context.Context) error
	// probe proves the runtime's worker image once the runtime is built and before the boot is
	// recorded, answering what its OK line reported of the image; nil under tmux, and for a
	// replaced runtime without one.
	probe func(ctx context.Context, rt runtime.Runtime) (bootprobe.ImageReport, error)
	// clusterCheck is the Kubernetes runtime's refusals before the store opens: Agent Sandbox's
	// install check, then the census of per-claim Sandboxes (sandbox.CensusLegacyIssueSandboxes).
	// Nil under tmux, and for a replaced runtime.
	clusterCheck func(ctx context.Context) error
	// claimsCheck is the Kubernetes runtime's refusal once the store has opened, before it
	// migrates: no stored claim may still carry a per-claim Sandbox locator of the layout before
	// issue pods (store.HasLegacySandboxClaims). Nil under tmux, and for a replaced runtime.
	claimsCheck func(ctx context.Context, st *store.Store) error
	// modelFallback reads, once the gate has passed, whether the host's Oh My Pi falls back to
	// another model (capabilities.ReadModelFallback), as the probe's OK line reports it for a pod:
	// "on", "off", or "" when the read failed, which it logs and never refuses. nil under
	// kubernetes, whose probe reports it, and for a replaced runtime.
	modelFallback func(ctx context.Context) string
	clock         supervise.Clock
	orphanSweep   time.Duration
	// controllerRetry is the controller keeper's first wait before it retries a failed controller.
	controllerRetry time.Duration
	// stopBudget is how long the stop waits for the daemon's own work once it begins (stopBudget).
	stopBudget time.Duration
	// secretsEnroller is the daemon's agent-secrets machine login as the machines' Enroller
	// (newSecretsLogin); nil when the deployment enrolls no pod.
	secretsEnroller supervise.Enroller
	// secretsLogin is the same machine login's client, read-only, for the state route to show its
	// current status (source.State, agentsecrets.Client.LoginStatus); nil when the deployment
	// enrolls no pod.
	secretsLogin *agentsecrets.Client
}

// runtimeFactory builds the runtime over the worker stream (C3): ctx is supervision's lifetime,
// listener the stream listener, address the one every agent's shim dials (shimAddress), tokens the
// workflow's App tokens (nil without a workflow), st the store the runtime reads, and removable the
// tree's candidate function (removableWorkspaces), which needs sup — created before this is called
// (openSupervision) — so it cannot be built inside the factory itself; a runtime that does not
// provision workspaces in its own pods ignores it.
type runtimeFactory func(ctx context.Context, listener *stream.Listener, address string, tokens appauth.Tokens, st *store.Store, removable func(ctx context.Context, tree, exclude string) ([]runtime.RemovableWorkspace, error)) (runtime.Runtime, error)

// prepare is every refusal that needs nothing but the configuration and the machine (readBoot's,
// then what writes or runs something: the state directory, the instructions copy, and what the
// runtime needs) — all before the plugin gate runs and before any agent can launch.
func prepare(cfg config.Config, log *slog.Logger, o overrides) (plan, error) {
	getenv := o.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	// A test that replaced the runtime runs no host Oh My Pi, so boot resolves no OMP invocation.
	hostOMP := o.runtime == nil
	reads, err := readBoot(cfg, environLookup(o.environment()), getenv, hostOMP, log)
	if err != nil {
		return plan{}, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return plan{}, fmt.Errorf("create state directory %s: %w", cfg.StateDir, err)
	}
	composer, err := prompts.New(cfg.StateDir)
	if err != nil {
		return plan{}, fmt.Errorf("construct role prompts: %w", err)
	}

	instructions := ""
	if reads.instructions != nil {
		if instructions, err = config.WriteDeploymentInstructions(reads.instructions, cfg.StateDir, cfg.Project); err != nil {
			return plan{}, err
		}
	}

	clock := o.clock
	if clock == nil {
		clock = supervise.RealClock{}
	}
	orphanSweep := o.orphanSweep
	if orphanSweep == 0 {
		orphanSweep = orphanSweepInterval
	}
	controllerRetry := o.controllerRetry
	if controllerRetry == 0 {
		controllerRetry = controllerRetryFirst
	}
	stop := o.stopBudget
	if stop == 0 {
		stop = stopBudget
	}
	secretsEnroller, secretsLogin := newSecretsLogin(cfg, log)
	p := plan{
		project: reads.project, operatorToken: reads.operatorToken, secrets: reads.secrets, nats: reads.nats, instructions: instructions,
		dispatchToken: reads.dispatchToken, prompts: composer, roleReferences: prompts.RoleReferences(),
		tools: reads.tmux.tools, clock: clock, orphanSweep: orphanSweep, controllerRetry: controllerRetry, stopBudget: stop,
		secretsEnroller: secretsEnroller, secretsLogin: secretsLogin,
	}
	if cfg.Runtime.Name == "kubernetes" {
		err = prepareSandbox(cfg, o, reads.sandbox, &p)
	} else {
		err = prepareTmux(cfg, log, o, reads.dispatchToken, reads.tmux.invocation, &p)
	}
	if err != nil {
		return plan{}, err
	}
	return p, nil
}

// prepareTmux is what panes on this host need beyond readBoot's: the plugin gate on the OMP
// invocation readBoot resolved, the Dispatch bearer written where every pane reads it, and the
// provider keys resolved from secretsd into the files every pane's shim reads. The worker stream is
// a unix socket under the state directory (decision 2 — no configuration key).
func prepareTmux(cfg config.Config, log *slog.Logger, o overrides, dispatchToken, invocation string, p *plan) error {
	p.newRuntime, p.gate = o.runtime, o.gate
	p.stream = "unix://" + filepath.Join(cfg.StateDir, streamSocket)
	if p.newRuntime == nil {
		log.Info("legion daemon resolved OMP invocation for boot probes and panes", "invocation", invocation)
	}
	dispatchTokenFile := ""
	if dispatchToken != "" {
		var err error
		if dispatchTokenFile, err = runtime.WriteDispatchTokenFile(cfg.StateDir, dispatchToken); err != nil {
			return fmt.Errorf("write the pane Dispatch token file: %w", err)
		}
	}
	// Last of the refusals: resolving a human-tier key may cost a YubiKey tap, which a
	// configuration refused a line earlier should never have asked for.
	providerEnvDir, err := config.MaterializeProviderKeys(cfg.ProviderKeys, cfg.StateDir, o.environment(), log)
	if err != nil {
		return err
	}
	if p.newRuntime == nil {
		env, err := gateEnvironment(os.Environ(), cfg.StateDir, providerEnvDir)
		if err != nil {
			return err
		}
		p.gate = pluginGate{
			env:            env,
			workDir:        cfg.StateDir,
			invocation:     invocation,
			prefix:         cfg.OmpLaunchPrefix,
			timeout:        cfg.SlowCommandTimeout,
			retry:          bootprobe.Daemon,
			contract:       api.DaemonAPIVersion,
			roleReferences: p.roleReferences,
			log:            log,
		}.verify
		image := capabilities.Image{Launch: omplaunch.WithPrefix(cfg.OmpLaunchPrefix, invocation), Env: environPairs(env), WorkDir: cfg.StateDir}
		p.modelFallback = func(ctx context.Context) string {
			state, err := capabilities.ReadModelFallback(ctx, image)
			if err != nil {
				log.Warn("the daemon could not read whether the host's Oh My Pi falls back to another model; the model-fallback capability is reported as not read", "error", err)
				return ""
			}
			return state
		}
		p.newRuntime = tmuxRuntime(cfg, p.project, invocation, providerEnvDir, dispatchTokenFile, log)
	}
	return nil
}

// tmuxRuntime builds the tmux runtime over the worker stream: the listener is its connection
// directory, and the listener's address is the `--connect` every pane's shim is started with;
// providerEnvDir, when set, is the `--provider-env-dir` beside it. The workflow's App tokens, when
// the daemon has them, are every tree pane's gh files (gitHubCredential); a daemon with no GitHub
// Apps hands the runtime none, and its panes hold no gh files. The private server's environment is
// scrubbed before anything is launched on it.
func tmuxRuntime(cfg config.Config, project, invocation, providerEnvDir, dispatchTokenFile string, log *slog.Logger) runtimeFactory {
	return func(ctx context.Context, listener *stream.Listener, streamAddress string, tokens appauth.Tokens, _ *store.Store, _ func(ctx context.Context, tree, exclude string) ([]runtime.RemovableWorkspace, error)) (runtime.Runtime, error) {
		opts := tmuxOptions(cfg, project, invocation, providerEnvDir, dispatchTokenFile, log)
		opts.StreamAddress, opts.Conns = streamAddress, listener
		if tokens != nil {
			opts.GitHubCredential = gitHubCredential(tokens, githubOwner(cfg))
		}
		rt, err := tmux.New(opts)
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

// tmuxOptions translates the configuration into the tmux runtime's Options, all but the worker
// stream and the GitHub credential function, which boot hands the factory.
func tmuxOptions(cfg config.Config, project, invocation, providerEnvDir, dispatchTokenFile string, log *slog.Logger) tmux.Options {
	return tmux.Options{
		Project:           project,
		StateDir:          cfg.StateDir,
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
		Log:               log,
	}
}

// reconfigureCloneCredential brings the shared clone an earlier daemon provisioned under the state
// directory to the helper every clone gets now (workspace.ConfigureRepositoryCredential): a clone
// provisioned before each pane read GitHub from its own gh files names that daemon's `legion
// credential` by the pane launcher's path, which answers no pane now, so its first push would
// fail. It runs the git boot resolved (tools), as the outbox's provisioning does, and logs the one
// clone it rewrote. A configuration with no repository, or a state directory with no clone yet, is
// nothing to do: provisioning writes the helper into a clone it makes.
func reconfigureCloneCredential(ctx context.Context, cfg config.Config, tools map[string]string, log *slog.Logger) error {
	repo := cfg.Projects[cfg.Project].Repo
	if repo.IsZero() || tools == nil {
		return nil
	}
	// Location derives the clone from the repository alone; the issue names only the workspace
	// beside it, which nothing here reads.
	located, err := workspace.Location(cfg.StateDir, repo, cfg.Project)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(located.Clone, ".git")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read the shared clone %s: %w", located.Clone, err)
	}
	changed, err := workspace.ConfigureRepositoryCredential(ctx, workspace.NewRunner(workspace.CommandTimeout, tools), located.Clone)
	if err != nil {
		return fmt.Errorf("set the shared clone's git credential helper: %w", err)
	}
	if changed {
		log.Info("legion daemon set the shared clone's git credential helper", "clone", located.Clone, "helper", workspace.GitHubCredentialHelper)
	}
	return nil
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
	// imageReport is what the worker image's passed probe reported of the image, from its OK
	// line (bootprobe.ImageReport); under tmux, the model-fallback mark alone, read by
	// plan.modelFallback. probed is whether the probe passed (kubernetes), so the image rows of the
	// capability report read present.
	imageReport bootprobe.ImageReport
	probed      bool
	// reportedGaps are the open capabilities the last report logged (reportCapabilities), under
	// reportMu: the tick's reads and boot's run on different goroutines.
	reportMu     sync.Mutex
	reportedGaps []string

	cancel context.CancelFunc
	// draining is the worker stream's life and what the API's routes run their decisions on
	// (api.Options.Drained), one context because they end together: when the API's drain does
	// (endDrain). decided is the claims those routes are deciding (api.RouteDecisions), whose
	// connections the halt keeps open. stopping ends when the stop begins (halt), for awaitStop and
	// the API's routes (api.Options.Stopping); stopBy is when the stop must end by, and drainBy when
	// the API's drain must, both set before.
	draining  context.Context
	endDrain  context.CancelFunc
	decided   *api.RouteDecisions
	stopping  context.Context
	beginStop context.CancelFunc
	stopBy    time.Time
	drainBy   time.Time
	haltOnce  sync.Once
	wg        sync.WaitGroup
	stopOnce  sync.Once
}

// Deployment is the deployment's capabilities as the configuration alone states them
// (capabilities.Deployment): the decisions, the runtime, whether a broker is configured, and the
// roles whose pods reserve no CPU and memory (config.RoleResources.Reserved) — every workflow
// role, and the controller's when the daemon launches it. What boot learns (the broker login, the
// probe, model fallback) is the supervision's to add (supervision.deployment); `legion start
// --check-config` reports from this alone.
func Deployment(cfg config.Config) capabilities.Deployment {
	d := capabilities.Deployment{Decided: cfg.Capabilities.Decided, Runtime: cfg.Runtime.Name}
	k := cfg.Runtime.Kubernetes
	if k == nil {
		return d
	}
	d.AgentSecrets = k.AgentSecrets != nil
	roles := claim.Roles
	if cfg.ControllerLaunch == config.ControllerLaunchDaemon {
		roles = append(slices.Clone(roles), claim.RoleController)
	}
	for _, role := range roles {
		if !k.Resources[role].Reserved() {
			d.RolesWithoutResources = append(d.RolesWithoutResources, role)
		}
	}
	return d
}

// deployment is Deployment with what this boot learned: the broker login's state, whether the
// image passed its probe, and the model-fallback mark the probe or the gate read.
func (s *supervision) deployment() capabilities.Deployment {
	d := Deployment(s.cfg)
	if s.plan.secretsLogin != nil {
		d.SecretsLogin = s.plan.secretsLogin.LoginStatus().State
	}
	d.Probed, d.ModelFallback = s.probed, s.imageReport.ModelFallback
	return d
}

// reportCapabilities names the deployment capabilities with no decision, in the table's order, and
// logs the report whenever that set differs from the one last logged: once at boot, and again from
// a controller tick that finds it changed (admit.Admission.ReportCapabilities asks only when the
// tick queues a wake: a controller is registered and no tick notice is pending) — the broker's
// login reaching issued is the one change a running daemon sees — so a gap is logged at boot and
// at the first tick after a change, never on every tick, and never between ticks.
func (s *supervision) reportCapabilities() []string {
	d := s.deployment()
	open := d.Open()
	names := make([]string, len(open))
	for i, name := range open {
		names[i] = string(name)
	}
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	if !slices.Equal(names, s.reportedGaps) {
		d.Log(s.log)
		s.reportedGaps = names
	}
	return names
}

// shimAddress is the address every agent's shim dials: the listener's bound address, or, when
// advertiseHost (the top-level advertise_host, which only runtime: kubernetes accepts) names one,
// that host at the bound port, the kernel's choice when worker_stream_port was 0. A bound address
// it cannot split into tcp://host:port beside an advertiseHost is refused, never handed to pods.
func shimAddress(bound, advertiseHost string) (string, error) {
	if advertiseHost == "" {
		return bound, nil
	}
	hostport, ok := strings.CutPrefix(bound, "tcp://")
	if !ok {
		return "", fmt.Errorf("advertise_host %s needs a tcp:// worker stream, and the listener bound %s", advertiseHost, bound)
	}
	_, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return "", fmt.Errorf("advertise_host %s: the worker stream listener's address %s: %w", advertiseHost, bound, err)
	}
	return "tcp://" + net.JoinHostPort(advertiseHost, port), nil
}

// openSupervision reads the claims the store holds, takes the worker stream, and builds the
// runtime over it, for supervision's lifetime and with the workflow's App tokens (nil without a
// workflow): every step of supervision that can refuse, so a daemon that cannot supervise refuses
// before its boot is recorded.
func openSupervision(boot context.Context, cfg config.Config, log *slog.Logger, p plan, st *store.Store, apps appauth.Tokens) (*supervision, error) {
	claims, err := st.Claims(boot)
	if err != nil {
		return nil, err
	}
	claims = ofProject(claims, p.project)

	supervising, cancel := context.WithCancel(context.Background())
	draining, endDrain := context.WithCancel(context.Background())
	tokens := api.NewBootTokens(st)
	sup := newSupervisor(supervising, st, p.project, cfg.StateDir, log)
	listener, err := stream.Listen(draining, p.stream,
		sup.helloResolver(tokens, cfg.WorkerRPCTimeout), stream.Options{RPCTimeout: cfg.WorkerRPCTimeout, Log: log})
	if err != nil {
		cancel()
		endDrain()
		return nil, err
	}
	dial, err := shimAddress(listener.Addr(), cfg.AdvertiseHost)
	if err != nil {
		cancel()
		endDrain()
		return nil, err
	}
	rt, err := p.newRuntime(supervising, listener, dial, apps, st, removableWorkspaces(st.Pool(), record.NewStore(), sup))
	if err != nil {
		cancel()
		endDrain()
		return nil, fmt.Errorf("build the %s runtime: %w", cfg.Runtime.Name, err)
	}
	repo := cfg.Projects[cfg.Project].Repo

	sup.deps = supervise.Deps{
		Runtime: rt,
		Conns:   listener,
		Store:   pruning(tokens.Recording(st), runtime.SecretsDir(cfg.StateDir), log),
		Specs: specs{
			stateDir: cfg.StateDir, project: p.project, instructions: p.instructions, secrets: p.secrets, repo: repo, prompts: p.prompts,
			identity: p.identity, designGate: cfg.Gates.Design, reviewWorkflows: cfg.Projects[cfg.Project].ReviewWorkflows,
		},
		Identity:     p.identity,
		Secrets:      p.secretsEnroller,
		PhaseHolds:   p.phaseHolds,
		TreeClosable: p.treeClosable,
		VolumeLost:   sup.volumeLost,
		Clock:        p.clock,
		Log:          log,
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
			Stop:                  cfg.WorkerStopTimeout,
		},
	}
	stopping, beginStop := context.WithCancel(context.Background())
	return &supervision{
		cfg: cfg, log: log, plan: p, stream: listener, runtime: rt, supervisor: sup, tokens: tokens, claims: claims,
		cancel: cancel, draining: draining, endDrain: endDrain, decided: api.NewRouteDecisions(),
		stopping: stopping, beginStop: beginStop,
	}, nil
}

// start supervises every claim the store holds. A claim with a live locator is re-adopted — its
// process told to the runtime and observed from now on, never launched again. An unrecorded launch
// is relaunched only after boot orphan reconciliation proves any pre-crash pane absent or reaped;
// a failed listing leaves it launch_uncertain until the bounded retry succeeds. Only then are
// hellos resolved: a shim reconnecting across the restart is admitted by a claim already supervised.
// A daemon that leaves the controller to its operator first stops the controller's claim an
// earlier boot under `controller: daemon` left (stopLaunchedController), which leaves it absent or
// retired. The only controller claim among the unrecorded launches is a launch a crash cut short,
// and by then the stop has retired it, so launchUnfinished would not relaunch it: its
// ReleaseUncertainLaunch acts only on a launch-uncertain claim. Dropping that token is a second
// guard beside that check, so boot does not report the retired claim as an unrecorded launch. The
// steps before the relaunches run on boot, and the relaunches on the machines' context; the
// daemon's stop ends both (run).
func (s *supervision) start(boot context.Context) error {
	unfinished, err := s.supervisor.restore(boot, s.claims)
	if err != nil {
		return err
	}
	if s.cfg.ControllerLaunch != config.ControllerLaunchDaemon {
		if err := s.stopLaunchedController(boot); err != nil {
			return err
		}
		controller := claim.ControllerToken(s.plan.project)
		unfinished = slices.DeleteFunc(unfinished, func(token claim.Token) bool { return token == controller })
	}
	pruneAllBut(runtime.SecretsDir(s.cfg.StateDir), s.claims, s.log)
	if s.reconcileBootOrphans(boot) {
		if failed := s.launchUnfinished(unfinished); len(failed) > 0 {
			s.retryUnfinished(failed, true)
		}
	} else if len(unfinished) > 0 {
		for _, token := range unfinished {
			s.log.Warn("supervise: unrecorded launch remains uncertain; not relaunching", "claim", token)
		}
		s.retryUnfinished(unfinished, false)
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
// returns true. Each attempt reads the claims as they are now: a later retry must know the claims
// suspended or retired since boot as they are, not as the boot read them.
func (s *supervision) reconcileBootOrphans(ctx context.Context) bool {
	for attempt := 1; attempt <= bootOrphanReconcileAttempts; attempt++ {
		claims, err := s.supervisor.Claims(ctx)
		if err == nil {
			err = s.runtime.ReconcileOrphans(ctx, knownClaims(claims), 0)
		}
		if err == nil {
			return true
		}
		if attempt == bootOrphanReconcileAttempts {
			s.log.Warn("reconcile orphans at boot left panes uncertain", "attempts", attempt, "error", err)
			return false
		}
		if ctx.Err() != nil {
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
// and it must never be joined by a second pane. It returns the claims whose release the store did
// not take, outside the daemon's stop: each is still launch_uncertain, in memory as in the store,
// and nothing else in this boot releases it, so the caller retries it (retryUnfinished).
func (s *supervision) launchUnfinished(tokens []claim.Token) []claim.Token {
	var failed []claim.Token
	for _, token := range tokens {
		m, ok := s.supervisor.Machine(token)
		if !ok {
			continue
		}
		released, err := m.ReleaseUncertainLaunch(s.supervisor.ctx)
		if err != nil {
			if s.supervisor.decisionFailed(token, "release of an uncertain launch", err,
				"supervise: release an uncertain launch; retrying at the next orphan sweep") {
				failed = append(failed, token)
			}
			continue
		}
		c := m.Claim()
		if !released || c.State != supervise.StateQueued || c.Locator != nil {
			s.log.Info("supervise: unrecorded launch settled without relaunch", "claim", token, "state", c.State)
			continue
		}
		s.log.Warn("supervise: launching again a launch the previous daemon did not finish", "claim", token)
		spawn := supervise.RequestSpawn{Claim: token}
		if err := m.Handle(s.supervisor.ctx, spawn); err != nil {
			s.supervisor.decisionFailed(token, fmt.Sprintf("%T", spawn), err, "supervise: launch an unfinished launch again")
		}
	}
	return failed
}

// retryUnfinished launches tokens' unfinished launches on the normal orphan-sweep cadence, until
// none is left or the daemon stops. Until the boot reconciliation has succeeded (reconciled), each
// sweep retries it first — each reconciliation has the bounded retry above, and only a success
// releases these claims to launch; once it has, a sweep retries only the releases the store did not
// take (launchUnfinished), since the reconciliation's proof that no predecessor runs still holds.
func (s *supervision) retryUnfinished(tokens []claim.Token, reconciled bool) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.plan.orphanSweep)
		defer ticker.Stop()
		for len(tokens) > 0 {
			select {
			case <-s.supervisor.ctx.Done():
				return
			case <-ticker.C:
			}
			if !reconciled {
				if reconciled = s.reconcileBootOrphans(s.supervisor.ctx); !reconciled {
					continue
				}
			}
			tokens = s.launchUnfinished(tokens)
		}
	}()
}

// reconcileOrphans ends, every sweep interval, whatever the runtime holds that belongs to none of
// the claims the daemon has not retired, once it has idled past the grace.
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
		if err := s.runtime.ReconcileOrphans(ctx, knownClaims(claims), orphanGrace); err != nil && ctx.Err() == nil {
			s.log.Error("reconcile orphans", "error", err)
		}
	}
}

// halt begins the stop, once. It sets when the stop and the API's drain must end by (stopBy,
// drainBy), says the daemon is stopping, why (cause) and which claims are deciding, and ends
// stopping, which tells awaitStop the stop has begun, and the API's routes too
// (api.Options.Stopping), from when an operator's request that would change a claim is refused.
// Then, in this order:
//
//  1. No machine is fed another event (supervisor.halt).
//  2. The worker stream stops answering a shim's hello and closes every shim connection except
//     those of the claims an API route is deciding (stream.Listener.Narrow), so every other shim
//     keeps what its agent says in its backlog for the next daemon rather than hand it to one that
//     no longer acts on it. A kept connection carries its process's shutdown frame; what else it
//     carries until the drain ends (endDrain) is not acted on, since its claim is being suspended
//     or stopped, or is registering an agent the next boot re-adopts.
//  3. The machines' context is cancelled. Every decision the daemon makes itself runs on it — the
//     machines' own events, the workflow's outbox, the controller keeper, the boot's relaunches —
//     so it ends now rather than when its runtime call's own wait runs out: a Sandbox relaunch
//     waits minutes for the previous pod to go, the tree's other pods to finish workspace-init and
//     the new pod to appear. A decision an API route asked for runs on until the drain ends.
//
// Three orderings are required. The routes learn the stop has begun before the second step, so an
// operator's request that records its claim too late for Narrow to keep its connection finds the
// stop begun and is refused, rather than decided without the connection. The first step comes
// before the second, so the Closed events of the connections Narrow ends reach no machine. The
// second comes before the third, so a hello whose resolve the cancellation fails is already the
// stop's, and closes unrefused. A launch the cancellation cuts short leaves its claim launching
// with no process recorded, which the next boot relaunches once its orphan reconciliation has run.
// Nothing in halt waits, so the stop's budget (stopBudget) runs from the moment it begins. A nil
// cause is a boot that refused before it was recorded, which served nothing and says nothing more.
func (s *supervision) halt(cause error) {
	s.haltOnce.Do(func() {
		now := time.Now()
		s.stopBy, s.drainBy = now.Add(s.plan.stopBudget), now.Add(drainOf(s.plan.stopBudget))
		if cause != nil {
			s.log.Info("legion daemon stopping", "project", s.cfg.Project, "claims", s.supervisor.count(),
				"deciding", s.supervisor.inDecision(), "cause", cause.Error())
		}
		s.beginStop()
		s.supervisor.halt()
		s.stream.Narrow(s.decided.Holds)
		s.cancel()
	})
}

// drainOf is how long the API's drain may take within a stop budget: its first four fifths
// (stopBudget).
func drainOf(budget time.Duration) time.Duration {
	return budget * 4 / 5
}

// stop ends supervision without ending a single agent: it halts it, if nothing has yet, ends the
// drain (endDrain: the routes' decisions and the worker stream), then waits until every decision
// in flight has returned and every send already out has its outcome handled, so nothing writes to
// the store once it closes. The panes and pods keep running; the next boot re-adopts them. The
// wait is unbounded here; run stops waiting for it at the stop's budget.
func (s *supervision) stop() {
	s.stopOnce.Do(func() {
		s.halt(nil)
		s.endDrain()
		s.supervisor.stop()
		s.supervisor.wait()
		s.wg.Wait()
	})
}

// serve runs the API on the listener the daemon already took until ctx is done or the server
// fails, and returns once it is closed: one goroutine serves, the other shuts down, and the
// shutdown runs on a context of its own so a cancelled ctx still drains the connections it has.
// The moment serving ends is the moment the daemon's stop begins: it halts supervision before
// anything waits — the workflow's outbox and the controller keeper would otherwise wait on a
// machine whose relaunch holds it, and the stop on them — and then drains the API until drainBy,
// letting the decisions its routes asked for finish, and ends them when the drain does (endDrain).
func serve(ctx context.Context, cfg config.Config, st *store.Store, startedAt time.Time, listener net.Listener, s *supervision, p plan, workflow *workflowRuntime) error {
	records := record.Store(record.NewStore())
	var handlers []intake.Handler
	var client dispatch.Client
	var tokens appauth.Tokens
	var grants *credential.Grants
	var claimReady func(supervise.Claim)
	var githubAPI string
	if workflow != nil {
		records, handlers, client, tokens, grants = workflow.records, workflow.handlers, workflow.dispatch, workflow.tokens, workflow.grants
		claimReady = workflow.claimReady
		githubAPI = workflow.githubAPI
	}
	// Under `controller: daemon` the keeper launches and keeps the project's controller, and takes
	// its ready. The liveness sweep runs beside it under either mode, never behind it.
	var keeper *controllerKeeper
	if cfg.ControllerLaunch == config.ControllerLaunchDaemon {
		keeper = newControllerKeeper(s.supervisor.ctx, s.supervisor, p.project, p.controllerRetry, s.log)
	}
	server := api.NewServer(cfg.Bind, cfg.Port, api.Options{
		State: &source{
			store:        st,
			records:      records,
			supervisor:   s.supervisor,
			project:      cfg.Project,
			projectToken: p.project,
			runtime:      cfg.Runtime.Name,
			admissionCap: cfg.AdmissionCap,
			startedAt:    startedAt,
			secretsLogin: p.secretsLogin,
			deployment:   s.deployment,
		},
		StateTransactions:  st,
		Supervisor:         s.supervisor,
		BootTokens:         s.tokens,
		Project:            p.project,
		OperatorToken:      p.operatorToken,
		Controller:         st,
		DesignGate:         cfg.Gates.Design,
		ControllerLaunched: keeper != nil,
		Stopping:           s.stopping,
		Drained:            s.draining,
		Decisions:          s.decided,
		Log:                s.log,
		Pool:               st.Pool(),
		Handlers:           handlers,
		Record:             records,
		Dispatch:           client,
		Tokens:             tokens,
		GitHubOwner:        githubOwner(cfg),
		GitHubAPI:          githubAPI,
		Repository:         projectRepository(cfg),
		Grants:             grants,
		Releaser:           s.supervisor.deps.Runtime,
		Trees:              st,
		ClaimReady:         claimReadyHook(keeper, claimReady),
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
		s.halt(context.Cause(serving))
		drain, cancel := context.WithDeadline(context.WithoutCancel(ctx), s.drainBy)
		defer cancel()
		err := server.Shutdown(drain)
		s.endDrain()
		if errors.Is(err, context.DeadlineExceeded) {
			// The drain's bound is the stop's, not a failure: what still ran ends now (endDrain).
			s.log.Info("legion daemon stopped draining the API; the requests still in flight end with the stop", "project", cfg.Project)
			return nil
		}
		return err
	})
	if workflow != nil {
		group.Go(func() error { return workflow.run(serving) })
	}
	if keeper != nil {
		group.Go(func() error {
			keeper.run(serving, p.orphanSweep)
			return nil
		})
	}
	group.Go(func() error {
		watchController(serving, st, cfg, p, s.log)
		return nil
	})
	return group.Wait()
}

// source answers the state route out of the daemon's own store: the daemon itself, the cap it
// admits under, every claim it supervises, filed under the issue it is on, and the project's
// controller.
type source struct {
	store        *store.Store
	records      record.Store
	supervisor   *supervisor
	project      string
	projectToken string
	runtime      string
	admissionCap int
	startedAt    time.Time
	// secretsLogin is the daemon's own agent-secrets machine login client (newSecretsLogin), read
	// through LoginStatus for the state route; nil when the deployment enrolls no pod.
	secretsLogin *agentsecrets.Client
	// deployment is the deployment's capabilities as this boot knows them (supervision.deployment),
	// whose report the state carries.
	deployment func() capabilities.Deployment
}

// projectRecords scopes the shared daemon database to the daemon's configured project without
// widening record.Store's fixed transaction interface; the workflow and the state route read the
// store through it. Every read that starts from Issues is within this filtered set. Slots are not
// filtered here: a slot's index is unique across every project's slots, so admission chooses the
// next one over all of them and counts only its own against its cap (admit.ownSlots). The outbox's
// claim is scoped in its SQL instead, because the lease it takes is itself the damage.
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
	state, err := projection.Project(ctx, tx, projectRecords{Store: s.records, project: s.project}, s.project, claims)
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
	controller, _, err := s.store.ControllerTx(ctx, tx, s.projectToken)
	if err != nil {
		return api.State{}, err
	}
	state.ControllerLocator = api.ControllerLocatorOf(s.runtime, controller)
	if s.secretsLogin != nil {
		login := s.secretsLogin.LoginStatus()
		state.AgentSecretsLogin = &api.AgentSecretsLoginView{State: login.State, Code: login.Code}
	}
	state.Capabilities = api.CapabilityStatesOf(s.deployment().Report())
	return state, nil
}

// githubOwner is the owner of the configured project's repository: the account both GitHub Apps
// are installed on. A Stage 2 configuration has no repository and so no owner.
func githubOwner(cfg config.Config) string {
	return cfg.Projects[cfg.Project].Repo.Owner()
}

// projectRepository answers the repository a project works in (`projects.<KEY>.repo`), as the
// handoff route reads an issue branch there: what outbox.createBranch creates the branch in. A
// project the configuration does not name, or names with no repository (Stage 2), has none.
func projectRepository(cfg config.Config) func(project string) (ghrepo.Repository, bool) {
	return func(project string) (ghrepo.Repository, bool) {
		configured, ok := cfg.Projects[project]
		return configured.Repo, ok && !configured.Repo.IsZero()
	}
}
