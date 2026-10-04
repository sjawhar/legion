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

// Failed and Cancelled are the results HeadChecks gives a check the settlement names as failed
// and as cancelled (intake.PullRequestChecks).
const (
	Failed    = "failure"
	Cancelled = "cancelled"
)

// HeadChecks is each check the pull request's base branch requires (Required), judged by the
// settlement that stands for its head (settled) under the rule READY refuses by (Judge): a check
// the settlement names as failed failed, one it names as cancelled was cancelled, one it reports
// otherwise passed, and one it does not report at all is missing, since the listener settles a
// commit only once every check suite it started has completed. ok is false when there is nothing
// to judge: no settlement stands for the head, or the required set was never read.
//
// A settlement carried back from a head a .legion/-only push replaced (CheckedHead is not the
// head) predicts a failure soundly, since the code is the same, but not a cancellation or a gap: a
// push cancels the run before it where a required workflow cancels in progress, and the reviewer's
// own approval push is a full-CI push. So a required check the carried settlement names cancelled,
// or does not report, is Pending: the head's own settlement decides it.
func HeadChecks(pr record.PullRequest) (checks []Standing, ok bool) {
	if !settled(pr) || pr.Required == nil {
		return nil, false
	}
	results := make(map[string]string, len(pr.CheckRuns)+len(pr.Failing)+len(pr.Cancelled))
	for _, run := range pr.CheckRuns {
		results[run.Name] = Success
	}
	for _, name := range pr.Cancelled {
		results[name] = Cancelled
	}
	for _, name := range pr.Failing {
		results[name] = Failed
	}
	checks = Judge(pr.Required, results)
	if pr.CheckedHead != pr.HeadSHA {
		for i, check := range checks {
			if check.Result == Cancelled || check.Result == Missing {
				checks[i].Result = Pending
			}
		}
	}
	return checks, true
}

// HeadVerdict is the CI verdict that stands for the pull request's current head: "red" when a check
// its base branch requires is red there (HeadChecks), "green" when every one passed, and none when
// HeadChecks has nothing to judge or a required check is still pending. A check the base branch
// does not require never makes a head red, whatever it settled, and a base that requires no check
// has nothing red, as READY then has nothing to refuse.
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
