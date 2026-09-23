package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// workflowRuntime is the daemon-owned integration surface around the pure transition engine.
// It is absent for Stage 2 configurations, whose unchanged supervision proof deliberately has no
// Dispatch, Apps, or Envoy stream configured.
type workflowRuntime struct {
	pool            *pgxpool.Pool
	records         record.Store
	engine          *workflow.Engine
	admission       *admit.Admission
	handlers        []intake.Handler
	dispatch        dispatch.Client
	tokens          appauth.Tokens
	owner           string
	grants          *credential.Grants
	project         config.Project
	projectID       string
	dispatchProject string
	stateDir        string
	log             *slog.Logger
	conn            *nats.Conn
	consumers       *intake.Consumers
	outbox          *outbox
	// failed carries the first supervision terminal fact that could not be applied. serve stops
	// the daemon with it: the claim's terminal state is durable, so the next boot's replay applies
	// the fact the failed callback lost.
	failed chan error
}

func openWorkflow(ctx context.Context, cfg config.Config, st *store.Store, projectID string, log *slog.Logger, suppliedTokens appauth.Tokens) (*workflowRuntime, error) {
	if cfg.DispatchURL == "" {
		return nil, nil
	}
	project, ok := cfg.Projects[cfg.Project]
	if !ok {
		return nil, fmt.Errorf("projects must configure %s", cfg.Project)
	}
	owner, _, ok := strings.Cut(project.Repo, "/")
	if !ok || owner == "" {
		return nil, fmt.Errorf("projects.%s.repo %q must be owner/repository", cfg.Project, project.Repo)
	}
	log.Info("legion workflow boot stage", "stage", "config")
	tokens := suppliedTokens
	if tokens == nil {
		tokens = appauth.New(cfg.GitHubApps, appauth.Options{})
	}
	for _, role := range []appauth.AppRole{appauth.Implement, appauth.Review} {
		if _, err := tokens.Token(ctx, role, owner); err != nil {
			return nil, fmt.Errorf("mint %s GitHub App token at boot: %w", role, err)
		}
	}
	log.Info("legion workflow boot stage", "stage", "appauth")
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{
		Project: cfg.Project, DesignGate: cfg.Gates.Design, ReviewRoundCap: cfg.ReviewRoundCap,
		MaxFixAttempts: cfg.MaxFixAttempts, LingerHours: time.Duration(cfg.LingerHours) * time.Hour,
	}, log)
	admission := admit.New(records, cfg.AdmissionCap, cfg.Project, log)
	return &workflowRuntime{
		pool: st.Pool(), records: records, engine: engine, admission: admission,
		handlers: []intake.Handler{engine, admission}, tokens: tokens, owner: owner,
		grants: credential.New(nil), project: project, projectID: projectID, dispatchProject: cfg.Project, stateDir: cfg.StateDir, log: log,
		failed: make(chan error, 1),
	}, nil
}

func (w *workflowRuntime) bind(cfg config.Config) error {
	if cfg.DispatchTokenFile == "" {
		return errors.New("dispatch_token_file is required when dispatch_url is configured")
	}
	token, err := config.ReadSecretPointer("dispatch_token_file", cfg.DispatchTokenFile)
	if err != nil {
		return err
	}
	w.dispatch = dispatch.New(cfg.DispatchURL, token)
	return nil
}

// connect opens Envoy's JetStream and this project's durable consumers, so a missing
// notification stream refuses boot rather than leaving a daemon that reads no events.
func (w *workflowRuntime) connect(ctx context.Context, cfg config.Config) error {
	if len(cfg.NatsURLs) == 0 {
		return errors.New("nats_urls is required when dispatch_url is configured")
	}
	conn, err := nats.Connect(strings.Join(cfg.NatsURLs, ","))
	if err != nil {
		return fmt.Errorf("connect Envoy NATS: %w", err)
	}
	w.conn = conn
	js, err := jetstream.New(conn)
	if err != nil {
		return fmt.Errorf("open Envoy JetStream: %w", err)
	}
	w.log.Info("legion workflow boot stage", "stage", "intake")
	w.consumers, err = intake.OpenConsumers(ctx, js, intake.ConsumerSpec{
		Project: cfg.Project, Repositories: []string{w.project.Repo}, AckWait: cfg.WorkerRPCTimeout, NakDelay: time.Second, Logger: w.log,
	})
	return err
}

func (w *workflowRuntime) reconcile(ctx context.Context) error {
	issues, err := w.dispatch.ListIssues(ctx, w.dispatchProject, []string{"todo", "in_progress", "testing", "needs_review", "retro"})
	if err != nil {
		return fmt.Errorf("list Dispatch issues for admission: %w", err)
	}
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error { return w.admission.Reconcile(ctx, tx, issues) }); err != nil {
		return fmt.Errorf("reconcile admission: %w", err)
	}
	return nil
}

// identity is the bot a role's commits are authored by: its App's, from the lease the token
// source mints for the repository owner (the identity is fetched once per App and cached).
func (w *workflowRuntime) identity(ctx context.Context, role claim.Role) (runtime.GitIdentity, error) {
	lease, err := w.tokens.Token(ctx, appauth.AppRoleFor(role), w.owner)
	if err != nil {
		return runtime.GitIdentity{}, fmt.Errorf("mint the %s App lease for %s: %w", appauth.AppRoleFor(role), role, err)
	}
	return lease.Identity, nil
}

func (w *workflowRuntime) attach(supervision *supervision) {
	w.log.Info("legion workflow boot stage", "stage", "outbox")
	w.outbox = newOutbox(w.pool, w.records, w.dispatch, notify.New(supervision.cfg.EnvoyURL, supervision.plan.secrets["ENVOY_TOKEN"]), supervision.supervisor,
		w.tokens, w.handlers, w.projectID, w.stateDir, w.project, supervision.plan.tools, w.log)
	supervision.supervisor.OnTerminal(w.terminal)
}

func (w *workflowRuntime) replayTerminal(ctx context.Context, claims []supervise.Claim) error {
	for _, c := range claims {
		if c.State != supervise.StateReady && c.State != supervise.StateFailed {
			continue
		}
		if err := w.applyTerminal(ctx, c, c.State); err != nil {
			return err
		}
	}
	return nil
}

func (w *workflowRuntime) terminal(c supervise.Claim, state supervise.ClaimState) {
	if err := w.applyTerminal(context.Background(), c, state); err != nil {
		w.log.Error("apply supervision terminal fact", "claim", c.Token, "state", state, "error", err)
		select {
		case w.failed <- err:
		default:
		}
	}
}

func (w *workflowRuntime) applyTerminal(ctx context.Context, c supervise.Claim, state supervise.ClaimState) error {
	var fact intake.Fact
	switch state {
	case supervise.StateReady:
		fact = intake.ClaimReady{Issue: c.Issue, Role: c.Role}
	case supervise.StateFailed:
		fact = intake.ClaimFailed{Issue: c.Issue, Role: c.Role}
	default:
		return fmt.Errorf("claim %s has non-terminal workflow state %s", c.Token, state)
	}
	_, err := intake.ApplyFact(ctx, w.pool, "supervise", fmt.Sprintf("supervise:%s:%d:%s", c.Token, c.Generation, state), fact, w.handlers...)
	if err != nil {
		return fmt.Errorf("apply %s for %s: %w", state, c.Token, err)
	}
	return nil
}

// run drains intake and the outbox until ctx ends. Intake ending, or a terminal fact that could not
// be applied, returns its error so serve stops the daemon: a daemon that answers its API while it
// reads no events or has lost a held or worker-died fact looks healthy and does nothing.
func (w *workflowRuntime) run(ctx context.Context) error {
	group, running := errgroup.WithContext(ctx)
	group.Go(func() error {
		if err := w.consumers.Run(running, w.pool, w.handlers...); err != nil {
			return fmt.Errorf("workflow intake stopped: %w", err)
		}
		return nil
	})
	group.Go(func() error {
		w.outbox.Run(running)
		return nil
	})
	group.Go(func() error {
		select {
		case err := <-w.failed:
			return fmt.Errorf("workflow supervision fact: %w", err)
		case <-running.Done():
			return nil
		}
	})
	return group.Wait()
}

func (w *workflowRuntime) stop() {
	if w == nil {
		return
	}
	if w.conn != nil {
		w.conn.Close()
	}
}
