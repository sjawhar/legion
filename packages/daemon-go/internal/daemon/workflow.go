package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
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
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
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
	// bootID disambiguates this boot's synthetic Dispatch consumer position facts from another
	// boot's, or another project's daemon: processed_events is keyed only on (source, event_id),
	// shared by every project's daemon in the database, and a stream recreated by a later boot
	// would otherwise repeat the same (ack floor, idle) pairs and have its own releases swallowed
	// as duplicates of the previous boot's.
	bootID string
	// holdPollInterval is pollHoldRelease's ticker period; zero means the production default. A
	// test shortens it to bound how long a release takes to observe.
	holdPollInterval time.Duration
	// holdWarnAfter and holdWarnEvery bound the watchdog log a hold that never releases gets: zero
	// means the production defaults. A test shortens both to bound how long the warning takes to
	// observe.
	holdWarnAfter, holdWarnEvery time.Duration
	// failed carries the first supervision terminal fact that could not be applied. serve stops
	// the daemon with it: the claim's terminal state is durable, so the next boot's replay applies
	// the fact the failed callback lost.
	failed chan error
	// readied is the architects claimReady recorded whose waiting notices releaseReadied has not
	// released yet; readyWake, holding at most one wake, tells it there are some.
	readyMu   sync.Mutex
	readied   map[claim.Token]bool
	readyWake chan struct{}
}

// readyReleaseTimeout bounds one release of ready architects' waiting notices
// (outbox.releaseWaiting): a read of a project's waiting notices and their trees, then one update,
// each well under a second. A release that runs this long holds a database that stopped answering;
// it is logged, and the notices it would have released still go on their schedule.
const readyReleaseTimeout = 30 * time.Second

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
	owner := project.Repo.Owner()
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
	admission := admit.New(records, engine, cfg.AdmissionCap, cfg.Project, log)
	return &workflowRuntime{
		pool: st.Pool(), records: records, engine: engine, admission: admission,
		handlers: []intake.Handler{engine, admission}, tokens: tokens, owner: owner,
		grants: credential.New(nil), project: project, projectID: projectID, dispatchProject: cfg.Project, stateDir: cfg.StateDir, log: log,
		failed: make(chan error, 1), readied: map[claim.Token]bool{}, readyWake: make(chan struct{}, 1),
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
// before reconcile, whose Dispatch listing covers only what precedes a consumer created now. It
// connects as nc, the user readBoot chose (natsConnection), after logging it, and logs what the
// server reports about the connection: every permission it refuses at error, every disconnect at
// warn and every reconnect at info (natsauth.LogEvents).
func (w *workflowRuntime) connect(ctx context.Context, cfg config.Config, nc natsConnection) error {
	nc.log(w.log)
	conn, err := natsauth.Connect(cfg.NatsURLs, nc.seed, natsauth.LogEvents(w.log))
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
		Project: cfg.Project, Repositories: []ghrepo.Repository{w.project.Repo}, AckWait: cfg.WorkerRPCTimeout, NakDelay: time.Second, Logger: w.log,
	})
	if err != nil {
		return err
	}
	w.bootID = fmt.Sprintf("%d", time.Now().UnixNano())
	return nil
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
// The listing is read first, then the notification stream's own current position (target), then
// the Dispatch consumer's own: a message published between the listing and target would land in
// the listing but go uncounted by target, and reading the consumer's position last catches it up
// as far as this call can before deciding what a record behind target must still wait for.
//
// The listing reads every status, not only the workflow's own (todo through retro): Dispatch
// already returns its complete project list in one call (HTTPClient.ListIssues filters it
// client-side, at no extra request cost), and a key currently out of that window — moved to
// backlog, or never past triage — while the daemon was down still needs to be held exactly like
// one still in it, so a replayed event that predates the move it fell out on cannot be admitted
// before the move's own event ever arrives.
func (w *workflowRuntime) reconcile(ctx context.Context) error {
	issues, err := w.dispatch.ListIssues(ctx, w.dispatchProject, nil)
	if err != nil {
		return fmt.Errorf("list Dispatch issues for admission: %w", err)
	}
	target, err := w.consumers.DispatchTarget(ctx)
	if err != nil {
		return fmt.Errorf("read notification stream target: %w", err)
	}
	position, err := w.consumers.DispatchPosition(ctx)
	if err != nil {
		return fmt.Errorf("read Dispatch consumer position: %w", err)
	}
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		return w.admission.Reconcile(ctx, tx, issues, target, position)
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
			// A terminal close's cause is the connection's alone (natsauth.WithLastError).
			return fmt.Errorf("workflow intake stopped: %w", natsauth.WithLastError(err, w.conn))
		}
		return nil
	})
	group.Go(func() error {
		w.outbox.Run(running)
		return nil
	})
	group.Go(func() error {
		w.pollHoldRelease(running)
		return nil
	})
	group.Go(func() error {
		w.releaseReadied(running)
		return nil
	})
	group.Go(func() error {
		// A notice the listener accepted but could not forward is queued again (outbox.rehold).
		sub, err := subscribeNoticeExceptions(w.conn, func(data []byte) {
			if err := w.outbox.rehold(running, data); err != nil {
				w.log.Error("role-lane exception not re-held", "error", err)
			}
		})
		if err != nil {
			return err
		}
		<-running.Done()
		return sub.Unsubscribe()
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

// defaultHoldPollInterval is pollHoldRelease's production ticker period.
const defaultHoldPollInterval = 2 * time.Second

// defaultHoldWarnAfter and defaultHoldWarnEvery bound the watchdog log a hold that has not
// released gets: a Dispatch event whose transaction always fails (an unmet precondition, a
// programming bug) pins the ack floor forever, since the consumer sets no MaxDeliver to ever give
// up on it, and nothing else says why a labeled root is waiting.
const (
	defaultHoldWarnAfter = 2 * time.Minute
	defaultHoldWarnEvery = 5 * time.Minute
)

// positionReader is pollHoldRelease's only dependency on the Dispatch consumer: reading its
// current position. A test substitutes one that fails on demand to prove the poll logs and
// retries rather than silently ending the hold.
type positionReader interface {
	DispatchPosition(ctx context.Context) (intake.DispatchConsumerPosition, error)
}

// pollHoldRelease is the boot-owned release that replaces intake's per-delivery hook with: while
// admission holds anything back, it re-reads the Dispatch consumer's own position on a ticker and
// applies each changed reading as a synthetic fact, independent of any message delivery. A quiet
// stream after its last backlog message delivers nothing further to trigger a release the old
// per-delivery hook depended on, and Ack() does not wait for the server: a position read taken
// immediately after a delivery's own ack can still see the previous one. This ticker is what
// eventually observes the ack once JetStream has applied it, bounded by its own period, and what
// closes a hold nothing will ever redeliver — the recreated-consumer and outbox-lag cases alike.
// A failed read or a failed apply is logged and retried on the next tick — last only adopts a
// position once ApplyFact for it has actually committed, so an unchanged reading after either
// kind of failure is retried rather than skipped as already seen; the hold never ends on the
// strength of an error.
// The event id folds in the project and this boot (bootID): processed_events is one table
// shared by every project's daemon, and a stream a later boot recreates would otherwise repeat an
// earlier boot's (ack floor, idle) pairs and have its own release swallowed as a duplicate of one
// that ran under the old boot.
func (w *workflowRuntime) pollHoldRelease(ctx context.Context) {
	w.pollHoldReleaseWith(ctx, w.consumers)
}

func (w *workflowRuntime) pollHoldReleaseWith(ctx context.Context, reader positionReader) {
	interval := w.holdPollInterval
	if interval <= 0 {
		interval = defaultHoldPollInterval
	}
	warnAfter, warnEvery := w.holdWarnAfter, w.holdWarnEvery
	if warnAfter <= 0 {
		warnAfter = defaultHoldWarnAfter
	}
	if warnEvery <= 0 {
		warnEvery = defaultHoldWarnEvery
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var last intake.DispatchConsumerPosition
	haveLast := false
	var heldSince, warnedAt time.Time
	wasHeld := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !w.admission.Held() {
			haveLast, heldSince, warnedAt = false, time.Time{}, time.Time{}
			if wasHeld {
				// The hold just cleared: a record a held key's own recording put down between an
				// earlier release's commit and its AfterCommit (which cleared pending) may still be
				// sitting in the waiting line, held back by that same release's own promote, which
				// ran before pending was actually cleared. One more pass, now that pending is
				// provably empty, promotes it without waiting on an unrelated fact to notice. A
				// fresh, never-reused event id (a closing pass never repeats a boot's own position
				// reading) keeps ApplyFact from treating it as the duplicate of whatever position
				// last released the hold. A failure is retried on the poll's own schedule, same as
				// a failed position apply: wasHeld only clears once the pass actually succeeds.
				if err := w.promoteAfterHoldClears(ctx); err != nil {
					w.log.Warn("apply the post-release promote failed", "error", err)
					continue
				}
			}
			wasHeld = false
			continue
		}
		wasHeld = true
		if heldSince.IsZero() {
			heldSince = time.Now()
		}
		position, err := reader.DispatchPosition(ctx)
		if err != nil {
			w.log.Error("read Dispatch consumer position for a held release", "error", err)
			continue
		}
		// This never releases on the strength of the timer — only a Reached position does that,
		// through the ApplyFact below — so a stuck hold stays stuck, but an operator now sees why:
		// the target it is waiting for and its current ack floor. Not the stream sequence stuck
		// behind that floor: the notification stream also carries GitHub subjects interleaved with
		// Dispatch's, so ack_floor+1 can name a message that has nothing to do with this hold.
		if waiting := time.Since(heldSince); waiting >= warnAfter && time.Since(warnedAt) >= warnEvery {
			warnedAt = time.Now()
			w.log.Warn("admission: a hold has not released", "waiting", waiting.Round(time.Second),
				"target", w.admission.Target(), "ack_floor", position.AckFloorStream)
		}
		if haveLast && position == last {
			continue
		}
		eventID := fmt.Sprintf("dispatch-position:%s:%s:%d:%t", w.dispatchProject, w.bootID, position.AckFloorStream, position.Idle)
		if _, err := intake.ApplyFact(ctx, w.pool, "dispatch", eventID, position, w.handlers...); err != nil {
			w.log.Warn("apply Dispatch consumer position failed", "error", err)
			continue
		}
		last, haveLast = position, true
	}
}

// promoteAfterHoldClears applies one synthetic DispatchConsumerPosition fact, its event id never
// reused, once nothing is held: Admission.Apply routes any DispatchConsumerPosition straight to
// release, which — with pending now provably empty — promotes exactly as a normal promote() would,
// picking up any candidate a held key's own recording left waiting behind it. The position's
// own values do not matter here: caughtUp is false whenever pending is empty, so release only
// reaches its own promoteHolds call, never re-applies a summary.
func (w *workflowRuntime) promoteAfterHoldClears(ctx context.Context) error {
	eventID := fmt.Sprintf("dispatch-position:%s:%s:closing:%d", w.dispatchProject, w.bootID, time.Now().UnixNano())
	_, err := intake.ApplyFact(ctx, w.pool, "dispatch", eventID, intake.DispatchConsumerPosition{}, w.handlers...)
	return err
}

// claimReady records an architect whose claim is ready, for releaseReadied to release the notices
// waiting for a later attempt that it owns (outbox.releaseWaiting): a relaunched architect is told
// what it missed at once, ahead of what follows. The ready route calls it before it answers the
// agent, so it only records the claim and wakes the release.
func (w *workflowRuntime) claimReady(c supervise.Claim) {
	if c.Role != claim.RoleArchitect {
		return
	}
	w.readyMu.Lock()
	w.readied[c.Token] = true
	w.readyMu.Unlock()
	select {
	case w.readyWake <- struct{}{}:
	default:
	}
}

// releaseReadied releases, until ctx ends, the waiting notices of the architects claimReady
// recorded: each wake takes every architect recorded since the last one, in one release.
func (w *workflowRuntime) releaseReadied(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.readyWake:
		}
		w.readyMu.Lock()
		architects := slices.Collect(maps.Keys(w.readied))
		clear(w.readied)
		w.readyMu.Unlock()
		if len(architects) == 0 {
			continue
		}
		release, cancel := context.WithTimeout(ctx, readyReleaseTimeout)
		err := w.outbox.releaseWaiting(release, architects...)
		cancel()
		if err != nil && ctx.Err() == nil {
			w.log.Error("release the notices waiting for ready architects", "architects", architects, "error", err)
		}
	}
}

func (w *workflowRuntime) stop() {
	if w == nil {
		return
	}
	if w.conn != nil {
		w.conn.Close()
	}
}
