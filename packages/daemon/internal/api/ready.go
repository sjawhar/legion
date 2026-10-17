package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/requiredchecks"
)

// refusal is a completion's refusal before its fact is applied: the status, code and sentence
// writeFailure answers with. Nothing is recorded under the completion's key, so the corrected call
// at the same head is applied rather than answered HANDOFF_ALREADY_RECORDED.
type refusal struct {
	status        int
	code, message string
}

func (r *refusal) write(w http.ResponseWriter) { writeFailure(w, r.status, r.code, r.message) }

// githubReadFailed is the refusal of a completion GitHub failed to answer a read for: GitHub's
// failure, not the head's, so the worker completes again. Nothing retries in the daemon; a GitHub
// outage holds every completion until it ends, as it holds every webhook-driven transition.
func githubReadFailed(err error) *refusal {
	return &refusal{http.StatusBadGateway, "GITHUB_READ_FAILED", fmt.Sprintf("%v; GitHub's failure, not the head's: complete again", err)}
}

// readyChecks refuses a READY whose head GitHub will never merge for its checks, or whose merge
// would carry the issue's handoffs onto the base branch. A pull request already merged has nothing
// left to gate: a person merged it before READY, which the workflow takes on to the production
// check, and no commit can change its head, so READY is published without reading it. Otherwise the
// head must not hold .legion/<issue>/ (headCarries), which retro's last commit removes
// (dispatch://LEGION-605): a squash merge commits the head merged into the base, and nothing on the
// base branch reads a handoff. A read GitHub fails is refused as GitHub's failure, to retry, not the
// head's. Then no check the base branch requires - its rulesets' required status checks and its
// branch protection's - may have failed on the pull request's head or be missing from it, and every
// workflow its rulesets require must have a run for the head that has not failed
// (requiredchecks.Required, requiredchecks.Workflows), judged by the rule the workflow's checks
// verdict judges by too (classify.Judge). A check or workflow still running is published: the
// daemon's submission of the merge (record.ReadyMerge) waits on GitHub for it, and a red it settles
// to withdraws the READY. A head reports none of them when its push skipped CI when it should not
// have (a `skip-checks: true` trailer on a push that touched more than .legion/), or when the pull
// request conflicts with its base, since GitHub starts no pull_request run for a pull request it
// cannot merge; it is refused here, naming the head, the check or workflow, and the conflict once
// GitHub shows it, rather than left for GitHub to block the merge. A required workflow another
// repository defines (an organization ruleset can require one) never matches a run here, since a
// run is matched in the repository that defines the workflow and belongs to the one it ran for; its
// refusal names that repository instead.
//
// A base branch that requires no check has no check to refuse, and READY is published once the head
// carries no handoffs. The note says so rather than reading like a head whose every required check
// was read and passed. The note is the completion's answer (HandoffCompleteResponse.Note). The
// answer's mergeNote is the by-hand note (mergeReadyPolicy), empty unless the pull request is one a
// person merges: the completion records it for the packet the daemon tells, and it is the note too.
// The answer also carries the record as the checks read it and GitHub's clock in their read of the
// pull request, which the workflow holds the READY to (readyAnswer).
//
// GitHub can answer a read from a replica that has not seen the newest push, while the record holds
// it from that push's webhook. Both clocks are the pull request's updated_at, to the second, so a
// record at another head whose clock is not before GitHub's answer, a same-second tie included, may
// be ahead of it, and READY is refused as a moved head; a tie costs one retry, until the head's own
// webhook lands. The clocks are compared rather than asking GitHub whether the READY's head
// descends from the recorded one: that answers ancestry, not freshness, and after a force push it
// would refuse a lagging record's READY until the webhook arrived. A pull request closed without
// merging is refused by GitHub's own state, since its close's webhook can arrive after the READY's
// advance of the record, which would then drop it as older (READY_PULL_REQUEST_CLOSED).
//
// Before the head's handoffs are read, the head must be the approved code plus retro's commits
// alone (unreviewedChange). Nothing else holds those commits to retro's: the merger only quotes
// them, and a READY a person merges is held to it too.
func readyChecks(ctx context.Context, github githubrest.Client, repository ghrepo.Repository, rules config.MergeRules, issue, readyHead, approvedHead string, pr *record.PullRequest, disarmPending bool) (readyAnswer, *refusal) {
	if pr == nil || pr.Number <= 0 {
		return readyAnswer{}, &refusal{http.StatusConflict, "NO_PULL_REQUEST", fmt.Sprintf("%s has no pull request recorded", issue)}
	}
	var pull struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref  string `json:"ref"`
			Repo struct {
				ID int64 `json:"id"`
			} `json:"repo"`
		} `json:"base"`
		// MergeableState is "dirty" while the pull request conflicts with its base.
		MergeableState string    `json:"mergeable_state"`
		State          string    `json:"state"`
		Merged         bool      `json:"merged"`
		UpdatedAt      time.Time `json:"updated_at"`
	}
	if err := github.Get(ctx, fmt.Sprintf("/pulls/%d", pr.Number), &pull); err != nil {
		return readyAnswer{}, githubReadFailed(err)
	}
	answer := readyAnswer{recordedHead: pr.HeadSHA, recordedHeadAt: pr.HeadUpdatedAt, headAt: pull.UpdatedAt}
	head := shortSHA(pull.Head.SHA)
	number := pr.Number
	if pull.Merged {
		answer.note = fmt.Sprintf("pull request #%d is already merged, so READY was published without reading its head's checks or handoffs", number)
		return answer, nil
	}
	if pull.State == "closed" {
		return readyAnswer{}, &refusal{http.StatusConflict, "READY_PULL_REQUEST_CLOSED", fmt.Sprintf("pull request #%d is closed without merging, so there is no merge to submit: tell the architect, whose decision it is whether the work is reopened", number)}
	}
	if pull.Head.SHA != readyHead {
		return readyAnswer{}, &refusal{http.StatusConflict, "READY_HEAD_MOVED", fmt.Sprintf("pull request #%d now heads at %s, not READY's %s; the daemon submits only the head READY names: build a new packet and complete again", number, pull.Head.SHA, readyHead)}
	}
	if pr.HeadSHA != pull.Head.SHA && !pr.HeadUpdatedAt.IsZero() && !pull.UpdatedAt.After(pr.HeadUpdatedAt) {
		return readyAnswer{}, &refusal{http.StatusConflict, "READY_HEAD_MOVED", fmt.Sprintf("the daemon recorded pull request #%d at head %s, at %s, no earlier than GitHub's answer of %s, at %s, which may not have caught up with that push: build a new packet once GitHub shows the new head, and complete again",
			number, pr.HeadSHA, pr.HeadUpdatedAt.UTC().Format(time.RFC3339), pull.Head.SHA, pull.UpdatedAt.UTC().Format(time.RFC3339))}
	}
	if refused := unreviewedChange(ctx, github, issue, number, approvedHead, pull.Head.SHA); refused != nil {
		return readyAnswer{}, refused
	}
	dir := handoffDir(issue)
	switch carries, err := headCarries(ctx, github, dir, pull.Head.SHA); {
	case err != nil:
		return readyAnswer{}, &refusal{http.StatusBadGateway, "GITHUB_READ_FAILED", fmt.Sprintf("GitHub's read of %s/ at head %s of pull request #%d failed, so whether the head still carries it is unknown: complete again; this is GitHub's failure, not the head's: %v", dir, head, number, err)}
	case carries:
		return readyAnswer{}, &refusal{http.StatusConflict, "READY_HEAD_CARRIES_HANDOFFS", fmt.Sprintf("head %s of pull request #%d still carries %s/, this issue's handoffs, which its merge would carry onto the base branch: retro's last commit removes them, so the issue goes back to retro; tell the architect", head, number, dir)}
	}
	// A pull request GitHub cannot merge gets no pull_request run and no merge: it is refused by
	// name, before its checks are read, since a missing check would otherwise be reported as the
	// cause. GitHub reports the conflict as mergeable_state "dirty" (computed shortly after each
	// push; "unknown" while it computes, which the checks below then judge as they stand).
	if pull.MergeableState == "dirty" {
		return readyAnswer{}, &refusal{http.StatusConflict, "READY_HEAD_CONFLICTS", fmt.Sprintf("head %s of pull request #%d conflicts with its base %s, which GitHub cannot merge and starts no pull_request CI for: the implementer brings %s into the branch with a forward merge; tell the architect", head, number, pull.Base.Ref, pull.Base.Ref)}
	}
	// The project's merge policy is read once, before the checks: a by_hand pull request is READY
	// for a person to merge however its base branch's checks are configured. Unlike ordinary READY,
	// a by-hand READY waits for an unfinished disarm: it must not leave an arm from an earlier
	// ordinary READY standing at this new head.
	mergeNote, refused := mergeReadyPolicy(ctx, github, rules, number)
	if refused != nil {
		return readyAnswer{}, refused
	}
	if mergeNote != "" && disarmPending {
		return readyAnswer{}, &refusal{http.StatusConflict, "READY_DISARM_PENDING", fmt.Sprintf("pull request #%d matches a by_hand merge rule, and an earlier auto-merge disarm is unfinished: complete again once it finishes, so no stale arm can merge this head", number)}
	}
	answer.note, answer.mergeNote = mergeNote, mergeNote
	required, err := requiredchecks.Required(ctx, github, pull.Base.Ref)
	if err != nil {
		return readyAnswer{}, githubReadFailed(err)
	}
	if len(required.Checks) == 0 && len(required.Workflows) == 0 {
		if mergeNote == "" {
			answer.note = fmt.Sprintf("no check is required on %q of %s, so READY was published without reading the head's checks", pull.Base.Ref, repository)
		}
		return answer, nil
	}
	var results map[string]string
	if len(required.Checks) > 0 {
		if results, err = headCheckResults(ctx, github, pull.Head.SHA); err != nil {
			return readyAnswer{}, githubReadFailed(err)
		}
	}
	var workflows []requiredchecks.WorkflowRun
	if len(required.Workflows) > 0 {
		if workflows, err = requiredchecks.Workflows(ctx, github, pull.Head.SHA, required.Workflows); err != nil {
			return readyAnswer{}, githubReadFailed(err)
		}
	}
	// notGreen is READY's refusal for one required check or workflow's standing on the head, nil
	// when it succeeded there or is still running.
	notGreen := func(check classify.Standing, what, missing string) *refusal {
		var message string
		switch name := check.Name; {
		case check.Result == classify.Missing:
			message = fmt.Sprintf("head %s of pull request #%d has %s the %s %q: its push may have skipped CI when it should not have; tell the architect", head, number, missing, what, name)
		case check.Red():
			message = fmt.Sprintf("the %s %q ended %s on head %s of pull request #%d", what, name, check.Result, head, number)
		default:
			return nil
		}
		return &refusal{http.StatusConflict, "READY_CHECKS_NOT_GREEN", message}
	}
	for _, check := range classify.Judge(required.Checks, results) {
		if refused := notGreen(check, "required check", "no result for"); refused != nil {
			return readyAnswer{}, refused
		}
	}
	for _, workflow := range workflows {
		if workflow.Result == classify.Missing && workflow.RepositoryID != pull.Base.Repo.ID {
			return readyAnswer{}, &refusal{http.StatusConflict, "READY_CHECKS_NOT_GREEN", fmt.Sprintf("the required workflow %q is defined in repository %d, not in pull request #%d's own (%d): Legion matches a workflow's runs only in the repository that defines it, so it found no run of it on head %s and cannot confirm it passed; tell the architect", workflow.Path, workflow.RepositoryID, number, pull.Base.Repo.ID, head)}
		}
		if refused := notGreen(workflow.Standing(), "required workflow", "no run of"); refused != nil {
			return readyAnswer{}, refused
		}
	}
	return answer, nil
}

// readyAnswer is what READY's checks (readyChecks) leave for a merger's ready: true: the note the
// completion answers with, the by-hand note the completion records when a person merges the pull
// request, the record's head and clock as the checks read them, and GitHub's clock in their read
// of the pull request, which the workflow holds the READY to and stamps its head with
// (intake.HandoffComplete.RecordedHead, RecordedHeadAt and HeadAt).
type readyAnswer struct {
	note, mergeNote        string
	recordedHead           string
	recordedHeadAt, headAt time.Time
}

// pullRequestFilesLimit is the most changed files GitHub lists for a pull request: a pull request
// that reaches it may change a path the list never shows.
const pullRequestFilesLimit = 3000

// byHandNotePrefix opens READY's by-hand note (mergeReadyPolicy) on the wire: the merger, the issue
// and the merge queue role read a note that begins with it as one a person merges.
const byHandNotePrefix = "MANUAL_MERGE_REQUIRED:"

// mergeReadyPolicy is the project's merge policy for pull request number: the by-hand note
// (byHandNotePrefix) when its change touches a by_hand path, which keeps the daemon from submitting
// its merge, and nothing otherwise. A list GitHub cut at pullRequestFilesLimit leaves a by_hand path
// unread, so READY is refused rather than submitted on it.
func mergeReadyPolicy(ctx context.Context, github githubrest.Client, rules config.MergeRules, number int) (string, *refusal) {
	if len(rules.ByHand) == 0 {
		return "", nil
	}
	paths, files, err := pullRequestPaths(ctx, github, number)
	if err != nil {
		return "", githubReadFailed(err)
	}
	if rules.MatchByHand(paths) {
		return fmt.Sprintf("%s pull request #%d matches a by_hand merge rule; do not auto-arm it", byHandNotePrefix, number), nil
	}
	if files >= pullRequestFilesLimit {
		return "", &refusal{http.StatusConflict, "READY_FILES_TRUNCATED", fmt.Sprintf("pull request #%d changes at least %d files, the most GitHub lists, so whether it touches a by_hand path is unknown: a person merges it, or the change is split; tell the architect", number, pullRequestFilesLimit)}
	}
	return "", nil
}

// pullRequestPaths is every path pull request number's change touches, both names of a renamed
// file among them (changedPaths), and how many files GitHub listed.
func pullRequestPaths(ctx context.Context, github githubrest.Client, number int) ([]string, int, error) {
	files, err := githubrest.GetPages[changedFile](ctx, github, fmt.Sprintf("/pulls/%d/files", number))
	if err != nil {
		return nil, 0, err
	}
	return changedPaths(files), len(files), nil
}

// changedFile is one file GitHub lists in a change, a pull request's or a compare's.
type changedFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
}

// changedPaths is every path files touch: each file's name, and a renamed file's old name too,
// since moving a file out of a path changes that path.
func changedPaths(files []changedFile) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Filename)
		if file.PreviousFilename != "" {
			paths = append(paths, file.PreviousFilename)
		}
	}
	return paths
}

// shortSHA is sha as a refusal names it, its first twelve characters.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// headCarries is whether head holds dir, a directory relative to the repository root: GitHub's
// contents read of that path at head answers 404 when it does not. Any other failure of the read is
// an error, which leaves the answer unknown.
func headCarries(ctx context.Context, github githubrest.Client, dir, head string) (bool, error) {
	err := github.Get(ctx, "/contents/"+dir+"?ref="+url.QueryEscape(head), nil)
	var answer *githubrest.Answer
	if errors.As(err, &answer) && answer.Status == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

// handoffDir is the directory issue's handoffs live under, relative to the repository root, with no
// trailing slash: .legion/<issue>/<phase>.json is written there by each file-backed phase's role,
// and retro's last commit removes the directory from the head a human merges, so none reaches main
// (dispatch://LEGION-605). READY reads whether the head still carries it (headCarries); the daemon
// reads nothing inside it.
func handoffDir(issue string) string { return ".legion/" + issue }

// compareFilesLimit is the most changed files GitHub's compare of two commits lists: "it includes
// up to 300 changed files for the entire comparison" (REST API, Compare two commits,
// https://docs.github.com/en/rest/commits/commits#compare-two-commits). A comparison that reaches
// it may change a path the list never shows.
const compareFilesLimit = 300

// unreviewedChangeNamed is how many paths READY_UNREVIEWED_CHANGE names before it counts the rest.
const unreviewedChangeNamed = 10

// unreviewedChange is READY's refusal of head, pull request number's, when it carries a change no
// review approved, nil when it carries none. approved is the head issue's review round approved
// (record.ApprovedHead), and only retro's commits may sit above it: its learnings under
// docs/solutions/, and its last commit's removal of the issue's handoffs, .legion/<issue>/
// (handoffDir). A round with no approval has no head to hold READY to (READY_NOT_APPROVED).
// Otherwise GitHub's compare of approved with head, as the implement App, must show approved to be
// head's ancestor ("ahead", or "identical" for a READY at the approved head itself; "diverged" or
// "behind" is not the approved code plus anything), list its files, and list fewer than
// compareFilesLimit; every path it lists, a rename's old name included, must be retro's. Anything
// else is refused READY_UNREVIEWED_CHANGE, naming what it found, and the remedy for each is the
// same: the reviewer reviews this head as a new round. An approved head
// GitHub does not know is refused the same way; any other failure of the read is GitHub's, to
// retry. The compare asks for one commit a page: the file list and status cover the whole
// comparison on its first page whatever the page size, and a smaller page reads less.
func unreviewedChange(ctx context.Context, github githubrest.Client, issue string, number int, approved, head string) *refusal {
	const remedy = "the head goes back to reviewing, so the reviewer reviews it; tell the architect"
	if approved == "" {
		return &refusal{http.StatusConflict, "READY_NOT_APPROVED", fmt.Sprintf("%s's review round recorded no approval, so READY has no approved head to hold head %s of pull request #%d to: %s", issue, shortSHA(head), number, remedy)}
	}
	unreviewed := func(format string, args ...any) *refusal {
		return &refusal{http.StatusConflict, "READY_UNREVIEWED_CHANGE", fmt.Sprintf(format, args...) + "; " + remedy}
	}
	var compare struct {
		Status string         `json:"status"`
		Files  *[]changedFile `json:"files"`
	}
	err := github.Get(ctx, "/compare/"+approved+"..."+head+"?per_page=1", &compare)
	approvedAt := fmt.Sprintf("%s, the head %s's review round approved", shortSHA(approved), issue)
	var answer *githubrest.Answer
	if errors.As(err, &answer) && answer.Status == http.StatusNotFound {
		return unreviewed("GitHub does not know %s, or cannot compare it with head %s of pull request #%d", approvedAt, shortSHA(head), number)
	}
	if err != nil {
		return githubReadFailed(err)
	}
	compared := fmt.Sprintf("GitHub's compare of %s, with head %s of pull request #%d", approvedAt, shortSHA(head), number)
	if compare.Status != "ahead" && compare.Status != "identical" {
		return unreviewed("%s is %q: the approved head is not an ancestor of the head, so the head is not the approved code plus retro's commits", compared, compare.Status)
	}
	if compare.Files == nil {
		return unreviewed("%s answered no file list, so what changed above the approved head is unknown", compared)
	}
	files := *compare.Files
	handoffs := handoffDir(issue) + "/"
	var outside []string
	for _, path := range changedPaths(files) {
		if !strings.HasPrefix(path, "docs/solutions/") && !strings.HasPrefix(path, handoffs) {
			outside = append(outside, path)
		}
	}
	if len(outside) > 0 {
		named := strings.Join(outside[:min(len(outside), unreviewedChangeNamed)], ", ")
		if len(outside) > unreviewedChangeNamed {
			named += fmt.Sprintf(" and %d more", len(outside)-unreviewedChangeNamed)
		}
		return unreviewed("head %s of pull request #%d changes %s since %s: only retro's commits, to docs/solutions/ and %s, may sit above it, so no review approved that change", shortSHA(head), number, named, approvedAt, handoffs)
	}
	if len(files) >= compareFilesLimit {
		return unreviewed("%s lists %d changed files, the most it lists, so a path past them is unread and may be one no review approved", compared, len(files))
	}
	return nil
}

// headCheckResults is each check and commit status reported on sha, by name: success, pending, or
// the failing conclusion or state (classify.Judge's results). A check run that ended neutral or
// skipped counts as a success, as GitHub counts it for a required check.
func headCheckResults(ctx context.Context, github githubrest.Client, sha string) (map[string]string, error) {
	type run struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}
	runs, err := githubrest.GetListPages[run](ctx, github, "/commits/"+sha+"/check-runs", "check_runs")
	if err != nil {
		return nil, err
	}
	results := map[string]string{}
	// GitHub documents no ordering for this list (unlike the combined-status endpoint below,
	// whose docs guarantee "the most recent status for each context"), and a cancelled run
	// superseded by a concurrency group's newer run can list after the newer run's own success
	// (pr-title.yaml's `edited` re-trigger after a concurrency-group cancellation is one way this
	// happens). Keeping whichever run a name is seen last in the list would then let a cancelled
	// run overwrite a later success. Check run IDs are assigned at creation and never reused, so
	// the highest ID per name is always its most recent run, regardless of list order.
	latestID := map[string]int64{}
	for _, run := range runs {
		if prevID, seen := latestID[run.Name]; seen && prevID >= run.ID {
			continue
		}
		latestID[run.Name] = run.ID
		results[run.Name] = classify.RunResult(run.Status, run.Conclusion)
	}
	var combined struct {
		Statuses []struct {
			Context string `json:"context"`
			State   string `json:"state"`
		} `json:"statuses"`
	}
	if err := github.Get(ctx, fmt.Sprintf("/commits/%s/status?per_page=100", sha), &combined); err != nil {
		return nil, err
	}
	for _, status := range combined.Statuses {
		results[status.Context] = status.State
	}
	return results, nil
}
