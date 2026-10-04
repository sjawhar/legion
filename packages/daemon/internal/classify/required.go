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
// .legion/-only push replaced. A check the settlement names as failed failed, and one it reports
// otherwise passed. One it names as cancelled, or does not report at all, is Pending: a settlement
// can come before a required check is decided, since the listener settles a commit once every check
// it has seen is terminal and the commit is quiet, which an aggregator job with needs: is not yet
// queued for, and which a run that concurrency cancels has reached before its replacement shows;
// the next settlement of the head decides it. It may never report, and a head with no verdict never
// reaches READY, so an approved round whose own head settles this way is told stuck, naming the
// check (workflow's reviewRound). ok is false when there is nothing to judge: no settlement stands
// for the head, or the required set was never read.
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
	return checks, true
}

// HeadVerdict is the CI verdict that stands for the pull request's current head: "red" when a check
// its base branch requires failed there (HeadChecks), "green" when every one passed, and none when
// HeadChecks has nothing to judge or a required check is pending. A check the base branch does not
// require never makes a head red, whatever it settled, and a base that requires no check has
// nothing red, as READY then has nothing to refuse.
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
