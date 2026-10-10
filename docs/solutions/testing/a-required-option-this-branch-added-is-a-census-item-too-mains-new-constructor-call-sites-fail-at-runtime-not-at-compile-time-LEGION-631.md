---
title: "A required option this branch added is a census item too: main's new constructor call sites fail at runtime, not at compile time"
category: testing
tags:
  - forward-merge
  - census
  - options-struct
  - sandbox.New
  - go
date: 2026-10-10
status: active
module: packages/daemon/internal/runtime/sandbox
related_issues:
  - "LEGION-631"
  - "sjawhar/legion#1843"
---
# A required option this branch added is a census item too: main's new constructor call sites fail at runtime, not at compile time

Extends docs/solutions/legion/a-deletion-branchs-forward-merge-runs-the-deletion-census-over-mains-new-files-and-looks-for-the-working-copys-orphan-LEGION-631.md.

- That note's census is of names the branch deleted. Add the names the branch made *required*: a
  field of an options struct a constructor now refuses nil for, a parameter a factory gained, an
  environment variable a process now demands. Code `main` wrote after the fork point omits them,
  and in Go an omitted struct-literal field is not a compile error: `go build` and `go vet` pass,
  the goldens diff clean, and the first `sandbox.New` in main's new test refuses at runtime.
- For each such name, grep main's new files for the constructor's call sites
  (`sandbox.New(`, `tmuxRuntime(`) and run every test file main added or changed that calls one,
  not only the packages the conflicts touched: the refusal lives in the test's first line of
  execution, where only a run reaches it.
- The negative control is the test as main wrote it, failing on the merged runtime with the
  refusal's own sentence: it proves the requirement is enforced and the fix is the test's, not the
  runtime's.

## Evidence

sjawhar/legion#1843, the fifth forward merge (38293495, main eb7c0e51 after #1842): main's new
`TestAChildClosedDoneReleasesItsVolumeWhileItsParentRuns` built `sandbox.New` with
`sandbox.Options{… Tokens: issueProvisionTokens{}, Conns: …}` and no `GitHubCredential`, which this
branch's `sandbox.New` refuses (`internal/runtime/sandbox/sandbox.go`: `case opts.GitHubCredential ==
nil: return refuse("no github credential function for the roles' gh files")`). Build, vet, the
regenerated goldens read against both parents, and the deletion census over main's 285 new files
were all clean; the full `go test ./...` on an embedded Postgres found it. The fix is the option its
sibling tests in the same file already pass (`internal/daemon/outbox_issue_suspend_test.go:311,458`,
`GitHubCredential: gitHubCredential(outboxTokens{}, "legion")`); the tester's round 8 kept the
unmodified test as its negative control.
