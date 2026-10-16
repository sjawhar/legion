// Package ghautomerge submits and disarms a pull request's merge through GitHub's GraphQL API: the
// way the daemon merges an accepted READY head under the repository's own rules, and makes sure a
// pull request armed for one READY head never merges another.
package ghautomerge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// Disarmed is what Disarm found and turned off.
type Disarmed struct {
	// Missing is GitHub answering NOT_FOUND for the pull request itself: there is nothing to disarm.
	Missing bool
	// Kept is the pull request's head being the READY head Disarm was told to keep: whatever is
	// armed there is that READY's, and is left as it is.
	Kept bool
	// AutoMerge is an auto-merge Disarm disabled.
	AutoMerge bool
	// Queue is a merge-queue entry Disarm removed.
	Queue bool
}

const read = `query PullRequestAutoMerge($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    autoMergeAllowed
    pullRequest(number: $number) {
      id state headRefOid mergeStateStatus updatedAt isMergeQueueEnabled
      autoMergeRequest { enabledAt } mergeQueueEntry { enqueuedAt }
    }
  }
}`

const (
	disable = `mutation DisableAutoMerge($id: ID!) { disablePullRequestAutoMerge(input: {pullRequestId: $id}) { clientMutationId } }`
	dequeue = `mutation DequeuePullRequest($id: ID!) { dequeuePullRequest(input: {id: $id}) { clientMutationId } }`
	enable  = `mutation EnableAutoMerge($id: ID!, $head: GitObjectID!, $method: PullRequestMergeMethod!) {
  enablePullRequestAutoMerge(input: {pullRequestId: $id, expectedHeadOid: $head, mergeMethod: $method}) { clientMutationId }
}`
	merge = `mutation MergeREADYHead($id: ID!, $head: GitObjectID!, $method: PullRequestMergeMethod!) {
  mergePullRequest(input: {pullRequestId: $id, expectedHeadOid: $head, mergeMethod: $method}) { clientMutationId }
}`
)

// Outcome is how Submit ended: a submission it asks to run again is a wait it returns instead.
type Outcome string

const (
	// Missing is GitHub answering NOT_FOUND for the pull request itself: there is nothing to merge.
	Missing Outcome = "missing"
	// Stale is a submission the record no longer covers: the issue no longer awaits READY's head.
	Stale Outcome = "stale"
	// Already is a pull request GitHub already merged at READY's head.
	Already Outcome = "already"
	// Merged is GitHub merging READY's head now.
	Merged Outcome = "merged"
)

// Keep answers the READY head an arm is legitimate for, empty when none is. Disarm asks it only
// after reading GitHub.
type Keep func(context.Context) (string, error)

// Disarm turns off what would merge pull request number of repo without a person, through github,
// a GraphQL client (githubrest.GraphQLAPI): its auto-merge where GitHub has it enabled, then its
// merge-queue entry where it has one. Auto-merge goes first because GitHub enqueues again a pull
// request it dequeues while auto-merge is still enabled; the dequeue is attempted even when the
// disable fails, so a refused disable never leaves the entry standing, and both failures are
// returned. It then reads the pull request once more and fails, so its row retries, when anything
// not kept is still armed: a mutation GitHub answered is not proof the merge is off.
//
// It decides by the pull request's state when it runs, never by when anything was armed: GitHub
// enqueues an armed pull request only once its checks pass, so a queue entry can postdate the arm
// it belongs to. It reads GitHub first and then asks keep for the READY head an arm is legitimate
// for: an arm is made only once its READY is recorded, so an arm the read finds is covered by what
// keep answers after it, where asked the other way round a READY recorded between the two would
// have its arm turned off. While the pull request's head on GitHub is that head, nothing is
// touched (Kept). A pull request with nothing armed is left alone, so a retried Disarm changes
// nothing. A NOT_FOUND at the pull request is Missing, not an error; one anywhere else, such as a
// repository the token cannot see, is an error.
func Disarm(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, number int, keep Keep) (Disarmed, error) {
	pull, err := readPull(ctx, github, repo, number)
	if err != nil {
		return Disarmed{}, err
	}
	if pull == nil {
		return Disarmed{Missing: true}, nil
	}
	if !pull.armed() {
		return Disarmed{}, nil
	}
	head, err := keep(ctx)
	if err != nil {
		return Disarmed{}, err
	}
	if pull.kept(head) {
		return Disarmed{Kept: true}, nil
	}
	var disarmed Disarmed
	var failures []error
	if pull.AutoMergeRequest != nil {
		if err := github.GraphQL(ctx, disable, map[string]any{"id": pull.ID}, nil); err != nil {
			failures = append(failures, fmt.Errorf("disable the auto-merge of pull request #%d of %s: %w", number, repo, err))
		} else {
			disarmed.AutoMerge = true
		}
	}
	if pull.MergeQueueEntry != nil {
		if err := github.GraphQL(ctx, dequeue, map[string]any{"id": pull.ID}, nil); err != nil {
			failures = append(failures, fmt.Errorf("take pull request #%d of %s out of the merge queue: %w", number, repo, err))
		} else {
			disarmed.Queue = true
		}
	}
	if len(failures) != 0 {
		return disarmed, errors.Join(failures...)
	}
	after, err := readPull(ctx, github, repo, number)
	if err != nil || after == nil || !after.armed() || after.kept(head) {
		return disarmed, err
	}
	return disarmed, fmt.Errorf("pull request #%d of %s is still armed after its disarm (auto-merge enabled: %t, in the merge queue: %t)",
		number, repo, after.AutoMergeRequest != nil, after.MergeQueueEntry != nil)
}

// Submit asks GitHub to merge pull request number of repo at head, the head an accepted READY
// named, through github, a GraphQL client. It reads GitHub first and then asks keep for the READY
// head the record still awaits, so a READY withdrawn or left between the two submits nothing. A
// base that requires a merge queue gets the pull request enqueued, a pull request GitHub reports
// mergeable now is merged, and a repository that allows auto-merge gets it armed; every mutation
// carries head as GitHub's expected head. A repository that allows none of those waits until GitHub
// reports the pull request mergeable (Unmergeable). GitHub's own rules decide each answer; Submit
// bypasses none.
//
// Only GitHub merging the pull request at head ends a submission, or the record leaving that READY
// (Stale). An arm or a merge-queue entry at head is a wait (Armed), not the end: a disarm sent
// before READY can still land, or a merge queue drop the pull request, and the next run then
// submits again. While the record still awaits head, a pull request GitHub shows closed, merged at
// another head, or at another head is a wait too (Lagging): the webhook that moves the record has
// not been applied yet, or GitHub's read is the one behind, and the next run decides again. GitHub
// always answers updatedAt, so a read without it fails rather than leaving the wait unbounded.
//
// A mutation GitHub refuses is read again: when GitHub still stands where it refused it (the same
// open pull request at head, nothing armed, the same submission chosen), a retry would meet the
// same refusal, and Submit returns it as *Refused. When that read fails, its error is returned,
// the refusal as its context, so a rate limit it meets reaches the caller (githubrest.RetryAfter).
// Any other failure, an HTTP or network one included, is returned as it is, for the row to retry.
func Submit(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, number int, head string, keep Keep) (Outcome, error) {
	answer, err := readRepository(ctx, github, repo, number)
	if err != nil {
		return "", err
	}
	pull := answer.PullRequest
	switch {
	case pull == nil:
		return Missing, nil
	case pull.State == "MERGED" && pull.HeadRefOid == head:
		return Already, nil
	}
	kept, err := keep(ctx)
	if err != nil {
		return "", err
	}
	switch {
	case kept != head:
		return Stale, nil
	case !pull.openAt(head) && pull.UpdatedAt.IsZero():
		return "", fmt.Errorf("GitHub's read of pull request #%d of %s carries no updatedAt to bound its disagreement with the record", number, repo)
	case !pull.openAt(head):
		return "", &Lagging{Number: number, Head: head, State: pull.State, GitHubHead: pull.HeadRefOid, Since: pull.UpdatedAt}
	case pull.armed():
		return "", &Armed{Number: number, Repo: repo, Head: head}
	}
	chosen, ok := choose(answer)
	if !ok {
		return "", &Unmergeable{Number: number, Repo: repo, Head: head, State: pull.MergeStateStatus}
	}
	sent := github.GraphQL(ctx, chosen.mutation, map[string]any{"id": pull.ID, "head": head, "method": "SQUASH"}, nil)
	switch {
	case sent == nil && chosen.merges:
		return Merged, nil
	case sent == nil:
		return "", &Armed{Number: number, Repo: repo, Head: head}
	}
	failed := fmt.Errorf("%s pull request #%d of %s at READY's head %s: %w", chosen.action, number, repo, head, sent)
	var refusal *githubrest.GraphQLError
	if !errors.As(sent, &refusal) || !terminalMutationRefusal(refusal) {
		return "", failed
	}
	after, err := readRepository(ctx, github, repo, number)
	if err != nil {
		return "", fmt.Errorf("read pull request #%d of %s again after GitHub's refusal to %v: %w", number, repo, failed, err)
	}
	if after.PullRequest == nil || !after.PullRequest.openAt(head) || after.PullRequest.armed() {
		return "", failed
	}
	if again, ok := choose(after); !ok || again != chosen {
		return "", failed
	}
	return "", &Refused{Number: number, Head: head, Action: chosen.action, Message: refusal.Error()}
}

// terminalMutationRefusal reports GraphQL mutation errors that no backoff can repair. GitHub's
// rate-limit, service and unknown error types retry: only a confirmed unchanged pull request plus
// one of these terminal rejections becomes Refused.
func terminalMutationRefusal(refusal *githubrest.GraphQLError) bool {
	if len(refusal.Failures) == 0 {
		return false
	}
	for _, failure := range refusal.Failures {
		switch failure.Type {
		case "FORBIDDEN", "UNPROCESSABLE", "VALIDATION_FAILED":
		default:
			return false
		}
	}
	return true
}

// Refused is GitHub refusing Submit's mutation where a retry would meet the same refusal: Action is
// what Submit asked of pull request Number at READY's Head, and Message is GitHub's answer.
type Refused struct {
	Number                int
	Head, Action, Message string
}

func (r *Refused) Error() string {
	return fmt.Sprintf("GitHub refused to %s pull request #%d at READY's head %s and still stands where it refused, so a retry meets the same refusal: %s",
		r.Action, r.Number, r.Head, r.Message)
}

// LagBound is how long GitHub's last change to a pull request stands before a disagreement with the
// record is a webhook the daemon never received rather than one still on its way: far longer than
// GitHub takes to deliver one.
const LagBound = 10 * time.Minute

// Lagging is GitHub showing pull request Number in State at GitHubHead while the record still awaits
// its merge at READY's Head: a close or a push whose webhook the record has not applied yet, or a
// GitHub read behind the record. Since is GitHub's last change to the pull request, from the same
// read. It is a wait (wait.ErrWaiting): the next run reads both again, until it is Stalled.
type Lagging struct {
	Number                  int
	Head, State, GitHubHead string
	Since                   time.Time
}

func (l *Lagging) Error() string {
	return fmt.Sprintf("GitHub shows pull request #%d %s at %s while the record still awaits its merge at READY's head %s: waiting for the two to agree",
		l.Number, l.State, l.GitHubHead, l.Head)
}

func (l *Lagging) Is(target error) bool { return target == wait.ErrWaiting }

// Stalled says whether at now the disagreement has stood on GitHub longer than LagBound: GitHub's
// own clock bounds it, never how often the row has tried.
func (l *Lagging) Stalled(now time.Time) bool { return now.Sub(l.Since) > LagBound }

// Unmergeable is GitHub reporting pull request Number of Repo in merge State at READY's Head, not
// mergeable yet, in a repository that allows no auto-merge: Submit merges it once GitHub reports it
// mergeable. It is a wait (wait.ErrWaiting).
type Unmergeable struct {
	Number      int
	Repo        ghrepo.Repository
	Head, State string
}

func (u *Unmergeable) Error() string {
	return fmt.Sprintf("pull request #%d of %s is %s at READY's head %s, and the repository allows no auto-merge: the daemon merges it once GitHub reports it mergeable",
		u.Number, u.Repo, u.State, u.Head)
}

func (u *Unmergeable) Is(target error) bool { return target == wait.ErrWaiting }

// Routine says whether the repository's own rules end the wait without anyone acting for Legion: a
// required check still running or a required review (BLOCKED), or GitHub still computing the merge
// state (UNKNOWN). A branch to update (BEHIND), a draft (DRAFT) or a conflict (DIRTY) waits on
// someone.
func (u *Unmergeable) Routine() bool { return u.State == "BLOCKED" || u.State == "UNKNOWN" }

// Armed is pull request Number of Repo armed for auto-merge, or in its merge queue, at READY's
// Head: GitHub merges it under the repository's own rules. It is a wait (wait.ErrWaiting) that
// ends once GitHub reports the pull request merged at Head; an arm or entry that disappears
// before then is submitted again.
type Armed struct {
	Number int
	Repo   ghrepo.Repository
	Head   string
}

func (a *Armed) Error() string {
	return fmt.Sprintf("pull request #%d of %s is armed for auto-merge or in its merge queue at READY's head %s: the daemon waits for GitHub to merge it",
		a.Number, a.Repo, a.Head)
}

func (a *Armed) Is(target error) bool { return target == wait.ErrWaiting }

// Routine is always true: only the repository's own rules hold an armed pull request back.
func (a *Armed) Routine() bool { return true }

// submission is the mutation Submit sends for a pull request, what it asks GitHub to do, and
// whether GitHub's acceptance of it merges the pull request now.
type submission struct {
	mutation, action string
	merges           bool
}

// choose is the submission GitHub's answer calls for: an enqueue on a base that requires a merge
// queue, a merge of a pull request mergeable now, an arm in a repository that allows auto-merge,
// and none otherwise.
func choose(answer repository) (submission, bool) {
	switch {
	case answer.PullRequest.IsMergeQueueEnabled:
		return submission{enable, "enqueue", false}, true
	case mergeableNow(answer.PullRequest.MergeStateStatus):
		return submission{merge, "merge", true}, true
	case answer.AutoMergeAllowed:
		return submission{enable, "enable auto-merge for", false}, true
	default:
		return submission{}, false
	}
}

// mergeableNow is GitHub's merge states in which the pull request merges without waiting, the
// states gh pr merge --auto merges at once instead of arming.
func mergeableNow(state string) bool {
	switch state {
	case "CLEAN", "HAS_HOOKS", "UNSTABLE":
		return true
	default:
		return false
	}
}

// repository is what Disarm and Submit read of a repository and its pull request.
type repository struct {
	AutoMergeAllowed bool         `json:"autoMergeAllowed"`
	PullRequest      *pullRequest `json:"pullRequest"`
}

// pullRequest is what Disarm and Submit read of a pull request: its node id, state, head, merge
// state, GitHub's last change to it, whether its base requires a merge queue, and whether auto-merge
// is enabled and it sits in a merge queue.
type pullRequest struct {
	ID                  string    `json:"id"`
	State               string    `json:"state"`
	HeadRefOid          string    `json:"headRefOid"`
	MergeStateStatus    string    `json:"mergeStateStatus"`
	UpdatedAt           time.Time `json:"updatedAt"`
	IsMergeQueueEnabled bool      `json:"isMergeQueueEnabled"`
	AutoMergeRequest    *struct{} `json:"autoMergeRequest"`
	MergeQueueEntry     *struct{} `json:"mergeQueueEntry"`
}

func (p *pullRequest) armed() bool { return p.AutoMergeRequest != nil || p.MergeQueueEntry != nil }

// kept says whether the pull request's head is head, the READY head an arm is legitimate for.
func (p *pullRequest) kept(head string) bool { return head != "" && p.HeadRefOid == head }

// openAt says whether the pull request is open at head.
func (p *pullRequest) openAt(head string) bool { return p.State == "OPEN" && p.HeadRefOid == head }

// readPull reads pull request number of repo, nil when GitHub answers NOT_FOUND at the pull
// request itself.
func readPull(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, number int) (*pullRequest, error) {
	answer, err := readRepository(ctx, github, repo, number)
	return answer.PullRequest, err
}

// readRepository reads repo and pull request number, with a nil pull request when GitHub answers
// NOT_FOUND at the pull request itself.
func readRepository(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, number int) (repository, error) {
	var answer struct {
		Repository repository `json:"repository"`
	}
	err := github.GraphQL(ctx, read, map[string]any{"owner": repo.Owner(), "name": repo.Name(), "number": number}, &answer)
	var failed *githubrest.GraphQLError
	if errors.As(err, &failed) && failed.NotFoundAt("repository", "pullRequest") {
		return repository{}, nil
	}
	if err != nil {
		return repository{}, fmt.Errorf("read the merge state of pull request #%d of %s: %w", number, repo, err)
	}
	return answer.Repository, nil
}
