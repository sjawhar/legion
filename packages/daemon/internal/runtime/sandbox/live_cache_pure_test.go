// The Stage 4a cache checks' pure logic. None of it touches a cluster, so, unlike live_test.go and
// live_cache_test.go, both `//go:build e2e`, this file carries no build tag: ordinary `go test ./...`
// compiles and exercises it directly (live_cache_unit_test.go).
package sandbox

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"time"
)

// cacheRunIssue is cache-b's issue key in the run whose project is project: S4A- and the FNV-1a
// hash of the project. cache-lost-volume pushes the issue's branch, legion/<issue>, onto the run's
// repository, which every Stage 4a run shares, so each run's key is its own, and a run only ever
// creates and deletes its own branch.
func cacheRunIssue(project string) string {
	sum := fnv.New32a()
	_, _ = sum.Write([]byte(project))
	return fmt.Sprintf("S4A-%d", sum.Sum32())
}

// staleRunBranchAge is longer than any Stage 4a run lives: the script stops its harness at
// `-test.timeout 150m`, and a run that long skips its teardown, so its cache-b branch stays.
const staleRunBranchAge = 4 * time.Hour

// cacheBranch is the shape of cache-b's branch, legion/<cacheRunIssue>.
var cacheBranch = regexp.MustCompile(`^legion/S4A-[0-9]+$`)

// runCommitMessage is the message of the commit cache-lost-volume pushes on branch.
func runCommitMessage(branch string) string { return "Stage 4a: " + branch + "'s own commit" }

// runBranch is a branch of the run's repository, its commit's message and when it was committed.
type runBranch struct {
	name, message string
	committed     time.Time
}

// staleRunBranches are the branches a killed run left: cache-b's shape, at a commit whose
// message is the harness's own, committed more than staleRunBranchAge before now. A run still in
// flight has a younger commit, and a branch anyone else wrote has another message.
func staleRunBranches(branches []runBranch, now time.Time) []string {
	var stale []string
	for _, b := range branches {
		if cacheBranch.MatchString(b.name) && b.message == runCommitMessage(b.name) && now.Sub(b.committed) > staleRunBranchAge {
			stale = append(stale, b.name)
		}
	}
	return stale
}
