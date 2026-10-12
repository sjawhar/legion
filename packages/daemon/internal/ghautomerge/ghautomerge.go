// Package ghautomerge disarms a pull request's merge through GitHub's GraphQL API: the way the
// daemon makes sure a pull request armed for one READY head never merges another.
package ghautomerge

import (
	"context"
	"errors"
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
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
    pullRequest(number: $number) { id headRefOid autoMergeRequest { enabledAt } mergeQueueEntry { enqueuedAt } }
  }
}`

const (
	disable = `mutation DisableAutoMerge($id: ID!) { disablePullRequestAutoMerge(input: {pullRequestId: $id}) { clientMutationId } }`
	dequeue = `mutation DequeuePullRequest($id: ID!) { dequeuePullRequest(input: {id: $id}) { clientMutationId } }`
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

// pullRequest is what Disarm reads of a pull request: its node id, its head, and whether
// auto-merge is enabled and it sits in a merge queue.
type pullRequest struct {
	ID               string    `json:"id"`
	HeadRefOid       string    `json:"headRefOid"`
	AutoMergeRequest *struct{} `json:"autoMergeRequest"`
	MergeQueueEntry  *struct{} `json:"mergeQueueEntry"`
}

func (p *pullRequest) armed() bool { return p.AutoMergeRequest != nil || p.MergeQueueEntry != nil }

// kept says whether the pull request's head is head, the READY head an arm is legitimate for.
func (p *pullRequest) kept(head string) bool { return head != "" && p.HeadRefOid == head }

// readPull reads pull request number of repo, nil when GitHub answers NOT_FOUND at the pull
// request itself.
func readPull(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, number int) (*pullRequest, error) {
	var answer struct {
		Repository struct {
			PullRequest *pullRequest `json:"pullRequest"`
		} `json:"repository"`
	}
	err := github.GraphQL(ctx, read, map[string]any{"owner": repo.Owner(), "name": repo.Name(), "number": number}, &answer)
	var failed *githubrest.GraphQLError
	if errors.As(err, &failed) && failed.NotFoundAt("repository", "pullRequest") {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read whether pull request #%d of %s is armed: %w", number, repo, err)
	}
	return answer.Repository.PullRequest, nil
}
