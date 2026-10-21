package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/ghbranch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// defaultBranchTTL is how long a repository's default branch, as GitHub named it, stands before the
// next read of it asks GitHub again: an issue recorded after a repository's default branch is
// renamed takes the new name within it. An issue already recorded keeps the default branch it was
// recorded with (record.Issue.DefaultBranch), so a rename mid-issue is not followed.
const defaultBranchTTL = 10 * time.Minute

// defaultBranches is each repository as GitHub names it (ghbranch.Read: its own spelling of owner
// and name, and its default branch), read as the implement App for the repository's owner — the
// App that creates an issue's branch and provisions its workspace — and kept per repository, in
// any spelling, for ttl from the read. Reads of one repository that overlap share one request to
// GitHub; a read GitHub refuses is returned and not kept.
type defaultBranches struct {
	tokens    appauth.Tokens
	githubAPI string
	ttl       time.Duration
	now       func() time.Time

	mu      sync.Mutex
	read    map[string]defaultBranchRead
	pending map[string]*pendingDefaultBranch
}

type defaultBranchRead struct {
	repository ghbranch.Repository
	at         time.Time
}

type pendingDefaultBranch struct {
	done       chan struct{}
	repository ghbranch.Repository
	err        error
}

// newDefaultBranches reads under githubAPI, the GitHub REST root ("" is https://api.github.com),
// with tokens' implement App, keeping each answer for ttl on now's clock.
func newDefaultBranches(tokens appauth.Tokens, githubAPI string, ttl time.Duration, now func() time.Time) *defaultBranches {
	return &defaultBranches{
		tokens: tokens, githubAPI: githubAPI, ttl: ttl, now: now,
		read: map[string]defaultBranchRead{}, pending: map[string]*pendingDefaultBranch{},
	}
}

// Named is repo as GitHub names it, with its default branch: the read made within ttl, or GitHub's
// answer now.
func (d *defaultBranches) Named(ctx context.Context, repo ghrepo.Repository) (ghbranch.Repository, error) {
	key := strings.ToLower(repo.String())
	d.mu.Lock()
	if kept, ok := d.read[key]; ok && d.now().Before(kept.at.Add(d.ttl)) {
		d.mu.Unlock()
		return kept.repository, nil
	}
	if waiting, ok := d.pending[key]; ok {
		d.mu.Unlock()
		select {
		case <-waiting.done:
			return waiting.repository, waiting.err
		case <-ctx.Done():
			return ghbranch.Repository{}, ctx.Err()
		}
	}
	waiting := &pendingDefaultBranch{done: make(chan struct{})}
	d.pending[key] = waiting
	d.mu.Unlock()

	repository, err := d.fetch(ctx, repo)

	d.mu.Lock()
	if err == nil {
		d.read[key] = defaultBranchRead{repository: repository, at: d.now()}
	}
	waiting.repository, waiting.err = repository, err
	delete(d.pending, key)
	close(waiting.done)
	d.mu.Unlock()
	return repository, err
}

// Kept is the last read of repo, in any spelling, however old, and asks GitHub nothing: what the
// workflow records an issue's repository and default branch from inside its transaction, which
// performs no I/O, once the read was made before it.
func (d *defaultBranches) Kept(repo string) (ghbranch.Repository, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	kept, ok := d.read[strings.ToLower(repo)]
	return kept.repository, ok
}

func (d *defaultBranches) fetch(ctx context.Context, repo ghrepo.Repository) (ghbranch.Repository, error) {
	lease, err := d.tokens.Token(ctx, appauth.Implement, repo.Owner())
	if err != nil {
		return ghbranch.Repository{}, fmt.Errorf("mint the implement App token to read the default branch of %s: %w", repo, err)
	}
	return ghbranch.Read(ctx, githubrest.Client{Token: lease.Token, API: githubrest.RepositoryAPI(d.githubAPI, repo)}, repo)
}
