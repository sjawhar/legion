package workflow

import (
	"context"
	"fmt"
	"regexp"
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
// bound a silent implementer keeps its pod for the whole of the check's bound, the cost the check
// after the close exists to remove.
const CheckLookBound = 2 * time.Hour

// The causes a look is woken by: what the check's record names as its last wake, and what the
// check task tells the implementer woke it.
const (
	wakeMerge         = "merge"
	wakeDeployed      = "issue.deployed"
	wakeDaemonBoot    = "daemon boot"
	wakeDispatchStart = "dispatch start"
	wakeDispatchBuild = "dispatch build"
	wakeBound         = "bound"
)

// buildWake says whether a wake cause is a build compare's: the three sources of one build wake
// alike, so a build that woke a check from one wakes it from no other.
func buildWake(cause string) bool {
	return cause == wakeDaemonBoot || cause == wakeDispatchStart || cause == wakeDispatchBuild
}

// openCheck opens issue's production check on its merged pull request pr and starts its first
// look, woken by the merge: the record (record.ProductionCheck, bound at the project's
// ProductionCheckBound from now), the check_bound row due at that bound, then the look (startLook).
// wake is only how the open is logged: a lingering tree's member has its implementer suspended,
// so the look's start row is a wake of that claim, where a running tree's is a task handed to the
// implementer's live session; the rows are the same, since the start resumes either. One check
// per issue: a second merge of the same issue — a rerun's, after a re-entry closed the first —
// replaces the earlier record, whose bound row the new check then defers to its own bound
// (checkBound); the same merge seen again — a done child a person parks and sets done again, each
// move a leave — opens nothing, since the check it has, open or recorded, is that merge's. The
// check's OpenedAt is its identity (record.ProductionCheck.ID), so it is the clock truncated to the
// microsecond, Postgres's timestamptz precision: the value the rows and facts of this transaction
// carry equals the one every later read of the record returns.
func (e *Engine) openCheck(ctx context.Context, tx pgx.Tx, issue record.Issue, pr record.PullRequest, wake bool) error {
	existing, err := e.store.ProductionCheck(ctx, tx, issue.Key)
	if err != nil {
		return err
	}
	if existing != nil && existing.Repo == pr.Repo && existing.Number == pr.Number && (existing.MergeSHA == "" || pr.MergeSHA == "" || existing.MergeSHA == pr.MergeSHA) {
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
		OpenedAt: now, BoundAt: now.Add(e.cfg.ProductionCheckBound),
	}
	if err := e.enqueueAt(ctx, tx, issue.Key, record.CheckBound{}, check.BoundAt); err != nil {
		return err
	}
	first := "live"
	if wake {
		first = "wake"
	}
	e.logOnCommit(ctx, "workflow: production check opened", "issue", issue.Key, "mergeCommit", pr.MergeSHA, "pullRequest", pr.Number,
		"bound", check.BoundAt, "firstLook", first)
	return e.startLook(ctx, tx, issue, &check, wakeMerge, pr.MergeSHA, false)
}

// startLook starts the next look of issue's check, woken by cause with detail, last marking the
// bound's look: the record counts the look open from now with that wake and no cause pending, the
// implementer is started on the check task — a start with no phase, fenced on the check and its
// look (record.SuperviseRequest.CheckLook and CheckOpenedAt) rather than on the generation, and
// stamped with the generation the issue holds as the delivery's run — and a check_look row due at
// the look bound, naming the check and the look, ends the look should nobody report it.
func (e *Engine) startLook(ctx context.Context, tx pgx.Tx, issue record.Issue, check *record.ProductionCheck, cause, detail string, last bool) error {
	now := e.now()
	check.Looks++
	check.LookOpen, check.LookWakeAt = true, &now
	check.LastWakeKind, check.LastWakeDetail, check.LastWakeAt = cause, detail, &now
	check.PendingCauseKind, check.PendingCauseDetail = "", ""
	check.Last = last
	if err := e.store.PutProductionCheck(ctx, tx, *check); err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "start", Tree: issue.Tree, Role: claim.RoleImplementer,
		Generation: issue.Generation, CheckLook: check.Looks, CheckOpenedAt: check.OpenedAt, Task: checkTask(issue, *check, cause, detail, last)}); err != nil {
		return err
	}
	if err := e.enqueueAt(ctx, tx, issue.Key, record.CheckLook{Look: check.Looks, OpenedAt: check.OpenedAt}, now.Add(CheckLookBound)); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: production check look started", "issue", issue.Key, "mergeCommit", check.MergeSHA, "look", check.Looks,
		"cause", cause, "detail", detail, "last", last)
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
		"pass once you drove the changed path in production and wrote the pull request's Production: line naming the merge commit and what woke this look; " +
		"pending when production does not carry the merge yet, the summary naming what is missing and the Production: line left at `pending deploy of <merge commit>`; " +
		"fail for a defect, a dispatch ask and never an issue you file (skill://legion-worker/references/findings.md), on the controller's report issue and naming this issue, the pull request, the merge commit, what you drove and what failed; then publish the ask's reference to the controller topic your Legion addressing line names, and name the ask in the Production: line and the summary. " +
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
	case wakeDeployed:
		return "Dispatch's issue.deployed: " + detail
	case wakeDaemonBoot:
		return "the Legion daemon's boot on build " + detail
	case wakeDispatchStart:
		return "Dispatch's start on build " + detail
	case wakeDispatchBuild:
		return "Dispatch's build " + detail
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

// checkReported records the implementer's report on a look (intake.CheckReport). A report on an
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
// operator's hold on its start, or the implementer's claim failed in it — as a report of pending
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
// whose implementer now works the new run — starts no look and suspends nobody: the check waits
// with whatever cause it holds, and closes with the run's next merge (openCheck) or at its bound
// (checkBound). A cause that arrived during the look starts the next look with it at once.
// Otherwise the implementer's claim is suspended, its session kept, and the issue's Sandbox is
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
	if check.PendingCauseKind != "" {
		return e.startLook(ctx, tx, issue, check, check.PendingCauseKind, check.PendingCauseDetail, false)
	}
	if err := e.suspend(ctx, tx, issue, claim.RoleImplementer, fmt.Sprintf("%s's production check waits for its deploy", issue.Key)); err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.IssueSuspend{Tree: issue.Tree, Generation: issue.Generation, TreeGeneration: root.Generation}); err != nil {
		return err
	}
	e.logOnCommit(ctx, "workflow: production check waits", "issue", issue.Key, "mergeCommit", check.MergeSHA, "look", check.Looks)
	return nil
}

// closeCheck closes the check with outcome (markCheckClosed) and ends the implementer's part in it:
// its claim is closed (issue_close: retired, its session dropped), and the issue's Sandbox is
// suspended and deleted with the pod it ran in, whether the issue is a child whose tree runs on or
// a root, or a child, of a lingering tree whose close would delete it anyway. A tree lingering past
// its deadline waited for this check (lingerExpired): once no check of the tree is open its close
// is re-armed with a LingerClose row due now, a duplicate of the waiting row's event, so applying
// both is harmless. An issue back in the workflow — a root reopened inside its linger — retires
// nobody and suspends nothing: its implementer and its pod are the new run's.
func (e *Engine) closeCheck(ctx context.Context, tx pgx.Tx, check *record.ProductionCheck, outcome record.CheckOutcome) error {
	if err := e.markCheckClosed(ctx, tx, check, outcome); err != nil {
		return err
	}
	issue, root, err := e.checkIssue(ctx, tx, *check)
	if err != nil {
		return err
	}
	if InWorkflow(issue) {
		e.logOnCommit(ctx, "workflow: production check closed while its issue runs again; its implementer and pod are the run's", "issue", issue.Key,
			"mergeCommit", check.MergeSHA, "phase", issue.Phase)
		return nil
	}
	if err := e.enqueue(ctx, tx, issue.Key, record.SuperviseRequest{Op: "issue_close", Tree: issue.Tree, Role: claim.RoleImplementer,
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

// issueDeployed is Dispatch's issue.deployed for a pull request (intake.IssueDeployed): it wakes
// the open check of that pull request (wake), and changes nothing for an issue with no open check
// or a check of another pull request.
func (e *Engine) issueDeployed(ctx context.Context, tx pgx.Tx, fact intake.IssueDeployed) (intake.Result, error) {
	check, err := e.store.ProductionCheck(ctx, tx, fact.Issue)
	if err != nil || check == nil || !check.Open() || check.Repo != fact.Repo || check.Number != fact.Number {
		return intake.Result{}, err
	}
	return intake.Result{}, e.wake(ctx, tx, check, wakeDeployed, fmt.Sprintf("run %d completed %s", fact.RunID, fact.CompletedAt.UTC().Format(time.RFC3339)))
}

// wake is the one wake rule of an open check, for every cause but the merge and the bound. A check
// whose last look has started is woken no more. issue.deployed wakes a check once: a second, after
// one woke the latest look or is kept for the next, changes nothing. A build wakes a check once
// whichever of its three sources reports it: a build that woke the latest look, or is kept for the
// next, changes nothing again. An issue back in the workflow (a root reopened inside its linger)
// keeps the cause for its next look and starts none, and so does a check whose look is open, where
// the look proceeds and a pending report starts the next look with the cause kept (endLookPending);
// a cause already kept is kept over a later one. Otherwise the cause starts the next look.
func (e *Engine) wake(ctx context.Context, tx pgx.Tx, check *record.ProductionCheck, kind, detail string) error {
	ignored := func(why string) error {
		e.logOnCommit(ctx, "workflow: production check wake ignored", "issue", check.Issue, "mergeCommit", check.MergeSHA, "kind", kind, "detail", detail, "why", why)
		return nil
	}
	switch {
	case check.Last:
		return ignored("the last look has started")
	case kind == wakeDeployed && (check.LastWakeKind == wakeDeployed || check.PendingCauseKind == wakeDeployed):
		return ignored("issue.deployed woke it once already")
	case buildWake(kind) && (buildWake(check.LastWakeKind) && check.LastWakeDetail == detail || buildWake(check.PendingCauseKind) && check.PendingCauseDetail == detail):
		return ignored("this build woke it already")
	}
	issue, err := e.store.Issue(ctx, tx, check.Issue)
	if err != nil {
		return err
	}
	if issue == nil {
		return fmt.Errorf("production check of %s: its issue is not recorded", check.Issue)
	}
	runsAgain := InWorkflow(*issue)
	if runsAgain || check.LookOpen {
		why := "a look is open"
		if runsAgain {
			why = "the issue runs again"
		}
		if check.PendingCauseKind != "" {
			return ignored(why + ", and " + check.PendingCauseKind + " is kept for the next look already")
		}
		check.PendingCauseKind, check.PendingCauseDetail = kind, detail
		if err := e.store.PutProductionCheck(ctx, tx, *check); err != nil {
			return err
		}
		e.logOnCommit(ctx, "workflow: production check wake kept for the next look", "issue", check.Issue, "mergeCommit", check.MergeSHA,
			"kind", kind, "detail", detail, "why", why)
		return nil
	}
	e.logOnCommit(ctx, "workflow: production check woken", "issue", check.Issue, "mergeCommit", check.MergeSHA, "kind", kind, "detail", detail)
	return e.startLook(ctx, tx, *issue, check, kind, detail, false)
}

// checkWake is a build compare that found the check's merge in a build (intake.CheckWake, the
// check_compare row's finding): the wake rule applies (wake); a check no longer open changes
// nothing, and so does a wake of a check a later merge has replaced, one the fact does not name
// (record.ProductionCheck.NamedBy), logged.
func (e *Engine) checkWake(ctx context.Context, tx pgx.Tx, fact intake.CheckWake) (intake.Result, error) {
	check, err := e.store.ProductionCheck(ctx, tx, fact.Issue)
	if err != nil || check == nil || !check.Open() {
		return intake.Result{}, err
	}
	if !check.NamedBy(fact.OpenedAt) {
		e.logOnCommit(ctx, "workflow: production check wake ignored: it names a check a later merge replaced", "issue", check.Issue, "mergeCommit", check.MergeSHA,
			"check", check.OpenedAt, "named", fact.OpenedAt, "kind", fact.Cause, "detail", fact.Detail)
		return intake.Result{}, nil
	}
	return intake.Result{}, e.wake(ctx, tx, check, fact.Cause, fact.Detail)
}

// serverStarted is Dispatch's start on a build (intake.ServerStarted): the build is compared
// against the open checks of the repository the daemon is built from (compareBuilds), as the
// daemon's own build is at boot.
func (e *Engine) serverStarted(ctx context.Context, tx pgx.Tx, fact intake.ServerStarted) (intake.Result, error) {
	return intake.Result{}, e.compareBuilds(ctx, tx, wakeDispatchStart, fact.Commit)
}

// buildShape is what a build's commit must look like to be compared: a git SHA, abbreviated or
// whole. The daemon's own revision and Dispatch's /healthz commit are release stamps of that
// shape; Dispatch's server.started is published on the bus by any client the listener takes a
// `source: dispatch` publish from, so its commit is held to the shape here, where every build
// meets the compare path (the implement App's GitHub compare URL) and the wake detail written
// into the implementer's task text, and nothing else reaches either.
var buildShape = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// compareBuilds queues one check_compare row, carrying cause, the build's commit and the check
// it is of, for every open check of this project in the repository the daemon is built from
// (Config.BuiltFrom): the row's executor reads GitHub for whether the build carries the check's
// merge and applies intake.CheckWake when it does. A check whose last look has started gets none,
// since no look follows; no build, or no repository the daemon is built from, compares nothing,
// and a build that is not a git SHA (buildShape) is refused, logged with its cause and length
// and never its text.
func (e *Engine) compareBuilds(ctx context.Context, tx pgx.Tx, cause, build string) error {
	if build == "" || e.cfg.BuiltFrom == "" {
		return nil
	}
	if !buildShape.MatchString(build) {
		e.logOnCommit(ctx, "workflow: a build is not compared against the open production checks: its commit is not a git SHA", "cause", cause, "length", len(build), "repo", e.cfg.BuiltFrom)
		return nil
	}
	open, err := e.store.OpenProductionChecks(ctx, tx)
	if err != nil {
		return err
	}
	compared := 0
	for _, check := range open {
		if check.Project != e.cfg.Project || check.Repo != e.cfg.BuiltFrom || check.Last {
			continue
		}
		if err := e.enqueue(ctx, tx, check.Issue, record.CheckCompare{Look: check.Looks, Cause: cause, Build: build, OpenedAt: check.OpenedAt}); err != nil {
			return err
		}
		compared++
	}
	if compared > 0 {
		e.logOnCommit(ctx, "workflow: a build is compared against the open production checks", "cause", cause, "build", build, "repo", e.cfg.BuiltFrom, "checks", compared)
	}
	return nil
}

// checkLookEnded is a look ending with no report (intake.CheckLookEnded: the look's bound, an
// operator's hold met by its start, the implementer's failed claim): the look it names, still
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
