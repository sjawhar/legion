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
// was cancelled, timed out or needs action, or the head reports no result for it. A check still
// running is not red yet.
func (s Standing) Red() bool {
	return s.Result != Success && s.Result != Pending
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

// HeadChecks is each check the pull request's base branch requires (Required), judged by the
// settlement that stands for its head (settled), its own or one carried back from a head a
// .legion/-only push replaced, then each workflow it requires (Workflows), judged by its latest run
// on the head that settlement is of, as the daemon last read it. A check the settlement names as
// failed failed, and one it reports otherwise passed. One it names as cancelled, or does not
// report at all, is Pending: a settlement can come before a required check is decided, since the
// listener settles a commit once every check it has seen is terminal and the commit is quiet,
// which an aggregator job with needs: is not yet queued for, and which a run that concurrency
// cancels has reached before its replacement shows; the next settlement of the head decides it. It
// may never report, and a head with no verdict never reaches READY, so an approved round whose own
// head settles this way is told stuck, naming the check (workflow's reviewRound). A workflow whose
// run succeeded passed and one whose run ended any other way failed with its conclusion; one still
// running, one the head has no run of (WorkflowsWithoutRun), and one not read at that head yet are
// Pending, and the daemon's next read decides it. ok is false when there is nothing to judge: no
// settlement stands for the head, or the required set was never read.
func HeadChecks(pr record.PullRequest) (checks []Standing, ok bool) {
	if !settled(pr) || pr.Required == nil {
		return nil, false
	}
	results := make(map[string]string, len(pr.CheckRuns)+len(pr.Failing)+len(pr.Cancelled))
	for _, run := range pr.CheckRuns {
		results[run.Name] = Success
	}
	// A cancelled check waits for the head's next settlement, as a missing one does below.
	for _, name := range pr.Cancelled {
		results[name] = Pending
	}
	for _, name := range pr.Failing {
		results[name] = Failed
	}
	checks = Judge(pr.Required, results)
	for i, check := range checks {
		if check.Result == Missing {
			checks[i].Result = Pending
		}
	}
	for _, workflow := range pr.Workflows {
		result := Pending
		if pr.WorkflowsHead == pr.CheckedHead && workflow.Result != Missing {
			result = workflow.Result
		}
		checks = append(checks, Standing{Name: workflow.Path, Result: result})
	}
	return checks, true
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

// WorkflowsWithoutRun names each workflow the base branch requires that the daemon's read found no
// run of on the head the settlement standing for the pull request's head is of. Unlike one still
// running, or not read at that head yet, such a workflow may never run there: a workflow GitHub
// does not start for the head, or one another repository defines (requiredchecks.Workflows).
func WorkflowsWithoutRun(pr record.PullRequest) []string {
	if !settled(pr) || pr.WorkflowsHead != pr.CheckedHead {
		return nil
	}
	var paths []string
	for _, workflow := range pr.Workflows {
		if workflow.Result == Missing {
			paths = append(paths, workflow.Path)
		}
	}
	return paths
}

// HeadVerdict is the CI verdict that stands for the pull request's current head: "red" when a check
// or workflow its base branch requires failed there (HeadChecks), "green" when every one passed,
// and none when HeadChecks has nothing to judge or a required check or workflow is pending. A check
// the base branch does not require never makes a head red, whatever it settled, and a base that
// requires nothing has nothing red, as READY then has nothing to refuse.
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
		case check.Result == Pending:
			verdict = ""
		}
	}
	return verdict
}
