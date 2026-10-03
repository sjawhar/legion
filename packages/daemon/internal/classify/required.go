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

// failed is the result HeadChecks gives a check the settlement names as failing: one that failed
// or was cancelled (intake.PullRequestChecks).
const failed = "failed"

// HeadChecks is each check the pull request's base branch requires (Required), judged by the
// settlement that stands for its head (settled): a check the settlement names as failing failed,
// one it reports otherwise passed, and one it does not report at all is missing, since the listener
// settles a commit only once every check suite it started has completed. ok is false when there is
// nothing to judge: no settlement stands for the head, or the required set was never read.
func HeadChecks(pr record.PullRequest) (checks []Standing, ok bool) {
	if !settled(pr) || pr.Required == nil {
		return nil, false
	}
	results := make(map[string]string, len(pr.CheckRuns)+len(pr.Failing))
	for _, run := range pr.CheckRuns {
		results[run.Name] = Success
	}
	for _, name := range pr.Failing {
		results[name] = failed
	}
	return Judge(pr.Required, results), true
}

// HeadVerdict is the CI verdict that stands for the pull request's current head: "red" when a check
// its base branch requires is red there (HeadChecks), "green" when none is, and none when HeadChecks
// has nothing to judge. A check the base branch does not require never makes a head red, whatever
// it settled, and a base that requires no check has nothing red, as READY then has nothing to
// refuse.
func HeadVerdict(pr record.PullRequest) string {
	checks, ok := HeadChecks(pr)
	if !ok {
		return ""
	}
	for _, check := range checks {
		if check.Red() {
			return "red"
		}
	}
	return "green"
}
