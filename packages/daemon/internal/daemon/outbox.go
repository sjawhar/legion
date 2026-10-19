package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghautomerge"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/wait"
	"github.com/sjawhar/legion/daemon/internal/workflow"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const (
	// outboxDeliveryPrefix namespaces a delivery id the outbox mints from its row id, so a row's
	// id cannot collide with a minted one. What a delivery is for is its phase, not its id: the
	// id is rotated by a prompt retry.
	outboxDeliveryPrefix = "outbox:"
	// outboxLease is how long a claim holds a row, and the sweep's interval (outbox_lanes.go): a
	// running row renews its lease every third of it, so a lease lapses only on a run that ended
	// without its finish or retry recorded, and the sweep finds the lapsed row within one more.
	outboxLease = 30 * time.Second
	// outboxSettleTimeout bounds a row's finish or retry once the daemon's stop has cancelled the
	// lane's context: the row's effect ran, and a settle that waits for the stop's context would
	// never be recorded, leaving the row to run again at the next boot as though it had not.
	outboxSettleTimeout = 5 * time.Second
	// heldWarnAfter is how long a due lifecycle row waits behind its issue's running lifecycle row
	// before the running row's lease renewal logs it: a start pulling an image holds the stop
	// behind it for a minute without anything being wrong, and longer than this an operator
	// should look at what the running row is doing.
	heldWarnAfter   = 2 * time.Minute
	messageReadSkew = 5 * time.Second
	// waitWarnAttempts is when a waiting row is logged as a warning: past the backoff's climb to
	// its cap, a wait of about two minutes, what it waits on is not ending on its own, and an
	// operator should look. A READY merge only GitHub's own rules hold back (routineWait) stays
	// debug at every attempt.
	waitWarnAttempts = 7
	// issueBranchTimeout bounds one issue_branch row's GitHub calls, its token's mint with them:
	// the row runs on its issue's lifecycle lane, one row at a time, so a call GitHub never
	// answers would hold the lane and every later lifecycle row of the issue behind it — the
	// starts that wait for the branch first of all.
	issueBranchTimeout = 30 * time.Second
)

// outbox runs each effect that the workflow transaction committed, one lane per issue and kind of
// work (outbox_lanes.go). Claiming and finishing have separate transactions so a slow external
// service never holds an issue transition lock.
type outbox struct {
	pool       *pgxpool.Pool
	records    record.Store
	dispatch   dispatch.Client
	notices    notify.Publisher
	supervisor *supervisor
	trees      outboxTrees
	tokens     appauth.Tokens
	handlers   []intake.Handler
	project    string
	// dispatchProject is the Dispatch project whose issues' rows this outbox claims: the database
	// is shared, and another project's rows are another daemon's.
	dispatchProject string
	stateDir        string
	log             *slog.Logger
	// now is the moment a row is listed, claimed, renewed and retried at; clock arms the lanes'
	// timers — the sweep's, a lane's wait for its next due row, a running row's lease renewal.
	// Both are the wall clock in production, and a test holds both still (heldClock). A test that
	// builds the outbox by hand and only drains it (RunOnce) leaves clock nil, which is the wall
	// clock (timers).
	now       func() time.Time
	clock     supervise.Clock
	provision func(context.Context, workspace.Request) (workspace.Workspace, error)
	remove    func(context.Context, workspace.Workspace) error
	// provisionMu runs the workspace provisions and removals one at a time: every issue's
	// workspace is a jj workspace of its repository's shared clone, and the clone's `jj git
	// fetch` and `workspace add` corrupt it when two run at once. Lanes run issues side by side,
	// so without it two issues' starts could both be provisioning. One lock serves every
	// repository's clone.
	provisionMu sync.Mutex
	// githubAPI is the GitHub REST root an issue_branch row creates its issue's branch under: the
	// workflow's (workflowRuntime.githubAPI), which empty, in production, is https://api.github.com.
	githubAPI string
	// limits is every GitHub rate limit a row met, by the budget it exhausted, held until its
	// deadline: a row that spends one of those budgets waits for the latest deadline among them
	// (standingLimit) rather than meet a limit again. The lanes run rows on many goroutines, so
	// limitsMu guards it (deferral, noteLimit).
	limitsMu sync.Mutex
	limits   map[githubrest.Budget]rateLimit
	// The lane registry (outbox_lanes.go). mu guards running, laneCtx and lanes; running is true
	// between Run's start and its stop, laneCtx is Run's context, which every lane runs under, and
	// lanes is each live lane by its key. wg counts the lane goroutines, so Run returns only once
	// every lane has.
	mu      sync.Mutex
	running bool
	laneCtx context.Context
	lanes   map[record.LaneKey]*outboxLane
	wg      sync.WaitGroup
}

// outboxTrees is the store's tree barrier (store.Store) as the outbox runs it: an issue close's
// suspension, a stop's wait for a stored claim's machine, and a workflow tree close's cleanup.
type outboxTrees interface {
	IssueSuspension(ctx context.Context, project string, closing store.IssueClose, outOfWorkflow func(status string) bool) (bool, error)
	StoredClaimMayRun(ctx context.Context, token claim.Token, row int64) (bool, error)
	ReserveWorkflowTreeCleanup(ctx context.Context, project, tree string, rootGeneration uint64) (treelifecycle.Lifecycle, bool, error)
	CleanupReservedTree(ctx context.Context, project, tree string, epoch uint64, releaser store.TreeReleaser) error
}

func newOutbox(pool *pgxpool.Pool, records record.Store, client dispatch.Client, publisher notify.Publisher, supervisor *supervisor, trees outboxTrees, tokens appauth.Tokens, handlers []intake.Handler, project, dispatchProject, stateDir, githubAPI string, tools map[string]string, log *slog.Logger) *outbox {
	if log == nil {
		log = slog.Default()
	}
	return &outbox{
		pool: pool, records: records, dispatch: client, notices: publisher, supervisor: supervisor, trees: trees, tokens: tokens,
		handlers: handlers, project: project, dispatchProject: dispatchProject, stateDir: stateDir, githubAPI: githubAPI,
		log: log, now: time.Now, clock: supervise.RealClock{},
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

// RunOnce drains every row of the project due now, in the order the lanes would run them within
// one issue (next_at, then id), on the caller's goroutine: the tests' synchronous drain. Production
// runs the lanes (Run, outbox_lanes.go), which this never starts. Each row is claimed by id as its
// turn comes (runRow), so a row another runner has leased since the list is skipped; and the list
// is made once, so a row that a listed row's finish makes claimable — an issue's second status
// write, once its first is done — runs on the next drain, as it did when the drain claimed a batch.
func (r *outbox) RunOnce(ctx context.Context) error {
	if r.log == nil {
		r.log = slog.Default()
	}
	if r.pool == nil || r.records == nil {
		return errors.New("outbox requires Postgres and its record store")
	}
	rows, err := r.listDue(ctx, "", nil, r.now().UTC())
	if err != nil {
		return fmt.Errorf("list due outbox rows: %w", err)
	}
	for _, row := range rows {
		// A claim that fails is logged by runRow and its row left for the next drain, as a lane leaves
		// it for the next wake or sweep.
		_ = r.runRow(ctx, row)
	}
	return nil
}

// settle records what a row's run came to, under the lease the run held: a wait or a failure is
// retried on the row's backoff (or after the wait GitHub asks for), a failure no retry repairs is
// finished refused with its follow-up (permanentRefusal, finishRefused), and anything else is
// finished. A row whose finish or retry fails is logged and left leased: its lease expires and the
// row is due again, so one row's failed transaction never stops the rows after it.
func (r *outbox) settle(ctx context.Context, row record.OutboxRow, err error) {
	if err != nil {
		r.noteLimit(row, err)
		// A failure no retry repairs is not logged as the row failing: once its finish commits,
		// finishRefused's own line says what became of the row (a person's dequeue of a READY merge,
		// say, is no failure of the daemon's), and a finish that does not commit is logged below with
		// it as the cause. A wait that ends the row (a stalled ghautomerge.Lagging) logs as a wait too.
		permanent := permanentRefusal(row, err, r.now().UTC())
		if errors.Is(err, wait.ErrWaiting) {
			// A wait is not a failure (wait.ErrWaiting): a held notice, a suspension or start held
			// for a turn, a task behind the claim's pending delivery, a tree's cleanup reservation,
			// a close waiting for its stops, or a READY merge GitHub does not allow, or has not
			// made, yet. The row runs again on its backoff, logged when it first waits and as a
			// warning once it has waited long, unless only GitHub's own rules hold it back.
			level := slog.LevelDebug
			switch {
			case row.Attempts == 0:
				level = slog.LevelInfo
			case row.Attempts >= waitWarnAttempts && !routineWait(err):
				level = slog.LevelWarn
			}
			r.log.Log(ctx, level, "outbox row waits", "row", row.ID, "kind", row.Kind, "issue", row.Issue,
				"attempts", row.Attempts, "error", err)
		} else if !permanent {
			r.log.Error("outbox row failed", "row", row.ID, "kind", row.Kind, "error", err)
		}
		if permanent {
			if finishErr := r.finishRefused(ctx, row, err); finishErr != nil {
				r.log.Error("outbox row finish not recorded; it runs again when its lease expires", "row", row.ID, "error", finishErr, "cause", err)
			}
			return
		}
		if err := r.retry(ctx, row, err); err != nil {
			r.log.Error("outbox row retry not recorded; it runs again when its lease expires", "row", row.ID, "error", err)
		}
		return
	}
	if err := r.finish(ctx, row); err != nil {
		r.log.Error("outbox row finish not recorded; it runs again when its lease expires", "row", row.ID, "error", err)
	}
}

// deferral is the GitHub rate limit a row of kind would meet at now, if one stands (standingLimit),
// read under limitsMu: the lanes run rows on many goroutines, and a limit one row met holds every
// row of the kinds that spend its budget, whichever lane runs them.
func (r *outbox) deferral(kind record.OutboxKind, now time.Time) (rateLimit, bool) {
	r.limitsMu.Lock()
	defer r.limitsMu.Unlock()
	return standingLimit(r.limits, kind, now)
}

// noteLimit records the GitHub rate limit err carries, when it does, under the budget it exhausted:
// the latest deadline for a budget wins.
func (r *outbox) noteLimit(row record.OutboxRow, err error) {
	after, budget := githubrest.RetryAfter(err)
	if after <= 0 {
		return
	}
	until := r.now().UTC().Add(after)
	r.limitsMu.Lock()
	defer r.limitsMu.Unlock()
	if r.limits == nil {
		r.limits = map[githubrest.Budget]rateLimit{}
	}
	if held, ok := r.limits[budget]; !ok || until.After(held.until) {
		r.limits[budget] = rateLimit{row: row.ID, err: err, until: until}
	}
}

func (r *outbox) finish(ctx context.Context, row record.OutboxRow) error {
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.FinishOutbox(ctx, tx, row.ID, row.LeaseToken)
	}); err != nil {
		return fmt.Errorf("finish outbox row %d: %w", row.ID, err)
	}
	return nil
}

// finishRefused finishes a row whose failure no retry repairs (permanentRefusal), its follow-up in
// the same transaction and only while the row's lease still holds (FinishLeasedOutbox), so a worker
// whose lease expired, or was taken by a reopen (record's readyMergeConflict), commits nothing
// beside the one that holds the row now.
//
// A status row Dispatch refused finishes so it no longer holds its issue's later writes back. A
// hand-over's row drops its fence with it: the write that would hand the child to Legion never
// lands, so no admission consumes the fence, and one left behind would keep naming the child's own
// tree as its Sandbox's home (store.IssueSandboxHome) for good. The child stays where it is until
// a person labels it, which admits it as an orphan. A READY merge row tells the issue's architect
// why it ended without a merge (readyMergeEnded): its notice row is written in the finish's own
// transaction, which owns the commit hook that write registers (projectRecords, intake.OnCommit),
// so the notice's lane is woken as the finish commits and the notice is published without a sweep.
func (r *outbox) finishRefused(ctx context.Context, row record.OutboxRow, cause error) error {
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		return fmt.Errorf("decode outbox row %d: %w", row.ID, err)
	}
	scoped, committed := intake.WithCommitHooks(ctx)
	// follow is what the transaction does beside the finish, and told the line logged once it commits.
	var follow func(pgx.Tx) error
	var told func()
	switch value := payload.(type) {
	case record.StatusWrite:
		refusal, _ := dispatch.PermanentRefusal(cause)
		r.log.Error("outbox status write refused by Dispatch and dropped; the issue's later writes go on",
			"row", row.ID, "issue", row.Issue, "status", refusal.Status, "code", refusal.Code, "error", cause)
		if value.HandOver {
			follow = func(tx pgx.Tx) error {
				return r.records.DeleteHandoverAt(scoped, tx, row.Issue, value.HandOverTree, value.HandOverLinger)
			}
			told = func() {
				r.log.Warn("outbox hand-over refused by Dispatch; its fence is dropped and the child is not handed to Legion",
					"row", row.ID, "issue", row.Issue, "tree", value.HandOverTree, "linger", value.HandOverLinger)
			}
		}
	case record.ReadyMerge:
		notice, err := record.NewOutboxRow(row.Issue, readyMergeEnded(cause), r.now().UTC())
		if err != nil {
			return err
		}
		follow = func(tx pgx.Tx) error { return r.records.Enqueue(scoped, tx, notice) }
		// Someone removing the arm is a person's act, told to the architect; GitHub refusing the
		// submission or a webhook the daemon never received is a fault.
		level := slog.LevelError
		var removed *ghautomerge.Removed
		if errors.As(cause, &removed) {
			level = slog.LevelWarn
		}
		told = func() {
			r.log.Log(scoped, level, "outbox READY merge ended without a merge; its architect is told", "row", row.ID, "issue", row.Issue, "error", cause)
		}
	}
	if err := pgx.BeginFunc(scoped, r.pool, func(tx pgx.Tx) error {
		if follow != nil {
			if err := follow(tx); err != nil {
				return err
			}
		}
		return r.records.FinishLeasedOutbox(scoped, tx, row.ID, row.LeaseToken)
	}); err != nil {
		return fmt.Errorf("finish outbox row %d: %w", row.ID, err)
	}
	committed()
	if told != nil {
		told()
	}
	return nil
}

// retry returns row to the outbox, due again after its backoff, or after the wait GitHub asks for
// when cause carries GitHub's rate limit and asks for longer.
func (r *outbox) retry(ctx context.Context, row record.OutboxRow, cause error) error {
	delay := retryBackoff(row.Attempts)
	if after, _ := githubrest.RetryAfter(cause); after > delay {
		delay = after
	}
	return r.retryAt(ctx, row, r.now().UTC().Add(delay), cause)
}

// retryAt returns row to the outbox, due again at nextAt, with cause as its last error.
func (r *outbox) retryAt(ctx context.Context, row record.OutboxRow, nextAt time.Time, cause error) error {
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.RetryOutbox(ctx, tx, row.ID, row.LeaseToken, nextAt, cause.Error())
	}); err != nil {
		return fmt.Errorf("retry outbox row %d: %w", row.ID, err)
	}
	return nil
}

// rateLimit is a GitHub rate limit a row met: the row, its error, and when the limit ends.
type rateLimit struct {
	row   int64
	err   error
	until time.Time
}

// githubBudgets is the GitHub budgets a row of each kind spends, as the implement App: its REST
// calls (an issue branch's create) the REST core's, its GraphQL calls (a disarm, a READY merge)
// GraphQL's, and every call GitHub's secondary limit. A kind it does not name calls no GitHub API.
var githubBudgets = map[record.OutboxKind][]githubrest.Budget{
	record.OutboxKindIssueBranch:      {githubrest.BudgetCore, githubrest.BudgetSecondary},
	record.OutboxKindAutoMergeDisable: {githubrest.BudgetGraphQL, githubrest.BudgetSecondary},
	record.OutboxKindReadyMerge:       {githubrest.BudgetGraphQL, githubrest.BudgetSecondary},
}

// standingLimit is the limit in limits, among the budgets a row of kind spends, that still stands
// at now and ends last, and whether there is one: that row would meet a limit until then.
func standingLimit(limits map[githubrest.Budget]rateLimit, kind record.OutboxKind, now time.Time) (rateLimit, bool) {
	var latest rateLimit
	for _, budget := range githubBudgets[kind] {
		if limit, ok := limits[budget]; ok && limit.until.After(now) && limit.until.After(latest.until) {
			latest = limit
		}
	}
	return latest, !latest.until.IsZero()
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

// permanentRefusal is a row's failure no retry repairs, so the row is finished with its follow-up
// (finishRefused) rather than retried: a Dispatch status write Dispatch refuses the same way every
// time (dispatch.PermanentRefusal), which would hold every later status of its issue behind it, and
// a READY merge GitHub refuses where it still stands (ghautomerge.Refused), whose pull request
// GitHub has shown closed or at another head than the record awaits for longer than
// ghautomerge.LagBound at now (ghautomerge.Lagging.Stalled), or whose arm or merge-queue entry
// someone else removed without a merge (ghautomerge.Removed), which a retry must not undo.
func permanentRefusal(row record.OutboxRow, err error, now time.Time) bool {
	switch row.Kind {
	case record.OutboxKindDispatchStatus:
		_, permanent := dispatch.PermanentRefusal(err)
		return permanent
	case record.OutboxKindReadyMerge:
		var refused *ghautomerge.Refused
		var removed *ghautomerge.Removed
		var lagging *ghautomerge.Lagging
		return errors.As(err, &refused) || errors.As(err, &removed) || errors.As(err, &lagging) && lagging.Stalled(now)
	default:
		return false
	}
}

// routineWait is a wait only GitHub's own rules hold back, which ends without anyone acting for
// Legion: a pull request armed or queued at READY's head (ghautomerge.Armed), one GitHub reports not
// mergeable yet for a reason its rules end (ghautomerge.Unmergeable.Routine), or one the merge queue
// took out to merge, which GitHub has yet to show merged (ghautomerge.Merging).
func routineWait(err error) bool {
	var routine interface{ Routine() bool }
	return errors.As(err, &routine) && routine.Routine()
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
	case record.IssueSuspend:
		if r.supervisor == nil {
			return errors.New("issue suspension executor has no claim supervisor")
		}
		suspender, ok := r.supervisor.deps.Runtime.(runtime.IssueSuspender)
		if !ok {
			return nil
		}
		closing := store.IssueClose{Issue: row.Issue, Tree: value.Tree, IssueGeneration: value.Generation,
			TreeGeneration: value.TreeGeneration, Row: row.ID, Release: value.Release}
		return suspender.SuspendIssue(ctx, row.Issue, value.Tree, value.Release, func(ctx context.Context) (bool, error) {
			return r.trees.IssueSuspension(ctx, r.project, closing, record.OutOfWorkflow)
		})
	case record.WorkspaceRemove:
		return r.removeWorkspace(ctx, row, value)
	case record.MergeQueuePublish:
		return r.mergeQueue(ctx, row, value)
	case record.IssueBranch:
		return r.issueBranch(ctx, row, value)
	case record.AutoMergeDisable:
		return r.autoMergeDisable(ctx, row, value)
	case record.ReadyMerge:
		return r.readyMerge(ctx, row, value)
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
		if payload.HandOver {
			if err := r.dropHandover(ctx, row.Issue, payload); err != nil {
				return err
			}
		}
		return nil
	}
	if payload.HandOver {
		if holds, err := r.handOverHolds(ctx, row, payload); err != nil || !holds {
			return err
		}
		if issue.Status == payload.Status && issue.HandedOver {
			return r.consumeHandover(ctx, row, payload)
		}
		// Every hand-over write is conditional on the sequence just read (dispatch HandOver), the
		// label added when the child lacks it, so a person's move since that read stands.
		if err := r.dispatch.HandOver(ctx, issue, payload.Status); err != nil {
			return fmt.Errorf("hand Dispatch issue %s to Legion as %s: %w", row.Issue, payload.Status, err)
		}
		return r.consumeHandover(ctx, row, payload)
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

// consumeHandover applies the hand-over to the record once Dispatch shows it: the row's hand-over
// write just set it, or Dispatch already showed the todo and label (which a write would make no
// event for). It does not wait for the write's event: a restart between the write and that event's
// publication lets the boot read record the event's sequence, and the live event then stops at the
// sequence fence. The fact is idempotent (admit's consumeHandover reads the fence it deletes), so
// the event, or this row's retry after a failure here, changes nothing more.
func (r *outbox) consumeHandover(ctx context.Context, row record.OutboxRow, payload record.StatusWrite) error {
	_, err := intake.ApplyFact(ctx, r.pool, "outbox", fmt.Sprintf("handover:%s:%d", row.Issue, row.ID),
		intake.Handover{Issue: row.Issue, Tree: payload.HandOverTree, Generation: payload.HandOverLinger}, r.handlers...)
	return err
}

// handOverHolds is an early check only; the durable fence is enforced again by intake when the
// PATCH's daemon event arrives. The one transaction confirms this row still owns the child and the
// linger it names, avoiding a needless write once a root reopen already removed that fence.
func (r *outbox) handOverHolds(ctx context.Context, row record.OutboxRow, payload record.StatusWrite) (bool, error) {
	var issue, root *record.Issue
	var handover *record.Handover
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var err error
		issue, err = r.records.Issue(ctx, tx, row.Issue)
		if err != nil || issue == nil {
			return err
		}
		handover, err = r.records.Handover(ctx, tx, row.Issue)
		if err != nil || handover == nil {
			return err
		}
		if handover.Cancelled {
			return r.records.DeleteHandoverAt(ctx, tx, row.Issue, payload.HandOverTree, payload.HandOverLinger)
		}
		root, err = r.records.Issue(ctx, tx, handover.Tree)
		return err
	}); err != nil {
		return false, fmt.Errorf("read hand-over of %s: %w", row.Issue, err)
	}
	if issue != nil && handover != nil && !handover.Cancelled && root != nil && issue.Tree == handover.Tree && handover.Tree == payload.HandOverTree &&
		handover.Generation == payload.HandOverLinger && root.LingersAt(handover.Generation) {
		return true, nil
	}
	r.log.Info("outbox hand-over no longer holds; finished without writing", "row", row.ID, "issue", row.Issue,
		"tree", payload.HandOverTree, "linger", payload.HandOverLinger)
	return false, nil
}

// dropHandover deletes the fence of a hand-over a person's move superseded, but only this row's
// own: the tree and linger generation the row names. A newer linger's fence on the same child is
// its own row's to settle.
func (r *outbox) dropHandover(ctx context.Context, issue string, payload record.StatusWrite) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return r.records.DeleteHandoverAt(ctx, tx, issue, payload.HandOverTree, payload.HandOverLinger)
	})
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
// topic, and to nothing else. Every notice kind is for an architect, and no claim subscribes to an
// issue topic (packages/pi-legion/src/claim-session.ts), so no issue topic carries one. The
// owner, the earlier notices it waits behind, and whether its tree lingers are read from one
// snapshot of the tree (notice_routing.go). A notice waits
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
		if catchUpSuperseded(*payload.CatchUp, r.supervisedClaim(root).Claim, &tree.root) {
			r.log.Info("outbox catch-up finished without publishing: a newer catch-up supersedes it", "row", row.ID, "issue", row.Issue,
				"generation", payload.CatchUp.Generation, "launch", payload.CatchUp.Launch)
			return nil
		}
	}
	architect, err := owningArchitect(tree.project, tree.issues, issue, payload.Kind, r.holdsItsRole)
	if err != nil {
		return fmt.Errorf("the architect of %s: %w", row.Issue, err)
	}
	if held := r.supervisedClaim(architect); stoppedWithTree(tree.root.Lingers(), held) {
		r.log.Info("outbox notice finished undelivered: its architect stopped with its finished tree",
			"row", row.ID, "kind", payload.Kind, "issue", row.Issue, "architect", architect, "state", held.State, "stopping", held.Stopping)
		return nil
	}
	if earlier := earlierNoticeFor(tree, architect, r.holdsItsRole); earlier != 0 {
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

// mergeQueue publishes the merger's READY packet, or the daemon's withdrawal of it, to the
// project's merge queue role, keyed by the row so a retried row is one delivery. A role with no
// live holder refuses every attempt until someone claims it, so waiting would retry forever; the
// Dispatch message the same READY posted already carries the packet, and the issue's status shows
// a withdrawal, so the issue is told the role had no holder, as the merger's prompt says the
// daemon does (internal/prompts/go/merger.md), and the row is done.
func (r *outbox) mergeQueue(ctx context.Context, row record.OutboxRow, payload record.MergeQueuePublish) error {
	if r.notices == nil {
		return errors.New("merge queue executor has no Envoy publisher")
	}
	err := r.notices.Publish(ctx, notify.RoleTopicPrefix+payload.Role, payload.Packet, payload.Packet, record.OutboxKey(row.ID))
	if errors.Is(err, notify.ErrNoHolder) {
		return r.message(ctx, row, record.MessagePost{Body: fmt.Sprintf("merge queue role %s had no live holder at %s", payload.Role, r.now().UTC().Format(time.RFC3339))})
	}
	if err != nil {
		return fmt.Errorf("publish %s's READY packet or its withdrawal to merge queue role %s: %w", row.Issue, payload.Role, err)
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
	// A close row naming a tree other than its issue's never reserves it; the checks below decide it.
	if payload.Op == "tree_close" && payload.Tree == issue.Tree {
		if handled, err := r.reservedTreeClose(ctx, row, payload); handled || err != nil {
			return err
		}
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
		// A child carried into a tree of its own — a finished root's hand-over, or a person's label
		// on a child its tree left mid-run — may still run a role of its old tree: one in a turn when
		// the old tree's close held its suspension, which that close's row, now of an earlier
		// generation, finishes without acting on. No role of the new tree starts while one runs.
		if err := r.stopElsewhere(ctx, issue); err != nil {
			return err
		}
		// A child of a closed tree re-admitted as a root of its own keeps its roles' claims, which
		// still name the tree it left; each is re-pointed before anything else here, so it binds this
		// tree's lifecycle and launches in this tree's resources, and so does whatever resumes it
		// while this start waits (an operator's resume of a claim the operator suspended). The issue
		// the supervisor keeps beside the machine (volumeLost) never moves with it: a claim's issue is
		// fixed for its life.
		if found && machine.Claim().Tree != issue.Tree {
			if err := machine.Retree(ctx, issue.Tree); err != nil {
				return err
			}
		}
		// An operator's suspension holds its claim until the operator resumes
		// it: the start, and the task it gives, wait on the row's backoff, before a quiesce
		// interrupts the role whose phase it would take over. StartedBy refuses the same in the
		// claim's own decision.
		if found {
			if held := machine.Claim(); held.RuleHolds() {
				return fmt.Errorf("start %s of %s: %w", payload.Role, issue.Key, held.RuleHoldError())
			}
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
			// The role owns the issue's phase this start is for: the rule resumes it, unless an
			// operator suspended it meanwhile, when the task's delivery below waits instead.
			if err := machine.Handle(ctx, supervise.RequestRuleResume{Claim: token,
				Reason: fmt.Sprintf("start row %d of %s (phase %q)", row.ID, issue.Key, payload.Phase)}); err != nil {
				return fmt.Errorf("resume claim %s: %w", token, err)
			}
		case supervise.StateFailed:
			// A boot rule-resume that spent its launch budget is terminal for this row: retrying it
			// with RequestRetry would reset that budget and launch forever. The failure's workflow
			// fact and boot report name it. Other workflow starts retain their existing retry rule.
			if payload.RuleResume {
				r.log.Info("outbox boot rule-resume finished: its claim spent the launch budget", "row", row.ID, "issue", issue.Key, "role", payload.Role)
				return nil
			}
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
			return r.stopWithoutMachine(ctx, row, token)
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
		if err := machine.Handle(ctx, supervise.RequestSuspend{Claim: token, Reason: payload.Reason, By: supervise.SuspendedByDaemon}); err != nil {
			return fmt.Errorf("suspend claim %s: %w", token, err)
		}
		return nil
	case "issue_close":
		// A child's close as done: the claim is closed as a tree close closes it, its session dropped
		// with it, since the issue's volume the session lived on goes with the close's release
		// (record.IssueSuspend's Release, which waits for every claim of the issue to retire). It is
		// fenced as a suspend is, not as a tree close: no linger is involved, and a newer start
		// means a person set the child todo again, whose run the close must not end.
		if !found {
			return r.stopWithoutMachine(ctx, row, token)
		}
		if last := machine.Claim().LastStartRow; !workflow.StopActs(row.ID, payload, last, nil) {
			r.log.Info("outbox issue close superseded by a newer start; finished without acting",
				"row", row.ID, "issue", issue.Key, "role", payload.Role, "start-row", last)
			return nil
		}
		if err := machine.Handle(ctx, supervise.RequestIssueClose{Claim: token}); err != nil {
			return fmt.Errorf("close the issue of claim %s: %w", token, err)
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
		// A close that reserved, or found reserved, its tree's cleanup ran through reservedTreeClose
		// already; one reaching here closes its claim alone.
		if err := machine.Handle(ctx, supervise.RequestTreeClose{Claim: token}); err != nil {
			return fmt.Errorf("close the tree of claim %s: %w", token, err)
		}
		return nil
	default:
		return fmt.Errorf("outbox row %d has unknown supervise operation %q", row.ID, payload.Op)
	}
}

// stopElsewhere suspends, as the daemon and keeping its session, every claim of issue that still
// runs a process in a tree other than the issue's, and is a wait (wait.ErrWaiting) until none
// does. A claim in a turn holds the suspension for the turn's end and a launching one until its
// agent is ready (supervise's suspend rows), so the start that asks is what waits, retried on its
// backoff, charging no claim a launch. No close of the old tree reaches such a claim, since its
// issue has left that tree, so the suspension names the deadline that close would have had
// (carriedStopDeadline): a turn still running at the old root's linger expiry is stopped there, and
// the claim keeps its workspace, the issue's branch and its session, which its next start resumes
// with its phase's resume task. The hold lives in the claim's memory, so a restart that forgets it
// is asked again by the start's next attempt, with the same deadline. Once none runs, every claim
// of the issue may be re-pointed to the issue's tree (supervise.Machine.Retree), and the issue's
// Sandbox, which no role of the old tree then runs in, is the new tree's to relabel
// (runtime/sandbox's ensureSandbox). A claim already in the issue's tree is skipped on its view,
// which only Retree changes, so a start waits on no decision a sibling claim has in flight.
func (r *outbox) stopElsewhere(ctx context.Context, issue record.Issue) error {
	var running []string
	for _, role := range claim.Roles {
		token, err := claim.NewToken(r.project, issue.Key, role)
		if err != nil {
			return fmt.Errorf("derive the %s claim of %s: %w", role, issue.Key, err)
		}
		machine, found := r.supervisor.Machine(token)
		if !found || machine.View().Claim.Tree == issue.Tree {
			continue
		}
		held := machine.Claim()
		if held.Tree == issue.Tree || held.RunsNothing() {
			continue
		}
		var oldRoot *record.Issue
		if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
			var err error
			oldRoot, err = r.records.Issue(ctx, tx, held.Tree)
			return err
		}); err != nil {
			return fmt.Errorf("read the root of tree %s, which claim %s still runs in: %w", held.Tree, token, err)
		}
		err = machine.Handle(ctx, supervise.RequestSuspend{Claim: token, By: supervise.SuspendedByDaemon,
			Deadline: carriedStopDeadline(oldRoot, r.now(), r.supervisor.deps.Timeouts.Stop),
			Reason:   fmt.Sprintf("%s moved from tree %s to tree %s", issue.Key, held.Tree, issue.Tree)})
		var refused *supervise.RefusedError
		if err != nil && !errors.Is(err, supervise.ErrSuspendHeld) && !errors.As(err, &refused) {
			return fmt.Errorf("suspend claim %s, still running in tree %s: %w", token, held.Tree, err)
		}
		if held = machine.Claim(); !held.RunsNothing() {
			running = append(running, fmt.Sprintf("%s:%s in tree %s", token, held.State, held.Tree))
		}
	}
	if len(running) > 0 {
		return wait.Errorf("start of %s in tree %s waits for %s to stop", issue.Key, issue.Tree, strings.Join(running, ", "))
	}
	return nil
}

// carriedStopDeadline is when a carried child's role still running in its old tree, under oldRoot,
// is stopped if its turn has not ended: at the old root's linger expiry, where that tree's close
// would have ended it, so a turn the linger leaves room for ends on its own. A root that no longer
// lingers (or is not recorded) has no close left to wait for, so the stop is bounded as the
// operator's is, at stop after now.
func carriedStopDeadline(oldRoot *record.Issue, now time.Time, stop time.Duration) time.Time {
	if oldRoot != nil && oldRoot.LingerUntil != nil {
		return *oldRoot.LingerUntil
	}
	return now.Add(stop)
}

// reservedTreeClose runs a workflow tree close through its tree's durable cleanup reservation, and
// does so before any stale-close fence. A close reserves only while the root still lingers at the
// close's generation. Once it has, a re-admission that ends that linger cannot open the tree's next
// epoch until the cleanup confirms, so every close row of the tree, however stale its own or its
// root's generation has become, still retires its claim and drives the cleanup; were the fences to
// finish those rows without acting, the reservation would never confirm and the re-admitted tree
// would wait forever. The cleanup (store.CleanupReservedTree) waits until every stored claim of the
// tree has retired, then has the runtime release what it holds for the tree and confirms the
// reservation. It reports false, acting on nothing, when the tree holds no unconfirmed
// reservation and this close could not take one; the ordinary fences then decide.
func (r *outbox) reservedTreeClose(ctx context.Context, row record.OutboxRow, payload record.SuperviseRequest) (bool, error) {
	lifecycle, reserved, err := r.trees.ReserveWorkflowTreeCleanup(ctx, r.project, payload.Tree, payload.Linger)
	if err != nil {
		return true, fmt.Errorf("reserve cleanup of workflow tree %s: %w", payload.Tree, err)
	}
	if !reserved {
		return false, nil
	}
	token, err := claim.NewToken(r.project, row.Issue, payload.Role)
	if err != nil {
		return true, fmt.Errorf("derive claim for outbox row %d: %w", row.ID, err)
	}
	if machine, found := r.supervisor.Machine(token); found {
		if err := machine.Handle(ctx, supervise.RequestTreeClose{Claim: token}); err != nil {
			return true, fmt.Errorf("close the tree of claim %s: %w", token, err)
		}
	}
	return true, r.trees.CleanupReservedTree(ctx, r.project, payload.Tree, lifecycle.Epoch, r.supervisor.deps.Runtime)
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

// stopWithoutMachine is a stop row — a suspend or an issue close — that finds no machine for its
// claim. Creation persists a claim before publishing its machine in memory, and the stop must not
// be consumed in that interval: while the stored claim may still hold a process, the row waits for
// the machine the later publish brings; a claim that runs nothing, or none at all, leaves it
// nothing to stop.
func (r *outbox) stopWithoutMachine(ctx context.Context, row record.OutboxRow, token claim.Token) error {
	pending, err := r.trees.StoredClaimMayRun(ctx, token, row.ID)
	if err != nil {
		return err
	}
	if pending {
		return wait.Errorf("stop claim %s: waiting for its stored claim's supervising machine", token)
	}
	return nil
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

// provisionWorkspace provisions issue's workspace in the repository its record names, at the
// default branch the record names, as the implement App for that repository's owner.
func (r *outbox) provisionWorkspace(ctx context.Context, issue record.Issue) error {
	if r.podsProvision() {
		return nil
	}
	if r.tokens == nil {
		return errors.New("supervise executor has no GitHub App token manager")
	}
	repo, err := issue.Repository()
	if err != nil {
		return fmt.Errorf("provision workspace for %s: %w", issue.Key, err)
	}
	lease, err := r.tokens.Token(ctx, appauth.Implement, repo.Owner())
	if err != nil {
		return fmt.Errorf("mint the implement App token for %s to provision %s: %w", repo.Owner(), issue.Key, err)
	}
	// One provision at a time (provisionMu): the issue's workspace is added to its repository's
	// shared clone, which every issue on that repository shares, and a second issue's lane
	// provisioning beside this one would have the clone fetching and adding under both.
	r.provisionMu.Lock()
	defer r.provisionMu.Unlock()
	if _, err := r.provision(ctx, workspace.Request{
		StateDir: r.stateDir, Repo: repo, Issue: issue.Key, DefaultBranch: issue.DefaultBranch, CredentialHelper: workspace.GitHubCredentialHelper,
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

// issueBranch creates the issue's branch, legion/<KEY>, on GitHub at the repository's default
// branch, so no role's push is the one that creates it: GitHub can refuse such a push on a large
// repository (its check for changed workflow files, which it runs for an App without the workflows
// permission, times out on a new ref), where it takes one that moves the branch. A branch GitHub
// already has is left where it is. A start of the issue waits for this row (the lifecycle lane's
// ordering rule, record.Store.DueRows and ClaimRow), so a create GitHub refuses holds back the
// issue's roles, each attempt logged with GitHub's answer, until one passes. A row of a generation
// the issue has left, or of a tree that lingers, finishes without acting: the starts it held back
// act on neither.
func (r *outbox) issueBranch(ctx context.Context, row record.OutboxRow, payload record.IssueBranch) error {
	issue, err := r.issue(ctx, row.Issue)
	if err != nil {
		return err
	}
	if payload.Generation != issue.Generation {
		r.log.Info("outbox issue branch serves an earlier generation; finished without acting", "row", row.ID, "issue", issue.Key,
			"generation", payload.Generation, "current", issue.Generation)
		return nil
	}
	root, err := r.root(ctx, issue)
	if err != nil {
		return err
	}
	if root != nil && root.Lingers() {
		r.log.Info("outbox issue branch of a member of a lingering tree; finished without acting", "row", row.ID, "issue", issue.Key, "tree", issue.Tree)
		return nil
	}
	if err := r.createBranch(ctx, issue); err != nil {
		return fmt.Errorf("create the branch of %s, which its roles' starts wait for: %w", issue.Key, err)
	}
	return nil
}

// createBranch creates issue's branch on GitHub, in the repository its record names, at the
// default branch the record names, unless GitHub already has it, as the implement App for that
// repository's owner: the App of the roles that write an issue branch (appauth.AppRoleFor), and
// the one workspace provisioning clones with.
func (r *outbox) createBranch(ctx context.Context, issue record.Issue) error {
	ctx, cancel := context.WithTimeout(ctx, issueBranchTimeout)
	defer cancel()
	repo, err := issue.Repository()
	if err != nil {
		return err
	}
	lease, err := r.tokens.Token(ctx, appauth.Implement, repo.Owner())
	if err != nil {
		return fmt.Errorf("mint the implement App token for %s: %w", repo.Owner(), err)
	}
	github := githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(r.githubAPI, repo)}
	return ghbranch.Create(ctx, github, repo, workspace.Bookmark(issue.Key), issue.DefaultBranch)
}

func (r *outbox) removeWorkspace(ctx context.Context, row record.OutboxRow, payload record.WorkspaceRemove) error {
	if r.supervisor == nil {
		return errors.New("workspace removal has no claim supervisor")
	}
	if r.podsProvision() {
		return nil
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
			return wait.Errorf("workspace removal of %s waits for the tree's close to retire claim %s (%s)", row.Issue, token, machine.Claim().State)
		}
	}
	repo, err := issue.Repository()
	if err != nil {
		return fmt.Errorf("locate workspace for %s: %w", row.Issue, err)
	}
	working, err := workspace.Location(r.stateDir, repo, row.Issue)
	if err != nil {
		return fmt.Errorf("locate workspace for %s: %w", row.Issue, err)
	}
	// The removal forgets the workspace in the same shared clone a provision adds to, so it takes
	// the provision's turn (provisionMu) rather than running under another issue's provision.
	r.provisionMu.Lock()
	defer r.provisionMu.Unlock()
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
