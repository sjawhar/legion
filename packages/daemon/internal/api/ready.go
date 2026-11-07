package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/record"
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

// readyChecks refuses a READY the workflow must not take on to the merge: one its issue has no
// pull request recorded for (NO_PULL_REQUEST), one GitHub shows closed without merging
// (READY_PULL_REQUEST_CLOSED), one at another head than READY names (READY_HEAD_MOVED), one whose
// review round recorded no approval at all (READY_NOT_APPROVED), and one GitHub cannot merge at
// all for a conflict with its base (READY_HEAD_CONFLICTS). Whether the merge then happens is
// GitHub's to decide, under the base branch's own rules: the daemon reads none of the head's
// check runs or commit statuses and judges none of them, and its submission of the merge
// (record.ReadyMerge) waits on GitHub for whatever those rules require of the head. It judges
// none of the commits above the approved head either: the daemon keeps no check of its own on
// what sits above it, and the repository's branch rules decide the merge (the owner's ruling,
// dispatch://INFRA-714/ask/beea7685).
//
// A pull request already merged has nothing left to gate: a person merged it before READY, which
// the workflow takes on to the production check, and no commit can change its head, so READY is
// published without reading it, and the note says so. Otherwise the head must be the one GitHub
// shows (the two clocks below). A read GitHub fails is refused as GitHub's failure, to retry, not
// the head's.
//
// The note is the completion's answer (HandoffCompleteResponse.Note): the already-merged sentence,
// or the by-hand note (mergeReadyPolicy) when the pull request is one a person merges, and nothing
// otherwise. That by-hand note is the answer's mergeNote too, which the completion records for the
// packet the daemon tells. A by-hand READY, unlike an ordinary one, waits for an unfinished disarm
// (READY_DISARM_PENDING), so no arm an earlier ordinary READY left can merge the head a person is
// to merge. The answer also carries the record as the checks read it and GitHub's clock in their
// read of the pull request, which the workflow holds the READY to (readyAnswer).
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
func readyChecks(ctx context.Context, github githubrest.Client, rules config.MergeRules, issue, readyHead, approvedHead string, pr *record.PullRequest, disarmPending bool) (readyAnswer, *refusal) {
	if pr == nil || pr.Number <= 0 {
		return readyAnswer{}, &refusal{http.StatusConflict, "NO_PULL_REQUEST", fmt.Sprintf("%s has no pull request recorded", issue)}
	}
	var pull struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
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
		answer.note = fmt.Sprintf("pull request #%d is already merged, so READY was published without reading its head", number)
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
	// The merger's task names the head its review round approved, and a round that recorded none
	// has no approval for READY to publish. Nothing else holds the head to that approval: the
	// daemon keeps no check of its own on what sits above the approved head, and the repository's
	// branch rules decide the merge (the owner's ruling, dispatch://INFRA-714/ask/beea7685).
	if approvedHead == "" {
		return readyAnswer{}, &refusal{http.StatusConflict, "READY_NOT_APPROVED", fmt.Sprintf("%s's review round recorded no approval, so READY has no approved head for head %s of pull request #%d: the head goes back to reviewing, so the reviewer reviews it; tell the architect", issue, head, number)}
	}
	// A pull request GitHub cannot merge gets no merge ref, no pull_request run of its own and no
	// merge: it is refused by name, so the implementer brings the base into the branch rather than
	// leaving a head GitHub will never merge for the daemon to submit. GitHub reports the conflict
	// as mergeable_state "dirty" (computed shortly after each push; "unknown" while it computes,
	// which is no conflict yet).
	if pull.MergeableState == "dirty" {
		return readyAnswer{}, &refusal{http.StatusConflict, "READY_HEAD_CONFLICTS", fmt.Sprintf("head %s of pull request #%d conflicts with its base %s, which GitHub cannot merge and starts no pull_request CI for: the implementer brings %s into the branch with a forward merge; tell the architect", head, number, pull.Base.Ref, pull.Base.Ref)}
	}
	// The project's merge policy decides whether a person merges this pull request, whatever its
	// base branch requires of the head. Unlike ordinary READY, a by-hand READY waits for an
	// unfinished disarm: it must not leave an arm from an earlier ordinary READY standing at this
	// new head.
	mergeNote, refused := mergeReadyPolicy(ctx, github, rules, number)
	if refused != nil {
		return readyAnswer{}, refused
	}
	if mergeNote != "" && disarmPending {
		return readyAnswer{}, &refusal{http.StatusConflict, "READY_DISARM_PENDING", fmt.Sprintf("pull request #%d matches a by_hand merge rule, and an earlier auto-merge disarm is unfinished: complete again once it finishes, so no stale arm can merge this head", number)}
	}
	answer.note, answer.mergeNote = mergeNote, mergeNote
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
// file among them (githubrest.ChangedPaths), and how many files GitHub listed.
func pullRequestPaths(ctx context.Context, github githubrest.Client, number int) ([]string, int, error) {
	files, err := githubrest.GetPages[githubrest.ChangedFile](ctx, github, fmt.Sprintf("/pulls/%d/files", number))
	if err != nil {
		return nil, 0, err
	}
	return githubrest.ChangedPaths(files), len(files), nil
}

// shortSHA is sha as a refusal names it, its first twelve characters.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
