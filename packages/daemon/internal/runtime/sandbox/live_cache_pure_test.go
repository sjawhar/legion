// The Stage 4a cache checks' pure logic. None of it touches a cluster, so, unlike live_test.go and
// live_cache_test.go, both `//go:build e2e`, this file carries no build tag: ordinary `go test ./...`
// compiles and exercises it directly (live_cache_unit_test.go).
package sandbox

import (
	"fmt"
	"hash/fnv"
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
