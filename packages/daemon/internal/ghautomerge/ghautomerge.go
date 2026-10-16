// Package ghautomerge submits and disarms a pull request's merge through GitHub's GraphQL API: the
// way the daemon merges an accepted READY head under the repository's own rules, and makes sure a
// pull request armed for one READY head never merges another.
package ghautomerge

import (
	"context"
	"errors"
	"fmt"

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
      id state headRefOid mergeStateStatus isMergeQueueEnabled
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

// Submitted is what Submit found or asked GitHub to do.
type Submitted struct {
	// Missing is GitHub answering NOT_FOUND for the pull request itself: there is nothing to merge.
	Missing bool
	// Stale is a submission that no longer covers the pull request: it is closed, its head moved
	// past READY's, or the record no longer awaits that READY.
	Stale bool
	// Already is a pull request GitHub already merged, armed or queued.
	Already bool
	// AutoMerge is GitHub accepting auto-merge or a merge-queue entry for READY's head.
	AutoMerge bool
	// Merged is GitHub merging READY's head now.
	Merged bool
}

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
// reports the pull request mergeable. GitHub's own rules decide each answer; Submit bypasses none.
func Submit(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, number int, head string, keep Keep) (Submitted, error) {
	answer, err := readRepository(ctx, github, repo, number)
	if err != nil {
		return Submitted{}, err
	}
	pull := answer.PullRequest
	switch {
	case pull == nil:
		return Submitted{Missing: true}, nil
	case pull.State == "MERGED":
		return Submitted{Already: true}, nil
	case pull.State != "OPEN" || pull.HeadRefOid != head:
		return Submitted{Stale: true}, nil
	}
	kept, err := keep(ctx)
	if err != nil {
		return Submitted{}, err
	}
	if kept != head {
		return Submitted{Stale: true}, nil
	}
	if pull.armed() {
		return Submitted{Already: true}, nil
	}
	variables := map[string]any{"id": pull.ID, "head": head, "method": "SQUASH"}
	switch {
	case pull.IsMergeQueueEnabled:
		return submitted(ctx, github, enable, variables, Submitted{AutoMerge: true}, "enqueue", number, repo)
	case mergeableNow(pull.MergeStateStatus):
		return submitted(ctx, github, merge, variables, Submitted{Merged: true}, "merge", number, repo)
	case answer.AutoMergeAllowed:
		return submitted(ctx, github, enable, variables, Submitted{AutoMerge: true}, "enable auto-merge for", number, repo)
	default:
		return Submitted{}, wait.Errorf("pull request #%d of %s is %s at READY's head %s, and the repository allows no auto-merge: the daemon merges it once GitHub reports it mergeable",
			number, repo, pull.MergeStateStatus, head)
	}
}

// submitted sends mutation and reports done when GitHub accepts it.
func submitted(ctx context.Context, github githubrest.Client, mutation string, variables map[string]any, done Submitted, action string, number int, repo ghrepo.Repository) (Submitted, error) {
	if err := github.GraphQL(ctx, mutation, variables, nil); err != nil {
		return Submitted{}, fmt.Errorf("%s pull request #%d of %s at READY's head %s: %w", action, number, repo, variables["head"], err)
	}
	return done, nil
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
// state, whether its base requires a merge queue, and whether auto-merge is enabled and it sits
// in a merge queue.
type pullRequest struct {
	ID                  string    `json:"id"`
	State               string    `json:"state"`
	HeadRefOid          string    `json:"headRefOid"`
	MergeStateStatus    string    `json:"mergeStateStatus"`
	IsMergeQueueEnabled bool      `json:"isMergeQueueEnabled"`
	AutoMergeRequest    *struct{} `json:"autoMergeRequest"`
	MergeQueueEntry     *struct{} `json:"mergeQueueEntry"`
}

func (p *pullRequest) armed() bool { return p.AutoMergeRequest != nil || p.MergeQueueEntry != nil }

// kept says whether the pull request's head is head, the READY head an arm is legitimate for.
func (p *pullRequest) kept(head string) bool { return head != "" && p.HeadRefOid == head }

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
