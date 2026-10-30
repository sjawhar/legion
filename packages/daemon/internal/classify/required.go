package classify

// Success and Pending are the results RunResult gives a run that has not failed: it passed (a run
// that ended neutral or skipped passes, as GitHub counts it for a required check), or it is still
// running.
const (
	Success = "success"
	Pending = "pending"
)

// Cancelled is the result RunResult gives a run GitHub cancelled: its conclusion, passed through.
const Cancelled = "cancelled"

// RunResult is what one check run or workflow run came to: Pending until it completes, Success
// when it ended success, neutral or skipped, as GitHub counts it for a required check, and
// otherwise the conclusion it failed with. Nothing in Legion judges a head by it: a CI result
// moves no issue (dispatch://INFRA-714/spec R1), and the one reader left is the merge queue's
// report of the runs that failed on a removed group commit (ghautomerge.failingRuns).
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
