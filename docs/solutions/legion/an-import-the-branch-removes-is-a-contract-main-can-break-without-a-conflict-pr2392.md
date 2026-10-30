---
title: "An import the branch removes is a contract main can break without a conflict"
category: legion
tags:
  - forward-merge
  - merge-ref
  - go
  - ci
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2392"
---

# An import the branch removes is a contract main can break without a conflict

Extends docs/solutions/legion/mains-test-fixture-change-is-a-conflict-mergeable-does-not-see.md.

- In Go, a branch that removes an import from a file has also removed that package from every
  line main later adds to the same file. Main's new line and the branch's removed import touch
  different hunks, so git merges them with no conflict and GitHub reports the pull request
  `MERGEABLE`, but the merged tree does not compile.
- While such a pull request waits for its merge, build the tree merged with current main
  (`jj new <branch> main@origin`, then `go build ./...` in the module), not only the branch head.
  Do it before each READY, since main can add the use at any time.
- Expect the red in whichever lane builds the binary first, not only in the Go lane. Here the
  daemon's `test` job failed first, because its scratch-Dispatch tests run
  `go build ./cmd/dispatch` from the merge ref.
- The fix is a forward merge that restores the import. That changes the pull request's own diff in
  a runtime file, so it is a full round, not a confirmation.

## Evidence

sjawhar/legion#2392 removed `api/events.go`'s import of `internal/dispatch/store` when it deleted
`EventHead`. Main then added `store.CastableUUID(...)` to that file's asks query. CI's `test` job
at af78f59b failed in `TestBlockedIssuesAreNotStartedAgainstScratchDispatch` and four other tests
with `internal/dispatch/api/events.go:368:39: undefined: store`, while GitHub still reported the
pull request mergeable. Forward merge 2127dde7 restored the import, and renumbered the migration
to 0149 because main had also taken 0148.
