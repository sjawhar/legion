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
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/credential"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
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

// appMintAttempt bounds one attempt at the boot's App tokens, both Apps' mints: installation
// discovery, the exchange and the bot identity lookups each answer in well under a second, so an
// attempt that runs this long holds a request GitHub never answered.
var appMintAttempt = 15 * time.Second

// appMintRetry is the wait between attempts that failed transiently: 5 s doubling to 30 s, five
// attempts. At worst the boot waits 2 min 20 s (five 15 s attempts and 65 s between them) before it
// refuses, naming the last failure. The API listens only after the mint, so /healthz is refused
// meanwhile; the rest of a boot takes a second or two, which leaves a boot that passes on the last
// attempt inside the 180 s Stage 3 gives a daemon to answer /healthz.
var appMintRetry = bootprobe.Retry{Initial: 5 * time.Second, Max: 30 * time.Second, Attempts: 5}

// mintAtBoot mints the implement and then the review App token for owner, as one attempt run again
// after a failure GitHub reports as its own trouble (appauth.TransientError); any other failure is
// refused at once. The token manager keeps a lease it minted, so an attempt after the implement
// token passed asks GitHub only for the review token. It returns the review App's bot login from
// its lease: the engine judges a push by its pusher against it, so one App configured for both
// roles would make every implementer push the review App's and count no fix attempt, and is
// refused.
func mintAtBoot(ctx context.Context, tokens appauth.Tokens, owner string, log *slog.Logger) (string, error) {
	logins := map[appauth.AppRole]string{}
	err := bootprobe.Run(ctx, "GitHub App tokens mint", appMintRetry, log, func(ctx context.Context) bootprobe.Outcome {
		attempt, cancel := context.WithTimeout(ctx, appMintAttempt)
		defer cancel()
		for _, role := range []appauth.AppRole{appauth.Implement, appauth.Review} {
			lease, err := tokens.Token(attempt, role, owner)
			if err == nil {
				logins[role] = lease.Identity.Name
				continue
			}
			err = fmt.Errorf("mint %s GitHub App token at boot: %w", role, err)
			var transient *appauth.TransientError
			if errors.As(err, &transient) {
				return bootprobe.Outcome{Detail: err.Error()}
			}
			return bootprobe.Outcome{Refusal: err}
		}
		return bootprobe.Outcome{Passed: true}
	})
	if err != nil {
		return "", err
	}
	if logins[appauth.Review] == "" || logins[appauth.Review] == logins[appauth.Implement] {
		return "", fmt.Errorf("the review App's token lease names bot login %q and the implement App's %q; the workflow needs two different Apps", logins[appauth.Review], logins[appauth.Implement])
	}
	return logins[appauth.Review], nil
}

func openWorkflow(ctx context.Context, cfg config.Config, st *store.Store, projectID string, log *slog.Logger, suppliedTokens appauth.Tokens) (*workflowRuntime, error) {
	if cfg.DispatchURL == "" {
		return nil, nil
	}
	// The loader refuses a workflow whose projects do not configure the daemon's own, and a repo that
	// is not owner/name.
	project := cfg.Projects[cfg.Project]
	owner, _, _ := strings.Cut(project.Repo, "/")
	log.Info("legion workflow boot stage", "stage", "config")
	tokens := suppliedTokens
	if tokens == nil {
		tokens = appauth.New(cfg.GitHubApps, appauth.Options{})
	}
	reviewAppLogin, err := mintAtBoot(ctx, tokens, owner, log)
	if err != nil {
		return nil, err
	}
	log.Info("legion workflow boot stage", "stage", "appauth")
	// The database is shared by every project's daemon: the workflow reads this project's issues.
	records := projectRecords{Store: record.NewStore(), project: cfg.Project}
	engine := workflow.New(records, engineConfig(cfg, reviewAppLogin), log)
	admission := admit.New(records, cfg.AdmissionCap, cfg.Project, log)
	return &workflowRuntime{
		pool: st.Pool(), records: records, engine: engine, admission: admission,
		handlers: []intake.Handler{engine, admission}, tokens: tokens, owner: owner,
		grants: credential.New(nil), project: project, projectID: projectID, dispatchProject: cfg.Project, stateDir: cfg.StateDir, log: log,
		failed: make(chan error, 1),
	}, nil
}

// engineConfig is the workflow engine's configuration from the daemon's and the review App's bot
// login from its boot lease: its own project's merge_queue_role is the role the merger's READY is
// published to.
func engineConfig(cfg config.Config, reviewAppLogin string) workflow.Config {
	return workflow.Config{
		Project: cfg.Project, DesignGate: cfg.Gates.Design, ReviewRoundCap: cfg.ReviewRoundCap,
		MaxFixAttempts: cfg.MaxFixAttempts, Linger: cfg.Linger, ReviewAppLogin: reviewAppLogin,
		MergeQueueRole: cfg.Projects[cfg.Project].MergeQueueRole,
	}
}

// bind is the workflow's Dispatch client, with the bearer prepare read.
func (w *workflowRuntime) bind(url, token string) {
	w.dispatch = dispatch.New(url, token)
}

// connect opens Envoy's JetStream and this project's durable consumers, so a missing
// notification stream refuses boot rather than leaving a daemon that reads no events. Boot runs it
// before reconcile, whose Dispatch listing covers only what precedes a consumer created now.
func (w *workflowRuntime) connect(ctx context.Context, cfg config.Config) error {
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

// phaseHolds answers supervise's Deps.PhaseHolds from the issue record: a task carries the phase
// it was queued for, so once the issue is in another phase a delivery still queued for that one is
// finished work and is dropped rather than sent. The phase is compared, not the role that works
// it: one role runs several phases — the implementer runs implementing, retro and the production
// check — and a task written for the first is not the work of the third.
//
// A delivery of no phase holds here. Only the workflow names one, so an operator's delivery made
// by hand through the API is not dropped for the phase its issue is in: the operator asked for
// it, was answered 200, and dropping it would make the work silently not happen. An architect's
// task belongs to no phase and names none for the same reason. This predicate is not the only
// way a delivery ends: a suspension retires an unconfirmed one that names a phase, the phase it
// is ending (supervise's settle).
//
// Nothing else on the delivery would answer this: the id is rotated by a prompt retry, and the
// task text is the workflow's prose. The phase is typed data, persisted with the delivery, and no
// path rewrites it.
//
// A claim on an issue the daemon does not record holds too. The workflow never deletes an issue,
// so there is no record only for a claim the workflow never made — the operator's own spawn, whose
// issue key need not be an issue key at all.
func (w *workflowRuntime) phaseHolds(ctx context.Context, key string, queuedFor phase.Phase) (bool, error) {
	issue, err := w.recordedIssue(ctx, key)
	if err != nil {
		return false, fmt.Errorf("read %s for the phase of its task: %w", key, err)
	}
	if issue == nil {
		return true, nil
	}
	return issue.Phase == queuedFor, nil
}

// treeClosable answers supervise's Deps.TreeClosable: a tree a workflow issue backs closes when
// its linger expires, never on an operator's close of its root claim. The machine asks it where
// the close is decided rather than a round trip before it, for the operator's own close and for a
// refused stop of the tree's root, whose refusal then names that close. The workflow's linger
// close is never asked: it holds that same issue record, so asking would refuse exactly the
// closes the workflow is entitled to make. It is not a lock on what it reads: admission and the
// engine commit issue records in their own transactions and take no machine lock, so one can
// still land between this answer and the retire.
func (w *workflowRuntime) treeClosable(ctx context.Context, c supervise.Claim) (bool, error) {
	issue, err := w.recordedIssue(ctx, c.Tree)
	if err != nil {
		return false, fmt.Errorf("read whether a workflow issue backs tree %s: %w", c.Tree, err)
	}
	return issue == nil, nil
}

// recordedIssue reads one issue in a transaction of its own, which is what both supervisor
// predicates need: the machine asks them while holding its own lock, and neither answer may take
// a lock of the daemon's.
func (w *workflowRuntime) recordedIssue(ctx context.Context, key string) (*record.Issue, error) {
	var issue *record.Issue
	err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		var err error
		issue, err = w.records.Issue(ctx, tx, key)
		return err
	})
	return issue, err
}

// reconcile takes Dispatch's bounded boot read to admission. Only admission acts on a snapshot:
// a move a human made while the daemon was down carries its own event, which the durable stream
// consumer still holds and delivers with the actor that made it, so nothing here re-derives one.
func (w *workflowRuntime) reconcile(ctx context.Context) error {
	issues, err := w.dispatch.ListIssues(ctx, w.dispatchProject, []string{"todo", "in_progress", "testing", "needs_review", "retro"})
	if err != nil {
		return fmt.Errorf("list Dispatch issues for admission: %w", err)
	}
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		return w.admission.Reconcile(ctx, tx, issues)
	}); err != nil {
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
		w.tokens, w.handlers, w.projectID, w.dispatchProject, w.stateDir, w.project, supervision.plan.tools, w.log)
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
