// Package ghautomerge disarms a pull request's merge through GitHub's GraphQL API: the way the
// daemon makes sure a pull request armed for one READY head never merges another.
package ghautomerge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// Disarmed is what Disarm found and turned off.
type Disarmed struct {
	// Missing is GitHub answering NOT_FOUND for the pull request: there is nothing to disarm.
	Missing bool
	// AutoMerge is an auto-merge Disarm disabled.
	AutoMerge bool
	// Queue is a merge-queue entry Disarm removed.
	Queue bool
}

const read = `query PullRequestAutoMerge($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) { id autoMergeRequest { enabledAt } mergeQueueEntry { enqueuedAt } }
  }
}`

const (
	disable = `mutation DisableAutoMerge($id: ID!) { disablePullRequestAutoMerge(input: {pullRequestId: $id}) { clientMutationId } }`
	dequeue = `mutation DequeuePullRequest($id: ID!) { dequeuePullRequest(input: {id: $id}) { clientMutationId } }`
)

// Disarm turns off what would merge pull request number of repo without a person, through github,
// a GraphQL client (githubrest.GraphQLAPI): its auto-merge where GitHub has it enabled, and its
// merge-queue entry where it has one. It reads both first, so a pull request with neither is left
// alone and a retried Disarm changes nothing. An auto-merge enabled, or a queue entry made, after
// since is left alone too: since is when the disarm was decided, and only a later READY armed it.
// A pull request GitHub answers NOT_FOUND for is Missing, not an error.
func Disarm(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, number int, since time.Time) (Disarmed, error) {
	var answer struct {
		Repository struct {
			PullRequest *struct {
				ID               string `json:"id"`
				AutoMergeRequest *struct {
					EnabledAt time.Time `json:"enabledAt"`
				} `json:"autoMergeRequest"`
				MergeQueueEntry *struct {
					EnqueuedAt time.Time `json:"enqueuedAt"`
				} `json:"mergeQueueEntry"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	err := github.GraphQL(ctx, read, map[string]any{"owner": repo.Owner(), "name": repo.Name(), "number": number}, &answer)
	var failed *githubrest.GraphQLError
	if errors.As(err, &failed) && failed.NotFound() {
		return Disarmed{Missing: true}, nil
	}
	if err != nil {
		return Disarmed{}, fmt.Errorf("read whether pull request #%d of %s is armed: %w", number, repo, err)
	}
	pull := answer.Repository.PullRequest
	if pull == nil {
		return Disarmed{Missing: true}, nil
	}
	var disarmed Disarmed
	if request := pull.AutoMergeRequest; request != nil && !request.EnabledAt.After(since) {
		if err := github.GraphQL(ctx, disable, map[string]any{"id": pull.ID}, nil); err != nil {
			return disarmed, fmt.Errorf("disable the auto-merge of pull request #%d of %s: %w", number, repo, err)
		}
		disarmed.AutoMerge = true
	}
	if entry := pull.MergeQueueEntry; entry != nil && !entry.EnqueuedAt.After(since) {
		if err := github.GraphQL(ctx, dequeue, map[string]any{"id": pull.ID}, nil); err != nil {
			return disarmed, fmt.Errorf("take pull request #%d of %s out of the merge queue: %w", number, repo, err)
		}
		disarmed.Queue = true
	}
	return disarmed, nil
}
