package classify

import "github.com/sjawhar/legion/daemon/internal/record"

// Success, Pending and Missing are the results Judge gives a required check that has not failed:
// it passed (a check run that ended neutral or skipped passes, as GitHub counts it for a required
// check), it is still running, or the head reports no result for it at all.
const (
	Success = "success"
	Pending = "pending"
	Missing = "missing"
)

// Standing is one required check's result on a head: Success, Pending, Missing, or the conclusion
// or state it failed with.
type Standing struct {
	Name   string
	Result string
}

// Red is whether the check keeps GitHub from merging the head until something changes: it failed,
// was cancelled, timed out or needs action. A check still running is not red yet, and neither is
// one the head reports no result for (Missing): READY refuses that on its own, and it leaves the
// workflow's verdict undecided (HeadVerdict), since a later settlement or run can still bring it.
func (s Standing) Red() bool {
	return s.Result != Success && s.Result != Pending && s.Result != Missing
}

// RunResult is what Judge reads for one check run or workflow run: Pending until it completes,
// Success when it ended success, neutral or skipped, as GitHub counts it for a required check, and
// otherwise the conclusion it failed with.
func RunResult(status, conclusion string) string {
	switch {
	case status != "completed":
		return Pending
	case conclusion == "success" || conclusion == "neutral" || conclusion == "skipped":
		return Success
	default:
		return conclusion
	}
}

// Judge is Legion's one rule for whether CI is red at a head: each name in required - the checks
// the base branch requires (requiredchecks.Required) - in order, with its result in results (a
// check's name to Success, Pending, or what it failed with), and a required check results does not
// name Missing. A check results names that required does not is no part of the answer. The
// merger's READY judges the results it reads from GitHub by it, and the workflow the settlement
// that stands for the head (HeadChecks).
func Judge(required []string, results map[string]string) []Standing {
	standings := make([]Standing, 0, len(required))
	for _, name := range required {
		result, reported := results[name]
		if !reported {
			result = Missing
		}
		standings = append(standings, Standing{Name: name, Result: result})
	}
	return standings
}

// Failed is the result HeadChecks gives a check the settlement names as failed
// (intake.PullRequestChecks).
const Failed = "failure"

// Cancelled is the result RunResult gives a run GitHub cancelled: its conclusion, passed through.
const Cancelled = "cancelled"

// HeadChecks is each check the pull request's base branch requires (Required), judged by the
// settlement that stands for its head (settled), its own or one carried back from a head a
// .legion/-only push replaced, then each workflow it requires (Workflows), judged by its latest run
// on the head that settlement is of, as the daemon last read it. A check the settlement names as
// failed failed, one it reports otherwise passed, one it names as cancelled is Pending, and one it
// does not report at all is Missing: a settlement can come before a required check is decided,
// since the listener settles a commit once every check it has seen is terminal and the commit is
// quiet, which an aggregator job with needs: is not yet queued for, and which a run that
// concurrency cancels has reached before its replacement shows; the next settlement of the head
// decides it. It may never report, and a head with no verdict never reaches READY, so an approved
// round whose own head settles this way is told stuck, naming the check (workflow's reviewRound).
// A workflow's result stands only when the daemon read it at the settlement's head (WorkflowHead),
// and is Pending otherwise, until the daemon's next read: a run that succeeded passed, one still
// running is Pending, one the head has no run of is Missing, and one that ended any other way
// failed with its conclusion - except a run cancelled at a head a .legion/-only push replaced,
// which is Pending: that push's own run cancelled it (a workflow that cancels a run in flight on a
// push), and the head's own settlement and run decide it. ok is false when there is nothing to
// judge: no settlement stands for the head, or the required set was never read.
func HeadChecks(pr record.PullRequest) (checks []Standing, ok bool) {
	checks, workflows, ok := headStandings(pr)
	return append(checks, workflows...), ok
}

// headStandings is HeadChecks in its two halves: each required check's standing, judged by the
// settlement, and each required workflow's, judged by its latest run.
func headStandings(pr record.PullRequest) (checks, workflows []Standing, ok bool) {
	if !settled(pr) || pr.Required == nil {
		return nil, nil, false
	}
	results := make(map[string]string, len(pr.CheckRuns)+len(pr.Failing)+len(pr.Cancelled))
	for _, run := range pr.CheckRuns {
		results[run.Name] = Success
	}
	// A cancelled check waits for the head's next settlement.
	for _, name := range pr.Cancelled {
		results[name] = Pending
	}
	for _, name := range pr.Failing {
		results[name] = Failed
	}
	workflows = make([]Standing, 0, len(pr.Workflows))
	for _, workflow := range pr.Workflows {
		workflows = append(workflows, WorkflowStanding(pr, workflow))
	}
	return Judge(pr.Required, results), workflows, true
}

// WorkflowStanding is how one required workflow of the pull request stands at its head
// (HeadChecks): its latest run's result as the daemon read it, except Pending when the runs were
// read at another head than the settlement's, or when the run was cancelled at a head a
// .legion/-only push replaced. It is the one judgment of a required workflow, so whatever names a
// workflow red names exactly the ones HeadVerdict counts.
func WorkflowStanding(pr record.PullRequest, workflow record.RequiredWorkflow) Standing {
	result := workflow.Result
	if pr.WorkflowsHead != pr.CheckedHead || (result == Cancelled && pr.WorkflowsHead != pr.HeadSHA) {
		result = Pending
	}
	return Standing{Name: workflow.Path, Result: result}
}

// WorkflowHead is the head whose workflow runs stand for the pull request's head: the head the
// settlement standing for it is of (settled), which a .legion/-only push that started no CI of its
// own can have left, else the head itself, whose settlement is still to come. The daemon reads the
// required workflows' runs there (intake.RequiredChecks), and HeadChecks judges by them only while
// that is still the settlement's head.
func WorkflowHead(pr record.PullRequest) string {
	if settled(pr) {
		return pr.CheckedHead
	}
	return pr.HeadSHA
}

// HeadVerdict is the CI verdict that stands for the pull request's current head: "red" when a check
// or workflow its base branch requires failed there (HeadChecks), "green" when every one passed,
// and none when HeadChecks has nothing to judge or a required check or workflow is pending or has
// no result. A check the base branch does not require never makes a head red, whatever it settled,
// and a base that requires nothing has nothing red, as READY then has nothing to refuse.
func HeadVerdict(pr record.PullRequest) string {
	checks, ok := HeadChecks(pr)
	if !ok {
		return ""
	}
	verdict := "green"
	for _, check := range checks {
		switch {
		case check.Red():
			return "red"
		case check.Result != Success:
			verdict = ""
		}
	}
	return verdict
}

// RedOnlyByWorkflows says whether CI is red at the pull request's head only because a workflow its
// base branch requires failed there: no required check is red (each passed, is pending or has no
// result), and at least one required workflow's run is red (HeadChecks). Its result is as old as
// the daemon's last read of the runs, so in awaiting_merge only such a read withdraws a READY on it
// (workflow's decideChecks).
func RedOnlyByWorkflows(pr record.PullRequest) bool {
	return redOnlyByWorkflows(pr, func(string) bool { return true })
}

// RedOnlyByReviewWorkflows says whether CI is red at the pull request's head only because review
// workflows failed there: no required check is red (each passed, is pending or has no result), at
// least one required workflow's run is red, and every red one is a workflow the project declares as
// a review workflow (reviewWorkflows, its `review_workflows` paths). Such a workflow fails on its
// own findings, which the reviewer adjudicates, so in testing and reviewing its red is the
// reviewer's round's to decide (RedSendsBack). A red required workflow the project does not so
// declare is a failing check like any other. With no review workflow declared, it is never true.
func RedOnlyByReviewWorkflows(pr record.PullRequest, reviewWorkflows []string) bool {
	return redOnlyByWorkflows(pr, func(path string) bool {
		for _, review := range reviewWorkflows {
			if review == path {
				return true
			}
		}
		return false
	})
}

// redOnlyByWorkflows says whether no required check is red at the pull request's head, at least
// one required workflow is, and counts holds for each red workflow's path.
func redOnlyByWorkflows(pr record.PullRequest, counts func(path string) bool) bool {
	checks, workflows, ok := headStandings(pr)
	if !ok {
		return false
	}
	for _, check := range checks {
		if check.Red() {
			return false
		}
	}
	red := false
	for _, workflow := range workflows {
		if workflow.Red() {
			if !counts(workflow.Name) {
				return false
			}
			red = true
		}
	}
	return red
}
