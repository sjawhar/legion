package workflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// CheckLookBound bounds every look of a production check: a check_look row due this long after
// the look's wake ends a look nobody reported (checkLookEnded). A first look publishes to the
// deploy role and reports pending in minutes; a woken look that drives the change in production
// is one agent turn, and the longest production checks measured ran within an hour. Without a
// bound a silent checking agent keeps its pod for the whole of the check's bound, the cost the check
// after the close exists to remove.
const CheckLookBound = 2 * time.Hour

// The causes a look is woken by: what the check's record names as its last wake, and what the
// check task tells the checking agent woke it.
const (
	wakeMerge = "merge"
	wakeBound = "bound"
)

// checkRole is the role that works the production checks this project opens (the project's
// `production_check` setting, Config.ProductionCheckRole), and the empty role for a project that
// opens none. One function, so no two sites can disagree on who the check is for. A check already
// open keeps the role it opened with, which its own record holds (record.ProductionCheck.Role):
// nothing re-reads the setting for it.
func (e *Engine) checkRole() claim.Role { return e.cfg.ProductionCheckRole }

// openCheck opens issue's production check on its merged pull request pr and starts its first
// look, woken by the merge: the record (record.ProductionCheck, bound at the project's
// ProductionCheckBound from now, carrying the role that works it), the check_bound row due at
// that bound, then the look (startLook). A project whose `production_check` is off has no role to
// work a check (checkRole), and opens nothing at all: no record, no bound row and no look, only
// the line saying the close had nothing to check.
// wake is only how the open is logged: a lingering tree's member has its claims suspended,
// so the look's start row is a wake of the checking role's claim, where a running tree's is a task
// handed to that role's live session; the rows are the same, since the start resumes either. One check
// per issue: a second merge of the same issue — a rerun's, after a re-entry closed the first —
// replaces the earlier record, whose bound row the new check then defers to its own bound
// (checkBound); the same merge seen again — a done child a person parks and sets done again, each
// move a leave — opens nothing, since the check it has, open or recorded, is that merge's. The
// check's OpenedAt is its identity (record.ProductionCheck.ID), so it is the clock truncated to the
// microsecond, Postgres's timestamptz precision: the value the rows and facts of this transaction
// carry equals the one every later read of the record returns.
func (e *Engine) openCheck(ctx context.Context, tx pgx.Tx, issue record.Issue, pr record.PullRequest, wake bool) error {
	role := e.checkRole()
	if role == "" {
		e.logOnCommit(ctx, "workflow: no production check: this project checks nothing in production", "issue", issue.Key,
			"mergeCommit", pr.MergeSHA, "pullRequest", pr.Number)
		return nil
	}
	existing, err := e.store.ProductionCheck(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	if existing != nil && e.sameRepository(existing.Repo, pr.Repo) && existing.Number == pr.Number && (existing.MergeSHA == "" || pr.MergeSHA == "" || existing.MergeSHA == pr.MergeSHA) {
		state := "open"
		if !existing.Open() {
			state = "closed as " + string(existing.Outcome)
		}
		e.logOnCommit(ctx, "workflow: production check not opened again: the issue's check is this merge's", "issue", issue.Key,
			"mergeCommit", pr.MergeSHA, "pullRequest", pr.Number, "check", existing.OpenedAt, "state", state)
		return nil
	}
	now := e.now().Truncate(time.Microsecond)
	check := record.ProductionCheck{
		Issue: issue.Key, Project: issue.Project, Tree: issue.Tree, Repo: pr.Repo, Number: pr.Number, MergeSHA: pr.MergeSHA,
		OpenedAt: now, BoundAt: now.Add(e.cfg.ProductionCheckBound), Role: role,
	}
	if err := e.enqueueAt(ctx, tx, issue.Key, record.CheckBound{}, check.BoundAt); err != nil {
		return err
	}
	first := "live"
	if wake {
		first = "wake"
	}
	e.logOnCommit(ctx, "workflow: production check opened", "issue", issue.Key, "mergeCommit", pr.MergeSHA, "pullRequest", pr.Number,
		"role", role, "bound", check.BoundAt, "firstLook", first)
	return e.startLook(ctx, tx, issue, &check, wakeMerge, pr.MergeSHA, false)
}

// startLook starts the next look of issue's check, woken by cause with detail, last marking the
// bound's look: the record counts the look open from now with that wake and no wake standing —
// the row this look starts from is spent, and only a pending report names the next one — the
// check's role (record.ProductionCheck.Role, the role the check opened with, never the setting as
// it stands now) is started on the check task — a start with no phase, fenced on the check and its
// look (record.SuperviseRequest.CheckLook and CheckOpenedAt) rather than on the generation, and
// stamped with the generation the issue holds as the delivery's run — and a check_look row due at
// the look bound, naming the check and the look, ends the look should nobody report it.
func (e *Engine) startLook(ctx context.Context, tx pgx.Tx, issue record.Issue, check *record.ProductionCheck, cause, detail string, last bool) error {
	now := e.now()
	check.Looks++
	check.LookOpen, check.LookWakeAt = true, &now
	check.LastWakeKind, check.LastWakeDetail, check.LastWakeAt = cause, detail, &now
	check.WakeAt, check.Last = nil, last
	if err := e.store.PutProductionCheck(ctx, tx, *check); err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: check.Role,
		Generation: issue.Generation, CheckLook: check.Looks, CheckOpenedAt: check.OpenedAt, Task: checkTask(issue, *check, cause, detail, last)}); err != nil {
		return err
	}
	if err := e.enqueueAt(ctx, tx, issue.Key, record.CheckLook{Look: check.Looks, OpenedAt: check.OpenedAt}, now.Add(CheckLookBound)); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: production check look started", "issue", issue.Key, "mergeCommit", check.MergeSHA, "look", check.Looks,
		"role", check.Role, "cause", cause, "detail", detail, "last", last)
	return nil
}

// checkTaskLead opens every check task: a resumed session tells the check from its last phase
// task by this line (the phase tasks open with "Continue"), and a test pins it.
const checkTaskLead = "Production check of "

// checkTask is what the implementer is told at a look of its closed issue's production check. The
// session it reaches may be fresh — a woken pod after a lost volume — so the text stands on its
// own: the issue, the repository, the pull request, the merge commit, the look and what woke it,
// whether it is the last, and the rules of the check in prose: how to reach the deploy, the three
// verdicts and what each asks of the pull request, no commit, and no waiting in the session for a
// deploy, which the daemon wakes the check on.
func checkTask(issue record.Issue, check record.ProductionCheck, cause, detail string, last bool) string {
	merge := check.MergeSHA
	if merge == "" {
		merge = "not yet recorded"
	}
	var text strings.Builder
	fmt.Fprintf(&text, "%s%s: %s.\n", checkTaskLead, issue.Key, issue.Title)
	fmt.Fprintf(&text, "Repository %s, pull request #%d, merge commit %s. Look %d, woken by %s.", check.Repo, check.Number, merge, check.Looks, wokenBy(cause, detail))
	if last {
		text.WriteString(" This is the last look: no look follows.")
	}
	text.WriteString("\nDo what the deployment instructions say to reach the deploy (publish the merge commit to the deploy role; await no reply), " +
		"read whether production carries the merge, then report with the legion tool's handoff_complete and a verdict, committing nothing: " +
		"pass once you drove the changed path in production and recorded the issue's after_merge check with dispatch result at the merge commit, its observed naming what woke this look, and put the line that command prints in the pull request body; " +
		"pending when production does not carry the merge yet, the summary naming what is missing, with no result recorded and the pull request body saying the check waits on the deploy of the merge commit; " +
		"fail for a defect, never an issue you file (skill://legion-worker/references/findings.md): a defect with a clear fix goes to the controller topic your Legion addressing line names, with its evidence posted as a comment on the controller's report issue; a defect that needs a decision is a dispatch ask on that report issue, whose reference you then publish to the controller topic. Either names this issue, the pull request, the merge commit, what you drove and what failed; record the after_merge check as a fail with dispatch result at the merge commit, and name the message or the ask in its observed, in the line you put in the pull request body and in the summary. " +
		"Wait for no deploy in this session: the daemon wakes the check on the deploy's event.")
	if last {
		text.WriteString(" On this last look a pending closes the check as not run: before reporting it, write on the pull request that the check did not run because production did not carry the merge within the bound.")
	}
	return text.String()
}

// wokenBy is the check task's wording of a look's wake, from the cause and its detail.
func wokenBy(cause, detail string) string {
	switch cause {
	case wakeMerge:
		return "the merge"
	case wakeBound:
		return "the bound: " + detail
	default:
		return cause + " " + detail
	}
}

// boundText is the check's bound as the bound's look is told it: whole days or hours as such, any
// other duration as Go prints it.
func boundText(bound time.Duration) string {
	switch {
	case bound > 0 && bound%(24*time.Hour) == 0:
		return fmt.Sprintf("%d days", bound/(24*time.Hour))
	case bound > 0 && bound%time.Hour == 0:
		return fmt.Sprintf("%d hours", bound/time.Hour)
	default:
		return bound.String()
	}
}

// checkReported records the checking agent's report on a look (intake.CheckReport). A report on an
// issue with no open check is refused (CHECK_NOT_OPEN), and so is one naming a look that is not
// the open one (CHECK_LOOK_NOT_OPEN: its bound or a hold ended it, or the report is of an earlier
// look) or carrying no verdict of pass, pending or fail (CHECK_VERDICT_REQUIRED). The look ends;
// pass closes the check as recorded and fail as a defect (closeCheck), and pending ends the look
// as one with nothing to show (endLookPending).
func (e *Engine) checkReported(ctx context.Context, tx pgx.Tx, fact intake.CheckReport) (intake.Result, error) {
	check, err := e.store.ProductionCheck(ctx, tx, fact.Issue)
	if err != nil {
		return intake.Result{}, err
	}
	if check == nil || !check.Open() {
		return refused("CHECK_NOT_OPEN", fmt.Sprintf("%s has no open production check; this report changed nothing", fact.Issue)), nil
	}
	if !check.LookOpen || fact.Look != check.Looks {
		return refused("CHECK_LOOK_NOT_OPEN", fmt.Sprintf("look %d of %s's production check is not open: the check is at look %d, and its bound or a hold ends a look nobody reported; this report changed nothing",
			fact.Look, fact.Issue, check.Looks)), nil
	}
	var outcome record.CheckOutcome
	switch fact.Verdict {
	case "pass":
		outcome = record.CheckRecorded
	case "fail":
		outcome = record.CheckDefect
	case "pending":
	default:
		return refused("CHECK_VERDICT_REQUIRED", fmt.Sprintf("a production check report names a verdict of pass, pending or fail, not %q; this report changed nothing", fact.Verdict)), nil
	}
	check.LookOpen = false
	if err := e.store.PutProductionCheck(ctx, tx, *check); err != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, "workflow: production check look reported", "issue", check.Issue, "mergeCommit", check.MergeSHA, "look", check.Looks,
		"cause", check.LastWakeKind, "verdict", fact.Verdict)
	if outcome == "" {
		return intake.Result{}, e.endLookPending(ctx, tx, check)
	}
	return intake.Result{}, e.closeCheck(ctx, tx, check, outcome)
}

// endLook ends the open look of a check nobody reported — its bound came due, the outbox met an
// operator's hold on its start, or the checking role's claim failed in it — as a report of pending
// would (endLookPending). cause is why, for the log.
func (e *Engine) endLook(ctx context.Context, tx pgx.Tx, check *record.ProductionCheck, cause string) error {
	check.LookOpen = false
	if err := e.store.PutProductionCheck(ctx, tx, *check); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: production check look ended without a report", "issue", check.Issue, "mergeCommit", check.MergeSHA,
		"look", check.Looks, "cause", cause)
	return e.endLookPending(ctx, tx, check)
}

// endLookPending is what follows a look that saw the merge nowhere in production. The last look
// closes the check as not run. An issue back in the workflow — a root reopened inside its linger,
// whose implementer now works the new run — starts no look and suspends nobody: the check waits,
// and closes with the run's next merge (openCheck) or at its bound (checkBound). Nothing here
// starts the next look from a cause that arrived during the look: the only writer of such a cause
// was Engine.wake, which read production from a build or a deploy event, and that is gone.
// The claim of the check's role is then suspended, its session kept, and the issue's Sandbox is
// deleted with the pod it ran in (record.IssueSuspend): both rows follow the look's start row, so
// the suspension's fences find no later start and wait for the suspend (store.IssueSuspension),
// and the pod goes until the next wake, whose launch makes the Sandbox again.
func (e *Engine) endLookPending(ctx context.Context, tx pgx.Tx, check *record.ProductionCheck) error {
	if check.Last {
		return e.closeCheck(ctx, tx, check, record.CheckNotRun)
	}
	issue, root, err := e.checkIssue(ctx, tx, *check)
	if err != nil {
		return err
	}
	if InWorkflow(issue) {
		e.logOnCommit(ctx, "workflow: production check waits while its issue runs again", "issue", issue.Key, "mergeCommit", check.MergeSHA,
			"look", check.Looks, "phase", issue.Phase)
		return nil
	}
	if err := e.suspend(ctx, tx, issue, check.Role, fmt.Sprintf("%s's production check waits for its deploy", issue.Key)); err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.IssueSuspend{Tree: issue.Tree, Generation: issue.Generation, TreeGeneration: root.Generation}); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: production check waits", "issue", issue.Key, "mergeCommit", check.MergeSHA, "look", check.Looks)
	return nil
}

// closeCheck closes the check with outcome (markCheckClosed) and ends the checking role's part in it:
// its claim is closed (issue_close: retired, its session dropped), and the issue's Sandbox is
// suspended and deleted with the pod it ran in, whether the issue is a child whose tree runs on or
// a root, or a child, of a lingering tree whose close would delete it anyway. A tree lingering past
// its deadline waited for this check (lingerExpired): once no check of the tree is open its close
// is re-armed with a LingerClose row due now, a duplicate of the waiting row's event, so applying
// both is harmless. An issue back in the workflow — a root reopened inside its linger — retires
// nobody and suspends nothing: the claims and the pod are the new run's.
func (e *Engine) closeCheck(ctx context.Context, tx pgx.Tx, check *record.ProductionCheck, outcome record.CheckOutcome) error {
	if err := e.markCheckClosed(ctx, tx, check, outcome); err != nil {
		return err
	}
	issue, root, err := e.checkIssue(ctx, tx, *check)
	if err != nil {
		return err
	}
	if InWorkflow(issue) {
		e.logOnCommit(ctx, "workflow: production check closed while its issue runs again; its claims and pod are the run's", "issue", issue.Key,
			"mergeCommit", check.MergeSHA, "phase", issue.Phase)
		return nil
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "issue_close", Tree: issue.Tree, Role: check.Role,
		Generation: issue.Generation, Reason: fmt.Sprintf("%s's production check closed as %s", issue.Key, outcome)}); err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.IssueSuspend{Tree: issue.Tree, Generation: issue.Generation, TreeGeneration: root.Generation}); err != nil {
		return err
	}
	if root.LingerUntil == nil || e.now().Before(*root.LingerUntil) {
		return nil
	}
	open, err := e.store.OpenProductionChecksOfTree(ctx, tx, root.Tree)
	if err != nil || len(open) > 0 {
		return err
	}
	e.logOnCommit(ctx, "workflow: the last open production check of a tree past its linger closed; the tree's close is re-armed", "tree", root.Key,
		"generation", root.Generation)
	return e.enqueue(ctx, tx, root.Key, record.LingerClose{Generation: root.Generation})
}

// markCheckClosed writes the check closed now with outcome, its look closed with it, and logs the
// close with the issue and the merge commit.
func (e *Engine) markCheckClosed(ctx context.Context, tx pgx.Tx, check *record.ProductionCheck, outcome record.CheckOutcome) error {
	now := e.now()
	check.ClosedAt, check.Outcome, check.LookOpen = &now, outcome, false
	if err := e.store.PutProductionCheck(ctx, tx, *check); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: production check closed", "issue", check.Issue, "mergeCommit", check.MergeSHA, "outcome", outcome, "looks", check.Looks)
	return nil
}

// checkIssue is the check's issue and its tree root as recorded.
func (e *Engine) checkIssue(ctx context.Context, tx pgx.Tx, check record.ProductionCheck) (record.Issue, record.Issue, error) {
	issue, err := e.store.Issue(ctx, tx, check.Issue)
	if err != nil {
		return record.Issue{}, record.Issue{}, err
	}
	if issue == nil {
		return record.Issue{}, record.Issue{}, fmt.Errorf("production check of %s: its issue is not recorded", check.Issue)
	}
	root, err := e.store.Issue(ctx, tx, check.Tree)
	if err != nil {
		return record.Issue{}, record.Issue{}, err
	}
	if root == nil {
		return record.Issue{}, record.Issue{}, fmt.Errorf("production check of %s: its tree root %s is not recorded", check.Issue, check.Tree)
	}
	return *issue, *root, nil
}

// checkLookEnded is a look ending with no report (intake.CheckLookEnded: the look's bound, an
// operator's hold met by its start, the checking role's failed claim): the look it names, still
// open, ends as a pending report would (endLook); a look already ended, or a check closed, changes
// nothing, and so does a look of a check a later merge has replaced, one the fact does not name
// (record.ProductionCheck.NamedBy), whose look 1 the current check's would otherwise be taken
// for, logged.
func (e *Engine) checkLookEnded(ctx context.Context, tx pgx.Tx, fact intake.CheckLookEnded) (intake.Result, error) {
	check, err := e.store.ProductionCheck(ctx, tx, fact.Issue)
	if err != nil || check == nil || !check.Open() || !check.LookOpen || check.Looks != fact.Look {
		return intake.Result{}, err
	}
	if !check.NamedBy(fact.OpenedAt) {
		e.logOnCommit(ctx, "workflow: production check look end ignored: it names a check a later merge replaced", "issue", check.Issue, "mergeCommit", check.MergeSHA,
			"check", check.OpenedAt, "named", fact.OpenedAt, "look", fact.Look, "cause", fact.Cause)
		return intake.Result{}, nil
	}
	return intake.Result{}, e.endLook(ctx, tx, check, fact.Cause)
}

// checkBound is the check's bound coming due (intake.CheckBound, the check_bound row at its
// bound_at). A closed check, or one whose last look has started, changes nothing. A row that comes
// due before the check's bound is an earlier check's of the same issue, replaced by a later merge
// (openCheck), and is deferred to this check's bound. A look open at the bound defers it by the
// look bound: the look's own bound ends the look first. An issue back in the workflow — a root
// reopened inside its linger — closes the check as not run, since no look can run while its
// implementer works the new run, and the run's own merge opens its own. Otherwise the last look
// starts, told that it is the last and that production did not carry the merge within the bound.
func (e *Engine) checkBound(ctx context.Context, tx pgx.Tx, fact intake.CheckBound) (intake.Result, error) {
	check, err := e.store.ProductionCheck(ctx, tx, fact.Issue)
	if err != nil || check == nil || !check.Open() || check.Last {
		return intake.Result{}, err
	}
	now := e.now()
	if now.Before(check.BoundAt) {
		e.logOnCommit(ctx, "workflow: a production check bound of an earlier check came due; deferred to the check's own", "issue", check.Issue, "bound", check.BoundAt)
		return intake.Result{}, e.enqueueAt(ctx, tx, check.Issue, record.CheckBound{}, check.BoundAt)
	}
	if check.LookOpen {
		e.logOnCommit(ctx, "workflow: production check bound deferred while a look is open", "issue", check.Issue, "mergeCommit", check.MergeSHA, "look", check.Looks)
		return intake.Result{}, e.enqueueAt(ctx, tx, check.Issue, record.CheckBound{}, now.Add(CheckLookBound))
	}
	issue, _, err := e.checkIssue(ctx, tx, *check)
	if err != nil {
		return intake.Result{}, err
	}
	if InWorkflow(issue) {
		e.logOnCommit(ctx, "workflow: production check bound came due while its issue runs again", "issue", issue.Key, "mergeCommit", check.MergeSHA, "phase", issue.Phase)
		return intake.Result{}, e.closeCheck(ctx, tx, check, record.CheckNotRun)
	}
	return intake.Result{}, e.startLook(ctx, tx, issue, check, wakeBound,
		fmt.Sprintf("production did not carry the merge within %s", boundText(e.cfg.ProductionCheckBound)), true)
}
