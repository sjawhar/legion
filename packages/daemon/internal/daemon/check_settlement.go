package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// readSettlement is the required-checks pass's read of the check runs on the head of pr, an open
// pull request no CI settlement stands for (classify.Settled), at the head its settlement would be
// of (classify.WorkflowHead). Envoy's listener settles a head from its check_run webhooks, and a
// delivery it lost, a run GitHub sent with no pull request named (which the listener never
// records), or a record it marked overflowed leaves the head with no settlement and the issue with
// no CI verdict. The read is one request per hundred runs, and only while no settlement stands.
// When GitHub lists at least one run, every run is completed and the newest completed at least
// requiredChecksInterval ago - the listener, had it seen them, would have settled by then - it
// applies the settlement the listener would have published (listenerSettlement), at
// classify.PassGeneration, which the workflow decides as it decides the listener's
// (workflow's checks). The listener's own settlement of the same head arriving afterwards is merged
// into it check by check and changes nothing the pass learned (classify.ClassifySettlement). Its
// event id names the head and the run-id set, so the same runs are applied once, however many
// passes read them.
//
// ctx bounds the apply, and read the read before it. It says whether the pass must stop: ctx
// ended, or GitHub answered with its rate limit, which every later read of the pass on the same
// installation meets. Any other failure is logged and applies nothing, and the next pass reads
// again.
func (w *workflowRuntime) readSettlement(ctx, read context.Context, github githubrest.Client, pr record.PullRequest) bool {
	if classify.Settled(pr) {
		return false
	}
	head := classify.WorkflowHead(pr)
	runs, err := github.CheckRuns(read, head)
	if err != nil {
		if ctx.Err() != nil {
			return true
		}
		return w.logReadFailure(err, "read the check runs on a pull request's head: GitHub's rate limit; this pass reads no more pull requests",
			"read the check runs on a pull request's head; it stays with no settlement until a later pass reads them or Envoy's listener settles it",
			"repo", pr.Repo, "pullRequest", pr.Number, "head", head)
	}
	fact, completed, ok := listenerSettlement(pr.Repo, pr.Number, head, runs, w.reviewAppID)
	if !ok || time.Since(completed) < requiredChecksInterval {
		return false
	}
	eventID := w.passEventID("checks-read", pr.Repo, pr.Number, head, fact.Snapshot)
	if !w.unapplied(ctx, eventID) {
		return false
	}
	w.log.Warn("workflow: GitHub shows every check run on a pull request's head completed and no settlement recorded for it; applying the settlement Envoy's listener would",
		"repo", pr.Repo, "pullRequest", pr.Number, "issue", pr.Issue, "head", head, "runs", len(fact.CheckRuns), "failing", fact.Failing, "cancelled", fact.Cancelled)
	if _, err := intake.ApplyFact(ctx, w.pool, "github", eventID, fact, w.handlers...); err != nil {
		w.log.Warn("apply a head's settlement read from GitHub", "repo", pr.Repo, "pullRequest", pr.Number, "head", head, "error", err)
	}
	return false
}

// reviewerVerdicts are the check names a Legion role's verdict is published under. Envoy's listener
// records a run of one only when the review App created it (packages/envoy/internal/webhook's
// reviewerVerdict and its reviewer App id), so a run of the same name from any other App passes
// for no verdict.
var reviewerVerdicts = map[string]bool{"tester": true, "architect": true}

// maxCheckNameRunes is how much of a check's name Envoy's listener keys its CI record by
// (packages/envoy/internal/contracts' maxEnvelopeTextRunes).
const maxCheckNameRunes = 2048

// listenerCheckName is a check's name as Envoy's listener keys its CI record and names it in a
// settlement (packages/envoy/internal/contracts' checkName): whole, or past maxCheckNameRunes its
// first maxCheckNameRunes runes, an ellipsis, a space and the first eight bytes, in hex, of the
// SHA-256 of the whole name.
func listenerCheckName(name string) string {
	if utf8.RuneCountInString(name) <= maxCheckNameRunes {
		return name
	}
	digest := sha256.Sum256([]byte(name))
	return string([]rune(name)[:maxCheckNameRunes]) + "… " + hex.EncodeToString(digest[:8])
}

// listenerOutcome is how Envoy's listener groups a completed run by its conclusion
// (packages/envoy/internal/cistore's classify): "failed" for failure, timed_out, action_required,
// startup_failure, stale and every conclusion it does not know, "cancelled" for cancelled, and ""
// for one that passed - success, neutral, skipped, or none.
func listenerOutcome(conclusion string) string {
	switch conclusion {
	case "cancelled":
		return "cancelled"
	case "success", "neutral", "skipped", "":
		return ""
	default:
		return "failed"
	}
}

// listenerSettlement is the settlement Envoy's listener would publish for head of pull request
// number of repo from runs, GitHub's check runs on it (packages/envoy/internal/cistore's
// renderSummary over the record its webhook handler keeps): a run of a reviewer verdict's name
// (reviewerVerdicts) is left out unless the review App, reviewAppID, created it; each check, named
// as the listener names it (listenerCheckName), is its run with the highest id; Failing and
// Cancelled name the checks whose run failed or was cancelled (listenerOutcome), sorted, as
// CheckRuns is by name. Its generation is classify.PassGeneration, and its snapshot a digest of the
// run-id set, which names the same runs alike on every read. completed is when the newest run it
// counts completed, a check's earlier run included: any run's completion is activity on the head.
// ok is false when no run is left, or a check's latest run has not completed: the listener settles
// only a head whose every check is done.
func listenerSettlement(repo string, number int, head string, runs []githubrest.CheckRun, reviewAppID string) (fact intake.PullRequestChecks, completed time.Time, ok bool) {
	latest := map[string]githubrest.CheckRun{}
	for _, run := range runs {
		name := listenerCheckName(run.Name)
		if run.ID <= 0 || name == "" || (reviewerVerdicts[name] && strconv.FormatInt(run.App.ID, 10) != reviewAppID) {
			continue
		}
		if run.CompletedAt != nil && run.CompletedAt.After(completed) {
			completed = *run.CompletedAt
		}
		if known, found := latest[name]; !found || run.ID > known.ID {
			latest[name] = run
		}
	}
	if len(latest) == 0 {
		return intake.PullRequestChecks{}, time.Time{}, false
	}
	fact = intake.PullRequestChecks{Repo: repo, Number: number, HeadSHA: head, CheckRuns: make([]record.AttemptRun, 0, len(latest)),
		Generation: classify.PassGeneration, Failing: []string{}, Cancelled: []string{}}
	for name, run := range latest {
		if run.Status != "completed" || run.CompletedAt == nil {
			return intake.PullRequestChecks{}, time.Time{}, false
		}
		fact.CheckRuns = append(fact.CheckRuns, record.AttemptRun{Name: name, ID: run.ID})
		switch listenerOutcome(run.Conclusion) {
		case "failed":
			fact.Failing = append(fact.Failing, name)
		case "cancelled":
			fact.Cancelled = append(fact.Cancelled, name)
		}
	}
	slices.SortFunc(fact.CheckRuns, func(a, b record.AttemptRun) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(fact.Failing)
	slices.Sort(fact.Cancelled)
	digest := sha256.New()
	for _, run := range fact.CheckRuns {
		digest.Write([]byte(run.Name + "\x00" + strconv.FormatInt(run.ID, 10) + "\x01"))
	}
	fact.Snapshot = "pass-" + hex.EncodeToString(digest.Sum(nil)[:16])
	return fact, completed, true
}
