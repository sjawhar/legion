package daemon

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// defaultBranchTTL is how long a repository's default branch, as GitHub named it, stands before the
// next launch, provisioning or branch creation reads it again: a repository whose default branch is
// renamed is followed within it.
const defaultBranchTTL = 10 * time.Minute

// defaultBranches is each repository's default branch as GitHub names it (ghbranch.DefaultBranch),
// read as the implement App for the repository's owner — the App that creates an issue's branch
// and provisions its workspace — and kept per repository for ttl from the read. Reads of one
// repository that overlap share one request to GitHub; a read GitHub refuses is returned and not
// kept.
type defaultBranches struct {
	tokens    appauth.Tokens
	githubAPI string
	ttl       time.Duration
	now       func() time.Time

	mu      sync.Mutex
	read    map[ghrepo.Repository]defaultBranchRead
	pending map[ghrepo.Repository]*pendingDefaultBranch
}

type defaultBranchRead struct {
	branch string
	at     time.Time
}

type pendingDefaultBranch struct {
	done   chan struct{}
	branch string
	err    error
}

// newDefaultBranches reads under githubAPI, the GitHub REST root ("" is https://api.github.com),
// with tokens' implement App, keeping each answer for ttl on now's clock.
func newDefaultBranches(tokens appauth.Tokens, githubAPI string, ttl time.Duration, now func() time.Time) *defaultBranches {
	return &defaultBranches{
		tokens: tokens, githubAPI: githubAPI, ttl: ttl, now: now,
		read: map[ghrepo.Repository]defaultBranchRead{}, pending: map[ghrepo.Repository]*pendingDefaultBranch{},
	}
}

// Of is repo's default branch: the one read within ttl, or GitHub's answer now.
func (d *defaultBranches) Of(ctx context.Context, repo ghrepo.Repository) (string, error) {
	d.mu.Lock()
	if kept, ok := d.read[repo]; ok && d.now().Before(kept.at.Add(d.ttl)) {
		d.mu.Unlock()
		return kept.branch, nil
	}
	if waiting, ok := d.pending[repo]; ok {
		d.mu.Unlock()
		select {
		case <-waiting.done:
			return waiting.branch, waiting.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	waiting := &pendingDefaultBranch{done: make(chan struct{})}
	d.pending[repo] = waiting
	d.mu.Unlock()

	branch, err := d.fetch(ctx, repo)

	d.mu.Lock()
	if err == nil {
		d.read[repo] = defaultBranchRead{branch: branch, at: d.now()}
	}
	waiting.branch, waiting.err = branch, err
	delete(d.pending, repo)
	close(waiting.done)
	d.mu.Unlock()
	return branch, err
}

func (d *defaultBranches) fetch(ctx context.Context, repo ghrepo.Repository) (string, error) {
	lease, err := d.tokens.Token(ctx, appauth.Implement, repo.Owner())
	if err != nil {
		return "", fmt.Errorf("mint the implement App token to read the default branch of %s: %w", repo, err)
	}
	return ghbranch.DefaultBranch(ctx, githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(d.githubAPI, repo)}, repo)
}
