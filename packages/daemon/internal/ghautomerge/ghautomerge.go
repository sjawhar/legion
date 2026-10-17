// Package ghautomerge submits and disarms a pull request's merge through GitHub's GraphQL API: the
// way the daemon merges an accepted READY head under the repository's own rules, and makes sure a
// pull request armed for one READY head never merges another.
package ghautomerge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/classify"
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
// base that requires a merge queue gets the pull request enqueued once GitHub reports it mergeable
// now, a pull request GitHub reports mergeable now is merged, and a repository that allows
// auto-merge gets it armed; every mutation carries head as GitHub's expected head. A base with a
// merge queue waits until GitHub reports the pull request mergeable (Unmergeable), a required check
// still running among what holds it back: there GitHub's enable arms auto-merge on a pull request
// it does not report mergeable rather than enqueueing it, and a required check may judge the arm,
// so Submit never arms one. A repository that allows none of those waits the same way. GitHub's
// own rules decide each answer; Submit bypasses none.
//
// Only GitHub merging the pull request at head ends a submission, or the record leaving that READY
// (Stale). An arm or a merge-queue entry at head is a wait (Armed), not the end. armed says the row
// has seen one at head before this run: an open pull request at head with nothing armed after that
// is a removal someone made, which Submit reads from GitHub's timeline (removal) and never answers
// with a second submission, which for a merge the queue failed would queue the same failure again
// and rebuild every entry behind it. The merge queue taking the pull request out because its merge
// failed is MergeFailed, and any other removal, a person's dequeue or turning auto-merge off, is
// Removed; only a removal that restarts the submission submits again: the submitting App's own, the
// daemon's disarm landing late, and auto-merge a close turned off, undone by the reopen at head.
// While the record still awaits head, a pull request GitHub shows closed, merged at another head, or
// at another head is a wait too (Lagging): the webhook that moves the record has not been applied
// yet, or GitHub's read is the one behind, and the next run decides again. GitHub always answers
// updatedAt, so a read without it fails rather than leaving the wait unbounded.
//
// A mutation GitHub refuses is read again: when GitHub still stands where it refused it (the same
// open pull request at head, nothing armed, the same submission chosen), a retry would meet the
// same refusal, and Submit returns it as *Refused. When that read fails, its error is returned,
// the refusal as its context, so a rate limit it meets reaches the caller (githubrest.RetryAfter).
// Any other failure, an HTTP or network one included, is returned as it is, for the row to retry.
func Submit(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, number int, head string, armed bool, keep Keep) (Outcome, error) {
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
	case armed:
		if err := removal(ctx, github, repo, number, head); err != nil {
			return "", err
		}
	}
	chosen, ok := choose(answer)
	if !ok {
		return "", &Unmergeable{Number: number, Repo: repo, Head: head, State: pull.MergeStateStatus, Queue: pull.IsMergeQueueEnabled}
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
// mergeable yet, on a base that requires a merge queue (Queue), where enabling auto-merge now would
// arm it rather than enqueue it, or in a repository that allows no auto-merge: Submit enqueues or
// merges it once GitHub reports it mergeable. It is a wait (wait.ErrWaiting).
type Unmergeable struct {
	Number      int
	Repo        ghrepo.Repository
	Head, State string
	Queue       bool
}

func (u *Unmergeable) Error() string {
	if u.Queue {
		return fmt.Sprintf("pull request #%d of %s is %s at READY's head %s, and its base requires a merge queue, where enabling auto-merge before GitHub reports it mergeable arms it instead of enqueueing it: the daemon enqueues it once GitHub reports it mergeable",
			u.Number, u.Repo, u.State, u.Head)
	}
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
// before then is a removal (MergeFailed, Removed), never submitted again unless the submitting App
// made it.
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

// MergeFailed is the merge queue taking pull request Number out at READY's Head because its merge
// failed: GitHub's removal for Reason failed_checks, or for a reason GitHub gives no other meaning
// with a run that failed on Commit, the commit the queue built for it (its merge_group runs ran on
// gh-readonly-queue/<base>/pr-<n>-…, a branch GitHub deletes with the entry). Runs names each check
// that failed there, with its run's URL; Event is GitHub's id for the removal, and At when it was
// made. The submission is over: the issue goes back to implementing on it.
type MergeFailed struct {
	Number               int
	Head, Reason, Commit string
	Runs                 []string
	Event                string
	At                   time.Time
}

func (f *MergeFailed) Error() string {
	text := fmt.Sprintf("the merge queue took pull request #%d out at READY's head %s at %s (%s)", f.Number, f.Head, f.At.UTC().Format(time.RFC3339), f.Reason)
	switch {
	case len(f.Runs) > 0:
		return text + fmt.Sprintf(": its merge_group run on %s, the commit the queue built for it, failed: %s", f.Commit, strings.Join(f.Runs, "; "))
	case f.Commit != "":
		return text + fmt.Sprintf(": GitHub reported no failing run on %s, the commit the queue built for it", f.Commit)
	default:
		return text + ": GitHub named no commit the queue built for it"
	}
}

// Removed is someone other than the submitting App taking pull request Number out of the merge
// queue (Queue), or turning its auto-merge off, at READY's Head without a merge, and not because its
// merge failed (MergeFailed): Actor, GitHub's Reason (for auto-merge, its reason code and Detail,
// its sentence), and when (At). The submission is over: the pull request is the remover's, and the
// row ends rather than undo the removal.
type Removed struct {
	Number                      int
	Head, Actor, Reason, Detail string
	Queue                       bool
	At                          time.Time
}

func (r *Removed) Error() string {
	actor := r.Actor
	if actor == "" {
		actor = "someone"
	}
	at := r.At.UTC().Format(time.RFC3339)
	if r.Queue {
		return fmt.Sprintf("%s took pull request #%d out of the merge queue at READY's head %s at %s (%s) without merging it: the daemon submits it no further, and the issue stays in awaiting_merge",
			actor, r.Number, r.Head, at, r.Reason)
	}
	return fmt.Sprintf("%s turned off the auto-merge of pull request #%d at READY's head %s at %s (%s: %s) without merging it: the daemon submits it no further, and the issue stays in awaiting_merge",
		actor, r.Number, r.Head, at, r.Reason, r.Detail)
}

// Disappeared is pull request Number of Repo open at READY's Head with nothing armed after the row
// saw it armed or queued there, while GitHub's timeline names no removal since that ends the
// submission: none at all yet, GitHub's read of the pull request ahead of its timeline, or the merge
// queue's removal for the merge itself (Merging), which GitHub has yet to show as merged. It is a
// wait (wait.ErrWaiting); only the merge's is routine, and a removal the timeline never explains
// waits on someone.
type Disappeared struct {
	Number  int
	Repo    ghrepo.Repository
	Head    string
	Merging bool
}

func (d *Disappeared) Error() string {
	if d.Merging {
		return fmt.Sprintf("the merge queue took pull request #%d of %s out at READY's head %s to merge it: the daemon waits for GitHub to show it merged",
			d.Number, d.Repo, d.Head)
	}
	return fmt.Sprintf("pull request #%d of %s is no longer armed or queued at READY's head %s, and its timeline names no removal since: the daemon waits for GitHub to record one",
		d.Number, d.Repo, d.Head)
}

func (d *Disappeared) Is(target error) bool { return target == wait.ErrWaiting }

// Routine says whether only GitHub's own merge holds the wait back.
func (d *Disappeared) Routine() bool { return d.Merging }

// submission is the mutation Submit sends for a pull request, what it asks GitHub to do, and
// whether GitHub's acceptance of it merges the pull request now.
type submission struct {
	mutation, action string
	merges           bool
}

// choose is the submission GitHub's answer calls for: an enqueue on a base that requires a merge
// queue once GitHub reports the pull request mergeable now, a merge of a pull request mergeable now
// anywhere else, an arm in a repository that allows auto-merge, and none otherwise. A merge-queue
// base gets no arm: GitHub's enable enqueues a pull request it reports mergeable now and arms
// auto-merge on any other.
func choose(answer repository) (submission, bool) {
	pull := answer.PullRequest
	switch {
	case pull.IsMergeQueueEnabled && mergeableNow(pull.MergeStateStatus):
		return submission{enable, "enqueue", false}, true
	case pull.IsMergeQueueEnabled:
		return submission{}, false
	case mergeableNow(pull.MergeStateStatus):
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

// checkPage is one page of a merge queue commit's checks as removals and moreChecks read it: whether
// GitHub lists more after it, and each check run, with its workflow run, and commit status.
const checkPage = `pageInfo { hasNextPage endCursor }
                  nodes {
                    __typename
                    ... on CheckRun { name status conclusion detailsUrl checkSuite { workflowRun { url } } }
                    ... on StatusContext { context state targetUrl }
                  }`

// removals is the read of what took a pull request's arm or merge-queue entry away: the GraphQL
// viewer, the account the token acts as, and the pull request's last arms and removals, oldest
// first, each queue removal with the commit the queue built for it and the first page of that
// commit's checks (moreChecks reads the rest).
const removals = `query PullRequestRemovals($owner: String!, $name: String!, $number: Int!) {
  viewer { login }
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      timelineItems(last: 20, itemTypes: [ADDED_TO_MERGE_QUEUE_EVENT, AUTO_MERGE_ENABLED_EVENT, REMOVED_FROM_MERGE_QUEUE_EVENT, AUTO_MERGE_DISABLED_EVENT]) {
        nodes {
          __typename
          ... on AddedToMergeQueueEvent { createdAt }
          ... on AutoMergeEnabledEvent { createdAt }
          ... on AutoMergeDisabledEvent { id createdAt reason reasonCode actor { login } }
          ... on RemovedFromMergeQueueEvent {
            id createdAt reason actor { login }
            beforeCommit {
              oid
              statusCheckRollup {
                contexts(first: 100) {
                  ` + checkPage + `
                }
              }
            }
          }
        }
      }
    }
  }
}`

// moreChecks is the read of a merge queue commit's checks after the page that ended at $after: a
// merge group can carry more than the 100 a page holds.
const moreChecks = `query MergeGroupContexts($owner: String!, $name: String!, $oid: GitObjectID!, $after: String!) {
  repository(owner: $owner, name: $name) {
    object(oid: $oid) {
      ... on Commit {
        oid
        statusCheckRollup {
          contexts(first: 100, after: $after) {
            ` + checkPage + `
          }
        }
      }
    }
  }
}`

// removal is what took pull request number of repo's arm or merge-queue entry away at READY's
// head, after Submit saw one there, as GitHub's timeline records it: the removals since the last
// arm or entry. The merge queue's removal for the merge itself is a wait for GitHub to show the
// merge (Disappeared, Merging), whatever else the timeline shows, and reads no check. A queue
// removal whose reason leaves it to the checks on the commit the queue built
// (timelineItem.judgedByChecks) is judged on every page of them (mergeGroupCommit.checks): one for
// failed_checks, or for an unexplained reason with a run that failed there (failingRuns), is
// MergeFailed. Two removals restart the submission, and removal answers nil when they are all there
// is, for Submit to submit again (timelineItem.restarts): the daemon's own disarm landing late, and
// auto-merge a close turned off, which the pull request's reopen at READY's head undid. Any other is
// Removed, the latest of them. A timeline that shows no removal since the last arm is behind the
// read of the pull request (Disappeared).
func removal(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, number int, head string) error {
	var answer struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
		Repository struct {
			PullRequest *struct {
				TimelineItems struct {
					Nodes []timelineItem `json:"nodes"`
				} `json:"timelineItems"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := github.GraphQL(ctx, removals, map[string]any{"owner": repo.Owner(), "name": repo.Name(), "number": number}, &answer); err != nil {
		return fmt.Errorf("read what took pull request #%d of %s out of the merge queue or off auto-merge: %w", number, repo, err)
	}
	if answer.Repository.PullRequest == nil {
		return fmt.Errorf("GitHub answered no timeline for pull request #%d of %s", number, repo)
	}
	items := answer.Repository.PullRequest.TimelineItems.Nodes
	since := 0
	for i, item := range items {
		if item.Type == "AddedToMergeQueueEvent" || item.Type == "AutoMergeEnabledEvent" {
			since = i + 1
		}
	}
	removed := items[since:]
	for _, item := range removed {
		if item.Type == "RemovedFromMergeQueueEvent" && item.Reason == "merged" {
			return &Disappeared{Number: number, Repo: repo, Head: head, Merging: true}
		}
	}
	self := strings.TrimSuffix(answer.Viewer.Login, "[bot]")
	var failed, other *timelineItem
	var runs []string
	for i := range removed {
		item := &removed[i]
		if item.judgedByChecks() {
			checks, err := item.BeforeCommit.checks(ctx, github, repo)
			if err != nil {
				return err
			}
			if failing := failingRuns(checks); item.Reason == "failed_checks" || len(failing) > 0 {
				failed, runs = item, failing
				continue
			}
		}
		if !item.restarts(self) {
			other = item
		}
	}
	switch {
	case failed != nil:
		commit := ""
		if failed.BeforeCommit != nil {
			commit = failed.BeforeCommit.Oid
		}
		return &MergeFailed{Number: number, Head: head, Reason: failed.Reason, Commit: commit,
			Runs: runs, Event: failed.ID, At: failed.CreatedAt}
	case other != nil:
		return &Removed{Number: number, Head: head, Actor: other.actor(), Reason: other.removalReason(), Detail: other.Reason,
			Queue: other.Type == "RemovedFromMergeQueueEvent", At: other.CreatedAt}
	case since == len(items):
		return &Disappeared{Number: number, Repo: repo, Head: head}
	default:
		return nil
	}
}

// judgedByChecks says whether a removal is a queue removal whose reason leaves it to the checks on
// the commit the queue built: failed_checks, whose failing runs the issue goes back to implementing
// on, and a reason GitHub gives no other meaning, a failed merge only with a run that failed there.
// The merge itself (merged), a person's dequeue (manual) and a conflict (merge_conflict) mean what
// they say, so none of their checks is read: a person's dequeue cancels the merge group's runs in
// flight, which is no failed merge.
func (t *timelineItem) judgedByChecks() bool {
	if t.Type != "RemovedFromMergeQueueEvent" {
		return false
	}
	switch t.Reason {
	case "merged", "manual", "merge_conflict":
		return false
	default:
		return true
	}
}

// restarts says whether a removal restarts the submission rather than ending it: auto-merge a close
// turned off (reason code closed), which the reopen at READY's head undid, and the daemon's own
// disarm, made by the account the token acts as (self, its login without [bot], as the timeline
// names a bot) with the reason a disarm leaves: auto-merge manually_disabled, or a manual dequeue.
// GitHub names the App that enabled auto-merge as the actor of its own disables (a merge method
// the repository blocks, a base switch), so the actor alone does not make a disable the daemon's.
func (t *timelineItem) restarts(self string) bool {
	if t.Type == "AutoMergeDisabledEvent" && t.ReasonCode == "closed" {
		return true
	}
	if self == "" || t.actor() != self {
		return false
	}
	return t.Type == "AutoMergeDisabledEvent" && t.ReasonCode == "manually_disabled" ||
		t.Type == "RemovedFromMergeQueueEvent" && t.Reason == "manual"
}

// timelineItem is one arm or removal on a pull request's timeline as removals reads it.
type timelineItem struct {
	Type       string    `json:"__typename"`
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"createdAt"`
	Reason     string    `json:"reason"`
	ReasonCode string    `json:"reasonCode"`
	Actor      *struct {
		Login string `json:"login"`
	} `json:"actor"`
	BeforeCommit *mergeGroupCommit `json:"beforeCommit"`
}

// actor is the login of who made the item, "" when GitHub names no one.
func (t *timelineItem) actor() string {
	if t.Actor == nil {
		return ""
	}
	return t.Actor.Login
}

// removalReason is GitHub's reason for a removal: a queue removal's reason, or the reason code of
// an auto-merge turned off.
func (t *timelineItem) removalReason() string {
	if t.Type == "AutoMergeDisabledEvent" {
		return t.ReasonCode
	}
	return t.Reason
}

// failingRuns is each of checks, every check on the commit the merge queue built for a queue
// removal (mergeGroupCommit.checks), that failed there, named with how it ended and its run: a check
// run that ended in a result Legion's rule for a check run counts as failed (classify.RunResult:
// failure, timed out, failed to start, action required, stale), and a commit status in failure or
// error. A cancelled run is no failure here: a person's dequeue cancels every run in flight.
func failingRuns(checks []checkContext) []string {
	var runs []string
	for _, check := range checks {
		switch check.Type {
		case "CheckRun":
			result := classify.RunResult(strings.ToLower(check.Status), strings.ToLower(check.Conclusion))
			if result == classify.Pending || result == classify.Success || result == classify.Cancelled {
				continue
			}
			url := check.DetailsURL
			if check.CheckSuite != nil && check.CheckSuite.WorkflowRun != nil && check.CheckSuite.WorkflowRun.URL != "" {
				url = check.CheckSuite.WorkflowRun.URL
			}
			runs = append(runs, fmt.Sprintf("%s (%s, %s)", check.Name, result, url))
		case "StatusContext":
			if check.State == "FAILURE" || check.State == "ERROR" {
				runs = append(runs, fmt.Sprintf("%s (%s, %s)", check.Context, strings.ToLower(check.State), check.TargetURL))
			}
		}
	}
	return runs
}

// mergeGroupCommit is the commit the merge queue built for a pull request it took out, and how its
// checks ended.
type mergeGroupCommit struct {
	Oid               string `json:"oid"`
	StatusCheckRollup *struct {
		Contexts checkContexts `json:"contexts"`
	} `json:"statusCheckRollup"`
}

// checkContexts is the checks GitHub listed on a merge queue commit, and whether it lists more.
type checkContexts struct {
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []checkContext `json:"nodes"`
}

// checks is every check GitHub lists on the commit: the first page, as the timeline's read carried
// it, then each later page, read one by one (moreChecks), so a judgment of them misses no failure
// past the first. A nil commit, or one with no checks, has none.
func (c *mergeGroupCommit) checks(ctx context.Context, github githubrest.Client, repo ghrepo.Repository) ([]checkContext, error) {
	if c == nil || c.StatusCheckRollup == nil {
		return nil, nil
	}
	checks, page := c.StatusCheckRollup.Contexts.Nodes, c.StatusCheckRollup.Contexts.PageInfo
	for page.HasNextPage {
		var answer struct {
			Repository struct {
				Object *mergeGroupCommit `json:"object"`
			} `json:"repository"`
		}
		if err := github.GraphQL(ctx, moreChecks, map[string]any{"owner": repo.Owner(), "name": repo.Name(), "oid": c.Oid, "after": page.EndCursor}, &answer); err != nil {
			return nil, fmt.Errorf("read the checks of merge queue commit %s after %s: %w", c.Oid, page.EndCursor, err)
		}
		next := answer.Repository.Object
		if next == nil || next.StatusCheckRollup == nil {
			return nil, fmt.Errorf("GitHub answered no further checks of merge queue commit %s after %s", c.Oid, page.EndCursor)
		}
		checks = append(checks, next.StatusCheckRollup.Contexts.Nodes...)
		page = next.StatusCheckRollup.Contexts.PageInfo
	}
	return checks, nil
}

// checkContext is one check run or commit status on a merge queue's commit.
type checkContext struct {
	Type       string `json:"__typename"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"detailsUrl"`
	CheckSuite *struct {
		WorkflowRun *struct {
			URL string `json:"url"`
		} `json:"workflowRun"`
	} `json:"checkSuite"`
	Context   string `json:"context"`
	State     string `json:"state"`
	TargetURL string `json:"targetUrl"`
}
