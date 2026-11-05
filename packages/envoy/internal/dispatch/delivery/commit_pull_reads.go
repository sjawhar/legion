package delivery

import (
	"sync"
	"time"
)

// commit_pull_reads.go bounds how often one commit's pull requests are read through
// (ReadThroughCommitPulls). It is default_branch.go's twin: that file holds how long an answer
// about a repository and a commit's place on its default branch is believed, this one how long
// the answer "GitHub names no pull request of mine for this commit" stands before GitHub is
// asked again. Its one caller is the results read (api/results_currency.go,
// currencyReader.readThroughHead), which keeps a memo for the span of one request and no policy.

// CommitPullReadStaleAfter is how long one commit's read-through stands before another read may
// run for that commit. Each read costs one GET /repos/{owner}/{repo}/commits/{sha}/pulls, and
// one more per pull request GitHub names whose title, body, branch and links name nobody held to
// the repository, since that one's commit messages are paged too (attributeHead). The
// Verification tab refetches an issue's results on every result.recorded, so without a bound
// every refetch would spend those calls again for every result at a commit heading no pull
// request Dispatch knows — for ever, since nothing about such a commit is stored. Five minutes
// is the span every other answer about a commit is believed for
// (DefaultBranchCommitStaleAfter) and the one the reconcile re-reads an open head on
// (PullRequestHeadStaleAfter), so a pull request opened at a recorded commit is read as that
// result's current head within it.
const CommitPullReadStaleAfter = 5 * time.Minute

// CommitPullReads is when one commit of one repository was last read through, held across reads
// so a commit costs at most one read-through per CommitPullReadStaleAfter rather than one per
// read of the results. Its zero value is an empty cache, and the server holds one for the
// process (api.server.commitPulls), as it holds one DefaultBranches: the answer is a commit's,
// not a request's, and every goroutine reading results shares it.
//
// Only the read-time read-through records here, and the one a recorded result makes
// (api/results.go's readThroughPullRequests) must not: a result recorded while no pull request
// carries its commit would leave a fresh read behind, and the read a moment after the pull
// request opened would be refused a read-through of its own and answer `older` — which is the
// whole of what dispatch://INFRA-1240 repairs.
//
// Nothing evicts an entry: the map holds one moment per repository-and-commit pair a results
// read has asked GitHub about, which is at most one per recorded result's commit, and a restart
// empties it.
type CommitPullReads struct {
	mu    sync.Mutex
	reads map[commitPullKey]time.Time
}

// commitPullKey is the pair one read-through is spent on: a repository, under the canonical name
// its caller resolved (refs.CanonicalRepo), and a commit.
type commitPullKey struct {
	repo string
	sha  string
}

// ReadDue reports whether sha's pull requests in repoFull may be read through at now: nothing is
// held for the pair, or what is held was read CommitPullReadStaleAfter ago or longer.
//
// The caller records the read itself (RecordRead), so two reads that ask together both read: the
// bound is about one call per pair per span rather than exactly one, and one read of one issue's
// results asks about a commit once whatever this answers (currencyReader.headOf's memo).
func (c *CommitPullReads) ReadDue(repoFull, sha string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	last, found := c.reads[commitPullKey{repo: repoFull, sha: sha}]
	return !found || now.Sub(last) >= CommitPullReadStaleAfter
}

// RecordRead records that sha's pull requests in repoFull were read through at now. A read that
// failed is recorded with the rest: GitHub was asked, and a repository whose every ask fails
// must not be asked again on every read.
func (c *CommitPullReads) RecordRead(repoFull, sha string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reads == nil {
		c.reads = map[commitPullKey]time.Time{}
	}
	c.reads[commitPullKey{repo: repoFull, sha: sha}] = now
}
