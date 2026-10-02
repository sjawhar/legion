package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const (
	outboxBatchSize = 32
	// outboxDeliveryPrefix namespaces a delivery id the outbox mints from its row id, so a row's
	// id cannot collide with a minted one. What a delivery is for is its phase, not its id: the
	// id is rotated by a prompt retry.
	outboxDeliveryPrefix = "outbox:"
	outboxLease          = 30 * time.Second
	outboxPoll           = 100 * time.Millisecond
	// outboxFailedTickWait spaces the ticks while the database refuses, so an outage logs once a
	// second rather than ten times.
	outboxFailedTickWait = time.Second
	messageReadSkew      = 5 * time.Second
	// pendingWaitWarnAttempts is when a row waiting on its claim's pending delivery is logged as a
	// warning: past the backoff's climb to its cap, a wait of about two minutes, the turn it waits
	// on is not ending on its own, and an operator should look.
	pendingWaitWarnAttempts = 7
)

// outbox runs each effect that the workflow transaction committed. Claiming and finishing have
// separate transactions so a slow external service never holds an issue transition lock.
type outbox struct {
	pool       *pgxpool.Pool
	records    record.Store
	dispatch   dispatch.Client
	notices    notify.Publisher
	supervisor *supervisor
	tokens     appauth.Tokens
	handlers   []intake.Handler
	project    string
	// dispatchProject is the Dispatch project whose issues' rows this outbox claims: the database
	// is shared, and another project's rows are another daemon's.
	dispatchProject string
	stateDir        string
	repo            ghrepo.Repository
	log             *slog.Logger
	now             func() time.Time
	provision       func(context.Context, workspace.Request) (workspace.Workspace, error)
	remove          func(context.Context, workspace.Workspace) error
}

func newOutbox(pool *pgxpool.Pool, records record.Store, client dispatch.Client, publisher notify.Publisher, supervisor *supervisor, tokens appauth.Tokens, handlers []intake.Handler, project, dispatchProject, stateDir string, configured config.Project, tools map[string]string, log *slog.Logger) *outbox {
	if log == nil {
		log = slog.Default()
	}
	return &outbox{
		pool: pool, records: records, dispatch: client, notices: publisher, supervisor: supervisor, tokens: tokens,
		handlers: handlers, project: project, dispatchProject: dispatchProject, stateDir: stateDir, repo: configured.Repo, log: log, now: time.Now,
		// WarmCodegraphIndexInBackground runs here, never in provisionWorkspace: every outbox
		// test injects its own `provision`, so only this production closure starts codegraph.
		provision: func(ctx context.Context, request workspace.Request) (workspace.Workspace, error) {
			provisioned, err := workspace.Provision(ctx, workspace.NewRunner(workspace.CommandTimeout, tools), request)
			if err != nil {
				return workspace.Workspace{}, err
			}
			workspace.WarmCodegraphIndexInBackground(provisioned.Dir)
			return provisioned, nil
		},
		remove: func(ctx context.Context, working workspace.Workspace) error {
			return workspace.Remove(ctx, workspace.NewRunner(workspace.CommandTimeout, tools), working)
		},
	}
}

// Run keeps due effects draining until the daemon stops. It runs immediately so a restart never
// waits one poll interval before recovering a row whose prior lease expired. A tick whose claim
// fails (a Postgres restart drops the pool's connections) is logged and tried again after
// outboxFailedTickWait, so the runner outlives the failure instead of ending with it.
func (r *outbox) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		wait := outboxPoll
		if err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("outbox tick failed; the next tick tries again", "error", err)
			wait = outboxFailedTickWait
		}
		timer.Reset(wait)
	}
}

// RunOnce claims currently due rows, executes each side effect outside the lease transaction, and
// finishes or retries only while its lease token still matches. A row whose finish or retry fails
// is logged and left leased: its lease expires and the row is due again, so one row's failed
// transaction never stops the rows after it.
func (r *outbox) RunOnce(ctx context.Context) error {
	if r.log == nil {
		r.log = slog.Default()
	}
	if r.pool == nil || r.records == nil {
		return errors.New("outbox requires Postgres and its record store")
	}
	now := r.now().UTC()
	var rows []record.OutboxRow
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		rows, err = r.records.ClaimDue(ctx, tx, r.dispatchProject, now, outboxBatchSize, outboxLease)
		return err
	}); err != nil {
		return fmt.Errorf("claim due outbox rows: %w", err)
	}
	for _, row := range rows {
		if err := r.execute(ctx, row); err != nil {
			if errors.Is(err, errNoticeWaits) {
				// A held notice is a wait for its architect, logged once for the row rather than on
				// every attempt of its backoff.
				if row.Attempts == 0 {
					r.log.Info("outbox notice waits for its architect", "row", row.ID, "issue", row.Issue, "error", err)
				}
			} else if errors.Is(err, supervise.ErrSuspendHeld) {
				// A suspension held for its agent's turn (supervise's holdSuspension) is a wait: the
				// row is asked again on its backoff and finishes once the claim is suspended.
				if row.Attempts == 0 {
					r.log.Info("outbox suspend is held for its agent's turn to end", "row", row.ID, "issue", row.Issue, "error", err)
				}
			} else if errors.Is(err, supervise.ErrQuiesceHeld) {
				// A start held for the turn of the role it takes the phase from (supervise's Quiesce)
				// is a wait: the row is asked again on its backoff and goes on once that turn is over.
				if row.Attempts == 0 {
					r.log.Info("outbox start is held for the turn of the role it takes the phase from", "row", row.ID, "issue", row.Issue, "error", err)
				}
			} else if errors.Is(err, supervise.ErrDeliveryPending) {
				// A task meeting the claim's own pending delivery is a wait, not a failure: the row
				// runs again on the same backoff once that delivery's turn is over.
				level := slog.LevelDebug
				if row.Attempts >= pendingWaitWarnAttempts {
					level = slog.LevelWarn
				}
				r.log.Log(ctx, level, "outbox row waits for the claim's pending delivery", "row", row.ID, "kind", row.Kind, "issue", row.Issue,
					"attempts", row.Attempts, "error", err)
			} else {
				r.log.Error("outbox row failed", "row", row.ID, "kind", row.Kind, "error", err)
			}
			if refusal, permanent := permanentStatusRefusal(row, err); permanent {
				// An issue's status writes run one at a time, so a row Dispatch will refuse
				// identically forever would hold every later status of that issue behind it. It is
				// finished instead, and the board follows the workflow again from the next one.
				r.log.Error("outbox status write refused by Dispatch and dropped; the issue's later writes go on",
					"row", row.ID, "issue", row.Issue, "status", refusal.Status, "code", refusal.Code, "error", err)
				if err := r.finish(ctx, row); err != nil {
					r.log.Error("outbox row finish not recorded; it runs again when its lease expires", "row", row.ID, "error", err)
				}
				continue
			}
			if retryErr := r.retry(ctx, row, err); retryErr != nil {
				r.log.Error("outbox row retry not recorded; it runs again when its lease expires", "row", row.ID, "error", retryErr)
			}
			continue
		}
		if err := r.finish(ctx, row); err != nil {
			r.log.Error("outbox row finish not recorded; it runs again when its lease expires", "row", row.ID, "error", err)
		}
	}
	return nil
}

func (r *outbox) finish(ctx context.Context, row record.OutboxRow) error {
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.FinishOutbox(ctx, tx, row.ID, row.LeaseToken)
	}); err != nil {
		return fmt.Errorf("finish outbox row %d: %w", row.ID, err)
	}
	return nil
}

func (r *outbox) retry(ctx context.Context, row record.OutboxRow, cause error) error {
	nextAt := r.now().UTC().Add(retryBackoff(row.Attempts))
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.RetryOutbox(ctx, tx, row.ID, row.LeaseToken, nextAt, cause.Error())
	}); err != nil {
		return fmt.Errorf("retry outbox row %d: %w", row.ID, err)
	}
	return nil
}

func retryBackoff(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if attempts > 6 {
		attempts = 6
	}
	return time.Second << attempts
}

// permanentStatusRefusal is a Dispatch status write whose refusal will not change however many
// times the write is made. Only a status row is judged — it is the kind that fences an issue's
// later writes — and what makes a refusal permanent is the Dispatch client's own rule.
func permanentStatusRefusal(row record.OutboxRow, err error) (*dispatch.Error, bool) {
	if row.Kind != record.OutboxKindDispatchStatus {
		return nil, false
	}
	return dispatch.PermanentRefusal(err)
}

func (r *outbox) execute(ctx context.Context, row record.OutboxRow) error {
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		return fmt.Errorf("decode outbox row %d: %w", row.ID, err)
	}
	switch value := payload.(type) {
	case record.StatusWrite:
		return r.status(ctx, row, value)
	case record.MessagePost:
		return r.message(ctx, row, value)
	case record.Notice:
		return r.notice(ctx, row, value)
	case record.ControllerNotice:
		return r.controllerNotice(ctx, row, value)
	case record.SuperviseRequest:
		return r.supervise(ctx, row, value)
	case record.GateSeed:
		return r.seedGate(ctx, row, value)
	case record.LingerClose:
		return r.linger(ctx, row, value)
	case record.WorkspaceRemove:
		return r.removeWorkspace(ctx, row, value)
	case record.MergeQueuePublish:
		return r.mergeQueue(ctx, row, value)
	default:
		return fmt.Errorf("outbox row %d: no executor for %T", row.ID, payload)
	}
}

func (r *outbox) status(ctx context.Context, row record.OutboxRow, payload record.StatusWrite) error {
	if r.dispatch == nil {
		return errors.New("dispatch status executor has no Dispatch client")
	}
	issue, err := r.dispatch.GetIssue(ctx, row.Issue)
	if err != nil {
		return fmt.Errorf("read Dispatch issue %s before status write: %w", row.Issue, err)
	}
	if issue.Status != payload.ObservedStatus && issue.Status != payload.Status {
		return nil
	}
	if issue.Status == payload.Status {
		return nil
	}
	// A done write's reason goes on the issue first: Dispatch refuses a message on a closed issue.
	// It is posted as this row's message, so a retry after a failed write finds it and posts none.
	if payload.Reason != "" {
		if err := r.message(ctx, row, record.MessagePost{Body: payload.Reason}); err != nil {
			return err
		}
	}
	if err := r.dispatch.SetStatus(ctx, row.Issue, payload.Status); err != nil {
		return fmt.Errorf("set Dispatch issue %s to %s: %w", row.Issue, payload.Status, err)
	}
	return nil
}

func (r *outbox) message(ctx context.Context, row record.OutboxRow, payload record.MessagePost) error {
	if r.dispatch == nil {
		return errors.New("dispatch message executor has no Dispatch client")
	}
	marker := record.MessageMarker(row.ID)
	bodies, err := r.dispatch.MessageBodiesSince(ctx, row.Issue, row.CreatedAt.Add(-messageReadSkew))
	if err != nil {
		return fmt.Errorf("read Dispatch messages for %s: %w", row.Issue, err)
	}
	for _, body := range bodies {
		if strings.Contains(body, marker) {
			return nil
		}
	}
	if err := r.dispatch.PostMessage(ctx, row.Issue, payload.Posted(row.ID)); err != nil {
		return fmt.Errorf("post Dispatch message for %s: %w", row.Issue, err)
	}
	return nil
}

// notice publishes a notice row to the architect that owns its issue, on that architect's own role
// topic, and to nothing else. Every notice kind is for an architect, and every issue topic is a
// subject that issue's phase workers subscribe to (packages/pi-envoy/src/legion/go-bootstrap.ts),
// so no issue topic carries one. The owner, the earlier notices it waits behind, and whether its
// tree lingers are read from one snapshot of the tree (notice_routing.go). A notice waits
// (errNoticeWaits) behind an earlier notice of its tree that is held for the same architect, so
// each architect is told in the order the notices were written. A role topic with no live holder
// refuses the publish (notify.ErrNoHolder): while the owning architect's claim can hold its role
// again — relaunching, or a root the operator suspended and can resume — the row is held for it;
// once nobody will hold that role for this notice — the claim has failed or retired, or its tree
// lingers or has closed — the row finishes undelivered with one log line. So does a row whose
// architect cannot be resolved from the record (errNoticeUnroutable), which holds back no later
// notice either, and, before any publish, a row whose architect stopped with its finished tree
// (stoppedWithTree). A publish the listener accepts but then cannot forward, to a session that is
// registered but no longer running, comes back as a role-lane exception and is queued again
// (rehold, notice_exceptions.go) unless its architect stopped with its finished tree. The runner
// executes a row it leased from memory, so a row deleted under its lease since (a catch-up a newer
// ready dropped) is found gone in the tree's snapshot and finishes without a publish, and so does a
// catch-up a newer one superseded (catchUpSuperseded, read against the snapshot's root and the
// claim as it runs now): the re-hold reads the claim from memory and writes its copy outside
// ApplyFact's lock, so it can lose the race to a ready and commit an older copy after the fresh
// catch-up.
func (r *outbox) notice(ctx context.Context, row record.OutboxRow, payload record.Notice) error {
	if r.notices == nil {
		return errors.New("notice executor has no Envoy publisher")
	}
	if r.supervisor == nil {
		return errors.New("notice executor has no claim supervisor")
	}
	issue, tree, err := r.readNoticeTree(ctx, row)
	if errors.Is(err, errNoticeRowGone) {
		r.log.Info("outbox notice finished without publishing: its row was deleted under its lease", "row", row.ID, "kind", payload.Kind, "issue", row.Issue)
		return nil
	}
	if errors.Is(err, errNoticeUnroutable) {
		r.log.Error("outbox notice finished undelivered: it has no architect", "row", row.ID, "kind", payload.Kind, "issue", row.Issue, "error", err)
		return nil
	}
	if err != nil {
		return err
	}
	if payload.CatchUp != nil {
		root, err := claim.NewToken(tree.project, tree.root.Key, claim.RoleArchitect)
		if err != nil {
			return fmt.Errorf("the architect of %s: %w", tree.root.Key, err)
		}
		if catchUpSuperseded(*payload.CatchUp, r.supervisedClaim(root), &tree.root) {
			r.log.Info("outbox catch-up finished without publishing: a newer catch-up supersedes it", "row", row.ID, "issue", row.Issue,
				"generation", payload.CatchUp.Generation, "launch", payload.CatchUp.Launch)
			return nil
		}
	}
	runs := func(token claim.Token) bool { return claimRuns(r.claimState(token)) }
	architect, err := owningArchitect(tree.project, tree.issues, issue, payload.Kind, runs)
	if err != nil {
		return fmt.Errorf("the architect of %s: %w", row.Issue, err)
	}
	if state := r.claimState(architect); stoppedWithTree(tree.root.Lingers(), state) {
		r.log.Info("outbox notice finished undelivered: its architect stopped with its finished tree",
			"row", row.ID, "kind", payload.Kind, "issue", row.Issue, "architect", architect, "state", state)
		return nil
	}
	if earlier := earlierNoticeFor(tree, architect, runs); earlier != 0 {
		return fmt.Errorf("%w: %s's notice row %d waits behind its row %d", errNoticeWaits, architect, row.ID, earlier)
	}
	notice, key := payload.Published(row.ID)
	published := r.notices.Publish(ctx, notify.RoleTopicPrefix+string(architect), noticeSummary(payload.Kind, row.Issue), notice, key)
	switch {
	case published == nil:
		return nil
	case !errors.Is(published, notify.ErrNoHolder):
		return fmt.Errorf("publish notice for %s to its architect %s: %w", row.Issue, architect, published)
	}
	if ended := architectEnded(tree, r.claimState(architect)); ended != "" {
		r.log.Info("outbox notice finished undelivered: its architect will not hold its role again",
			"row", row.ID, "kind", payload.Kind, "issue", row.Issue, "architect", architect, "because", ended)
		return nil
	}
	return fmt.Errorf("%w: %s holds no role for %s: %w", errNoticeWaits, architect, row.Issue, published)
}

// mergeQueue publishes the merger's READY packet to the project's merge queue role, keyed by the
// row so a retried row is one delivery. A role with no live holder refuses every attempt until
// someone claims it, so waiting would retry forever; the Dispatch message the same READY posted
// already carries the packet, so the issue is told the role had no holder, as the shared merger
// prompt has the merger say (packages/pi-envoy/roles/merger.md step 4), and the row is done.
func (r *outbox) mergeQueue(ctx context.Context, row record.OutboxRow, payload record.MergeQueuePublish) error {
	if r.notices == nil {
		return errors.New("merge queue executor has no Envoy publisher")
	}
	err := r.notices.Publish(ctx, notify.RoleTopicPrefix+payload.Role, payload.Packet, payload.Packet, record.OutboxKey(row.ID))
	if errors.Is(err, notify.ErrNoHolder) {
		return r.message(ctx, row, record.MessagePost{Body: fmt.Sprintf("merge queue role %s had no live holder at %s", payload.Role, r.now().UTC().Format(time.RFC3339))})
	}
	if err != nil {
		return fmt.Errorf("publish READY for %s to merge queue role %s: %w", row.Issue, payload.Role, err)
	}
	return nil
}

// controllerNotice publishes a controller notice row to the controller topic of the daemon's own
// project, the one its controller subscribes to, and to nothing else: the Notice it is, under the
// row's own key.
func (r *outbox) controllerNotice(ctx context.Context, row record.OutboxRow, payload record.ControllerNotice) error {
	if r.notices == nil {
		return errors.New("controller notice executor has no Envoy publisher")
	}
	if err := r.notices.Publish(ctx, notify.ControllerTopic(r.project), noticeSummary(payload.Kind, row.Issue), record.Notice(payload), record.OutboxKey(row.ID)); err != nil {
		return fmt.Errorf("publish controller notice for %s: %w", row.Issue, err)
	}
	return nil
}

func (r *outbox) supervise(ctx context.Context, row record.OutboxRow, payload record.SuperviseRequest) error {
	if r.supervisor == nil {
		return errors.New("supervise executor has no claim supervisor")
	}
	issue, err := r.issue(ctx, row.Issue)
	if err != nil {
		return err
	}
	if payload.Generation != issue.Generation {
		r.log.Info("outbox supervise row serves an earlier generation; finished without acting", "row", row.ID, "issue", issue.Key,
			"generation", payload.Generation, "current", issue.Generation, "op", payload.Op, "role", payload.Role)
		return nil
	}
	if payload.Phase != "" && payload.Phase != issue.Phase {
		r.log.Info("outbox start serves a phase the issue has left; finished without acting", "row", row.ID, "issue", issue.Key,
			"phase", payload.Phase, "current", issue.Phase, "role", payload.Role)
		return nil
	}
	if payload.Tree != issue.Tree {
		return fmt.Errorf("supervise row %d tree %s does not match issue %s tree %s", row.ID, payload.Tree, issue.Key, issue.Tree)
	}
	token, err := claim.NewToken(r.project, row.Issue, payload.Role)
	if err != nil {
		return fmt.Errorf("derive claim for outbox row %d: %w", row.ID, err)
	}
	machine, found := r.supervisor.Machine(token)
	switch payload.Op {
	case "start":
		// A tree that lingers has left the workflow, and linger holds each member where it stood: a
		// start that reaches its claim after the close (queued before it and backing off) finishes
		// without acting and records no start, so the close's suspend still applies. Re-admission
		// starts the member again (admit's startMidPhaseChildren).
		root, err := r.root(ctx, issue)
		if err != nil {
			return err
		}
		if root != nil && root.Lingers() {
			r.log.Info("outbox start of a member of a lingering tree; finished without acting", "row", row.ID, "issue", issue.Key,
				"tree", issue.Tree, "role", payload.Role)
			return nil
		}
		// A start that takes the phase over from a role still at work in it (CI settled red before
		// that role completed) does nothing — no claim, no working-copy adoption, no task — until
		// that role's turn is over (supervise.Machine.Quiesce), so the two never write the shared
		// workspace together. A retry of a start that has already run against its claim (the claim's
		// newest start, which StartedBy persists) is past that wait.
		if payload.Quiesce != "" && (!found || machine.Claim().LastStartRow < row.ID) {
			if err := r.quiesce(ctx, row, issue.Key, payload.Quiesce); err != nil {
				return err
			}
		}
		if !found {
			if err := r.provisionWorkspace(ctx, issue); err != nil {
				return err
			}
			machine, _, err = r.supervisor.Create(ctx, supervise.Claim{
				Token: token, Project: r.project, Tree: issue.Tree, Issue: issue.Key, Role: payload.Role, State: supervise.StateQueued,
			}, "")
			if err != nil {
				return fmt.Errorf("create claim %s: %w", token, err)
			}
		}
		// The claim remembers the newest start run against it, so a stop written before this one
		// is finished rather than acted on however late it arrives (see "suspend" below). A claim
		// this row created has no older stop to fence, and recording it here rather than only for
		// a claim that already existed keeps one rule instead of a special case.
		if err := machine.StartedBy(ctx, row.ID); err != nil {
			return fmt.Errorf("record the start of claim %s: %w", token, err)
		}
		state := machine.Claim().State
		switch state {
		case supervise.StateQueued:
			if err := machine.Handle(ctx, supervise.RequestSpawn{Claim: token}); err != nil {
				return fmt.Errorf("start claim %s: %w", token, err)
			}
		case supervise.StateSuspended:
			if err := machine.Handle(ctx, supervise.RequestResume{Claim: token}); err != nil {
				return fmt.Errorf("resume claim %s: %w", token, err)
			}
		case supervise.StateFailed:
			// Only the workflow starts a role whose claim failed: the architect retrying the
			// held phase, or a later transition that needs the role again.
			if err := machine.Handle(ctx, supervise.RequestRetry{Claim: token}); err != nil {
				return fmt.Errorf("retry claim %s: %w", token, err)
			}
		case supervise.StateRetired:
			// A retired claim's tree closed, and its linger removed the workspace; the tree was
			// re-admitted, so the workspace comes back before the kept session relaunches.
			if err := r.provisionWorkspace(ctx, issue); err != nil {
				return err
			}
			if err := machine.Handle(ctx, supervise.RequestRetry{Claim: token}); err != nil {
				return fmt.Errorf("relaunch retired claim %s: %w", token, err)
			}
		}
		// A start that finds a tree's root architect running is a re-admission while its session
		// lived on. The linger suspends the root's claim, so that happens only when the claim was
		// mid-launch when the suspend ran (the machine ignores a suspend while launching), or the
		// re-admission's start ran before the suspend. No ready follows, and a ready is what tells
		// the architect its tree (workflow's claimReady), so the start applies that fact itself,
		// for the running launch, once per start row, and the architect is told its tree's new
		// generation.
		if claimTookRole(state) && claim.IsTreeArchitect(payload.Role, issue.Key, issue.Tree) {
			if _, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("start-running:%s:%d", token, row.ID),
				intake.ClaimReady{Issue: issue.Key, Role: payload.Role, Launch: machine.Claim().Generation}, r.handlers...); err != nil {
				return fmt.Errorf("tell the running architect of %s its tree: %w", issue.Key, err)
			}
		}
		// A claim that, once relaunched, still holds a task of this row's phase and run that it will
		// work is given that task: one not yet confirmed (a failed claim keeps the task it held, and a
		// death in a turn takes the task back unconfirmed, behind the sentence saying so), or one
		// confirmed in the turn the claim is working. The row's own task would follow it as a second
		// prompt for work already under way, so it is not delivered when it is the phase's resume task
		// (promotion's) or the row relaunched a failed claim (the architect's retry of the held phase,
		// or a later transition). A confirmed task on a claim not working is over: the relaunch's
		// decision retired it here, or retires it first when a launch whose outcome was uncertain is
		// released and spawned again, after this start, so the row's task goes in its place.
		held := machine.Claim()
		if pending := held.Pending; (payload.ResumeTask || state == supervise.StateFailed) && pending != nil && payload.Phase != "" &&
			pending.StillWorked(payload.Generation, payload.Phase, held.State) {
			r.log.Info("outbox start of a claim that holds its phase's task; not delivered again", "row", row.ID, "issue", issue.Key,
				"role", payload.Role, "phase", payload.Phase, "held", pending.ID)
			return nil
		}
		if payload.Task != "" {
			// The row's phase, already held to the issue's above, travels with the delivery: it is
			// what says the task is still the work to do once the delivery has outlived its id.
			if err := machine.Handle(ctx, supervise.RequestDeliver{Claim: token, Task: payload.Task, Phase: payload.Phase, Generation: payload.Generation,
				ID: fmt.Sprintf("%s%d", outboxDeliveryPrefix, row.ID)}); err != nil {
				return fmt.Errorf("deliver to claim %s: %w", token, err)
			}
		}
		return nil
	case "suspend":
		if !found {
			return nil
		}
		switch machine.Claim().State {
		case supervise.StateFailed, supervise.StateRetired:
			// The claim runs nothing, so there is nothing to suspend.
			return nil
		}
		// Whether the suspend still acts is workflow.StopActs's rule, which promotion reads too.
		if last := machine.Claim().LastStartRow; !workflow.StopActs(row.ID, payload, last, nil) {
			r.log.Info("outbox suspend superseded by a newer start; finished without acting",
				"row", row.ID, "issue", issue.Key, "role", payload.Role, "phase", issue.Phase, "start-row", last)
			return nil
		}
		if err := machine.Handle(ctx, supervise.RequestSuspend{Claim: token, Reason: payload.Reason}); err != nil {
			return fmt.Errorf("suspend claim %s: %w", token, err)
		}
		return nil
	case "tree_close":
		if !found {
			return nil
		}
		// The close belongs to the linger it expired, the root generation it names
		// (workflow.StopActs). Once re-admission ends that linger, a close still backing off (a
		// release the runtime refused) would retire the claim the tree's new run relaunched, and the
		// task it holds with it, even in a later linger of the tree; it finishes without acting.
		root, err := r.root(ctx, issue)
		if err != nil {
			return err
		}
		if !workflow.StopActs(row.ID, payload, machine.Claim().LastStartRow, root) {
			r.log.Info("outbox tree close of a linger that has ended; finished without acting", "row", row.ID, "issue", issue.Key,
				"tree", issue.Tree, "role", payload.Role, "linger", payload.Linger)
			return nil
		}
		if err := machine.Handle(ctx, supervise.RequestTreeClose{Claim: token}); err != nil {
			return fmt.Errorf("close the tree of claim %s: %w", token, err)
		}
		if err := r.cleanupIssueResources(ctx, issue); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("outbox row %d has unknown supervise operation %q", row.ID, payload.Op)
	}
}

// cleanupIssueResources is the durable whole-issue effect after a tree-close row retired one
// claim. It acts only once every persisted sibling claim of this exact issue is retired; a later
// re-admission leaves one launching/active claim and fences an old close before the runtime ever
// sees a delete. Runtime Release remains role-scoped.
func (r *outbox) cleanupIssueResources(ctx context.Context, issue record.Issue) error {
	cleaner, ok := r.supervisor.deps.Runtime.(runtime.IssueResourceCleaner)
	if !ok {
		return nil
	}
	claims, err := r.supervisor.Claims(ctx)
	if err != nil {
		return fmt.Errorf("read sibling claims before cleanup of %s: %w", issue.Key, err)
	}
	found := false
	for _, sibling := range claims {
		if sibling.Project != r.project || sibling.Issue != issue.Key {
			continue
		}
		found = true
		if sibling.State != supervise.StateRetired {
			return nil
		}
	}
	if !found {
		return nil
	}
	if err := cleaner.CleanupIssue(ctx, r.project, issue.Key); err != nil {
		return fmt.Errorf("cleanup resources of closed issue %s: %w", issue.Key, err)
	}
	return nil
}

// root is issue's tree root as recorded, nil when it is not, read in a transaction of its own
// just before the executor acts on a row.
func (r *outbox) root(ctx context.Context, issue record.Issue) (*record.Issue, error) {
	var root *record.Issue
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		root, err = r.records.Issue(ctx, tx, issue.Tree)
		return err
	}); err != nil {
		return nil, fmt.Errorf("read the tree root of %s: %w", issue.Key, err)
	}
	return root, nil
}

// quiesce asks the claim of role on issue, whose phase the start row takes over, to be out of its
// turn (supervise.Machine.Quiesce): nil once it is, supervise.ErrQuiesceHeld while it is not, which
// the runner retries on its backoff. A role with no claim runs nothing.
func (r *outbox) quiesce(ctx context.Context, row record.OutboxRow, issue string, role claim.Role) error {
	token, err := claim.NewToken(r.project, issue, role)
	if err != nil {
		return fmt.Errorf("derive the claim outbox row %d takes over from: %w", row.ID, err)
	}
	outgoing, found := r.supervisor.Machine(token)
	if !found {
		return nil
	}
	if err := outgoing.Quiesce(ctx, row.ID); err != nil {
		return err
	}
	if row.Attempts > 0 {
		r.log.Info("outbox start goes on: the claim it takes the phase from is out of its turn", "row", row.ID, "issue", issue, "role", role)
	}
	return nil
}

// podsProvision is whether the runtime provisions each claim's workspace in the claim's own pod
// (C4): then the daemon provisions and removes none on its host.
func (r *outbox) podsProvision() bool { return r.supervisor.deps.Runtime.ProvisionsWorkspaces() }

func (r *outbox) provisionWorkspace(ctx context.Context, issue record.Issue) error {
	if r.podsProvision() {
		return nil
	}
	if r.tokens == nil {
		return errors.New("supervise executor has no GitHub App token manager")
	}
	if r.repo.IsZero() {
		return errors.New("workspace provisioning has no configured repository")
	}
	lease, err := r.tokens.Token(ctx, appauth.Implement, r.repo.Owner())
	if err != nil {
		return fmt.Errorf("mint implement App token to provision %s: %w", issue.Key, err)
	}
	if _, err := r.provision(ctx, workspace.Request{
		StateDir: r.stateDir, Repo: r.repo, Issue: issue.Key, CredentialHelper: credentialHelper(r.stateDir),
		Source: workspace.FromGitHub(lease.Token, r.stateDir),
		Log:    func(line string) { r.log.Warn("provisioning: "+line, "issue", issue.Key) },
	}); err != nil {
		return fmt.Errorf("provision workspace for %s: %w", issue.Key, err)
	}
	return nil
}

func (r *outbox) seedGate(ctx context.Context, row record.OutboxRow, payload record.GateSeed) error {
	if r.dispatch == nil {
		return errors.New("gate seed executor has no Dispatch client")
	}
	approval, err := r.dispatch.Approval(ctx, payload.ArtifactID)
	if err != nil {
		return fmt.Errorf("read Dispatch approval %s: %w", payload.ArtifactID, err)
	}
	// The version Dispatch holds is the current one, even one the architect did not see: approved
	// there, the gate opens at it; otherwise a later version still moves the gate to it, closed.
	fact := intake.DispatchArtifact{Key: row.Issue, ArtifactID: payload.ArtifactID, Kind: intake.DispatchArtifactVersion, Version: approval.LatestVersion}
	switch {
	case approval.State == "approved" && approval.Version != nil && *approval.Version == approval.LatestVersion:
		fact.Kind = intake.DispatchArtifactApproved
	case approval.LatestVersion <= payload.Version:
		return nil
	}
	_, err = intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("gate-seed:%s:%d:%s:%s:%d", row.Issue, payload.Generation, payload.ArtifactID, fact.Kind, fact.Version), fact, r.handlers...)
	if err != nil {
		return fmt.Errorf("apply approved gate seed for %s: %w", row.Issue, err)
	}
	return nil
}

func (r *outbox) linger(ctx context.Context, row record.OutboxRow, payload record.LingerClose) error {
	_, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("linger:%s:%d", row.Issue, payload.Generation), intake.LingerExpired{
		Issue: row.Issue, Generation: payload.Generation,
	}, r.handlers...)
	if err != nil {
		return fmt.Errorf("apply linger expiration for %s: %w", row.Issue, err)
	}
	return nil
}

func (r *outbox) removeWorkspace(ctx context.Context, row record.OutboxRow, payload record.WorkspaceRemove) error {
	if r.supervisor == nil {
		return errors.New("workspace removal has no claim supervisor")
	}
	if r.podsProvision() {
		return nil
	}
	if r.repo.IsZero() {
		return errors.New("workspace removal has no configured repository")
	}
	issue, err := r.issue(ctx, row.Issue)
	if err != nil {
		return err
	}
	// The removal belongs to the linger it expired, the root generation it names. Once
	// re-admission ends that linger, the tree's new run may already work in the workspace again,
	// even in a later linger of the tree, so a removal still backing off finishes without acting.
	root, err := r.root(ctx, issue)
	if err != nil {
		return err
	}
	if root == nil || !root.LingersAt(payload.Linger) {
		r.log.Info("outbox workspace removal of a linger that has ended; finished without acting", "row", row.ID, "issue", issue.Key,
			"tree", issue.Tree, "linger", payload.Linger)
		return nil
	}
	// The workspace goes only once the close has retired every claim of the issue: a claim whose
	// release the runtime refused still runs there, and one that is re-admitted before its close
	// lands goes on in it. A retired claim's relaunch provisions the workspace again.
	for _, role := range claim.Roles {
		token, err := claim.NewToken(r.project, row.Issue, role)
		if err != nil {
			return fmt.Errorf("derive claim for outbox row %d: %w", row.ID, err)
		}
		if machine, found := r.supervisor.Machine(token); found && machine.Claim().State != supervise.StateRetired {
			return fmt.Errorf("workspace removal of %s waits for the tree's close to retire claim %s (%s)", row.Issue, token, machine.Claim().State)
		}
	}
	working, err := workspace.Location(r.stateDir, r.repo, row.Issue)
	if err != nil {
		return fmt.Errorf("locate workspace for %s: %w", row.Issue, err)
	}
	if err := r.remove(ctx, working); err != nil {
		return fmt.Errorf("remove workspace for %s: %w", row.Issue, err)
	}
	return nil
}

func (r *outbox) issue(ctx context.Context, key string) (record.Issue, error) {
	var issue *record.Issue
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		issue, err = r.records.Issue(ctx, tx, key)
		return err
	}); err != nil {
		return record.Issue{}, fmt.Errorf("read workflow issue %s: %w", key, err)
	}
	if issue == nil {
		return record.Issue{}, fmt.Errorf("workflow issue %s is not recorded", key)
	}
	return *issue, nil
}

// credentialHelper is the git credential helper every issue workspace names: this daemon's pane
// launcher by its absolute path, so a push from any directory reaches `legion credential`.
func credentialHelper(stateDir string) string {
	return "!" + shellprefix.Literal(filepath.Join(workerbin.LauncherDir(stateDir), "legion")) + " credential"
}
