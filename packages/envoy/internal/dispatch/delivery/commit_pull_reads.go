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
// One decision, one lock: Claim tests the bound and records the read together, so no caller can
// hold the two apart and two reads that ask at once cannot both be granted. The map holds one
// moment per repository-and-commit pair asked about within the last CommitPullReadStaleAfter,
// since a granted claim drops every older entry once the map has passed commitPullReadSweepAt.
// That sweep walks the whole map under the lock, and it runs only on a claim that is granting a
// GitHub call, so what waits for it is already waiting for a round trip.
//
// Beside those moments it holds the pairs GitHub has answered for good (Settle), which no claim
// may spend a call on again and which the sweep must not drop: a read that named a pull request
// whose head is another commit has had GitHub's whole answer about this commit, and a later read
// would buy the same answer again. What bounds them is the results Dispatch holds — one entry per
// repository and commit a recorded result named whose pull request has moved past it — and a
// restart empties the lot, which costs one read-through of each such commit again and no more.
type CommitPullReads struct {
	mu      sync.Mutex
	reads   map[commitPullKey]time.Time
	settled map[commitPullKey]struct{}
}

// commitPullReadSweepAt is the size the map is swept from. An entry read CommitPullReadStaleAfter
// ago or longer refuses nothing and is overwritten by the next claim for its pair, so it is dead
// weight; below this size the weight is not worth a walk, and above it a process that reads the
// results of many issues for weeks would otherwise hold one entry per commit it ever asked about.
const commitPullReadSweepAt = 1024

// commitPullKey is the pair one read-through is spent on: a repository, under the canonical name
// its caller resolved (refs.CanonicalRepo), and a commit.
type commitPullKey struct {
	repo string
	sha  string
}

// Claim reports whether sha's pull requests in repoFull may be read through at now — the pair is
// not settled, and either nothing is held for it or what is held was read CommitPullReadStaleAfter
// ago or longer — and records the read when it answers yes, under the one lock that read the
// bound.
//
// A read that fails is claimed with the rest: GitHub was asked, and a repository whose every ask
// fails must not be asked again on every read. One read of one issue's results asks about a
// commit once whatever this answers (api/results_currency.go, currencyReader.headOf's memo).
func (c *CommitPullReads) Claim(repoFull, sha string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := commitPullKey{repo: repoFull, sha: sha}
	if _, settled := c.settled[key]; settled {
		return false
	}
	if last, found := c.reads[key]; found && now.Sub(last) < CommitPullReadStaleAfter {
		return false
	}
	if c.reads == nil {
		c.reads = map[commitPullKey]time.Time{}
	} else if len(c.reads) >= commitPullReadSweepAt {
		for key, last := range c.reads {
			if now.Sub(last) >= CommitPullReadStaleAfter {
				delete(c.reads, key)
			}
		}
	}
	c.reads[key] = now
	return true
}

// Settle ends the asking about one repository and commit: GitHub has been read for the pair and
// named a pull request whose head is another commit, so this commit is behind that pull request
// and nothing GitHub says later can make a result at it current (api/results_currency.go,
// currencyReader.readThroughHead). Without it the pair would be claimed again every
// CommitPullReadStaleAfter for as long as the result is read, since nothing is stored at a commit
// no pull request heads.
//
// It carries no moment, because the answer does not go stale: a pull request's head never moves
// back to a commit it has left. A settled pair the sweep kept is held until the process restarts.
func (c *CommitPullReads) Settle(repoFull, sha string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.settled == nil {
		c.settled = map[commitPullKey]struct{}{}
	}
	c.settled[commitPullKey{repo: repoFull, sha: sha}] = struct{}{}
}
