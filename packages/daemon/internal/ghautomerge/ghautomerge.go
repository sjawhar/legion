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

// Disarm turns off what would merge pull request number of repo without a person, through github,
// a GraphQL client (githubrest.GraphQLAPI): its merge-queue entry where it has one, then its
// auto-merge where GitHub has it enabled. The queue entry goes first, being the step closest to
// merging, so a disable that keeps failing never leaves it standing. It decides by the pull
// request's state when it runs, never by when anything was armed: GitHub enqueues an armed pull
// request only once its checks pass, so a queue entry can postdate the arm it belongs to. keep is
// the READY head an arm is legitimate for, empty when none is: while the pull request's head on
// GitHub is keep, nothing is touched (Kept). Otherwise everything armed is turned off. It reads
// first, so a pull request with nothing armed is left alone and a retried Disarm changes nothing. A
// NOT_FOUND at the pull request is Missing, not an error; one anywhere else, such as a repository
// the token cannot see, is an error.
func Disarm(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, number int, keep string) (Disarmed, error) {
	var answer struct {
		Repository struct {
			PullRequest *struct {
				ID               string    `json:"id"`
				HeadRefOid       string    `json:"headRefOid"`
				AutoMergeRequest *struct{} `json:"autoMergeRequest"`
				MergeQueueEntry  *struct{} `json:"mergeQueueEntry"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	err := github.GraphQL(ctx, read, map[string]any{"owner": repo.Owner(), "name": repo.Name(), "number": number}, &answer)
	var failed *githubrest.GraphQLError
	if errors.As(err, &failed) && failed.NotFoundAt("repository", "pullRequest") {
		return Disarmed{Missing: true}, nil
	}
	if err != nil {
		return Disarmed{}, fmt.Errorf("read whether pull request #%d of %s is armed: %w", number, repo, err)
	}
	pull := answer.Repository.PullRequest
	if pull == nil {
		return Disarmed{Missing: true}, nil
	}
	if keep != "" && pull.HeadRefOid == keep {
		return Disarmed{Kept: true}, nil
	}
	var disarmed Disarmed
	if pull.MergeQueueEntry != nil {
		if err := github.GraphQL(ctx, dequeue, map[string]any{"id": pull.ID}, nil); err != nil {
			return disarmed, fmt.Errorf("take pull request #%d of %s out of the merge queue: %w", number, repo, err)
		}
		disarmed.Queue = true
	}
	if pull.AutoMergeRequest != nil {
		if err := github.GraphQL(ctx, disable, map[string]any{"id": pull.ID}, nil); err != nil {
			return disarmed, fmt.Errorf("disable the auto-merge of pull request #%d of %s: %w", number, repo, err)
		}
		disarmed.AutoMerge = true
	}
	return disarmed, nil
}
