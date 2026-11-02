---
title: "A term added to an expression another open pull request rewrites is guarded by a test file of its own"
category: legion
tags:
  - concurrent-pull-requests
  - merge-guard
  - regression-lock
  - handoff
date: 2026-11-01
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2620"
---

# A term added to an expression another open pull request rewrites is guarded by a test file of its own

- **Find out whether an open pull request rewrites the expression you are adding a term to**, and
  record the deciding observation in your handoff: the other pull request's state and `mergedAt`,
  and whether a file only it carries is in your tree. Both answers are facts a later reader can
  check; "it was open when I looked" is not.
- **Put your new tests in a new file, never in the test file the other pull request edits.** A
  term dropped by whoever resolves the merge then turns a test red in the merge-queue build. A
  comment on the other pull request is read by a person and checked by nobody, so it names that
  file rather than standing alone.
- **Write the other branch's instruction where it survives your retro.** The plan handoff is
  deleted from the head before READY, so an instruction that lives only in
  `.legion/<issue>/plan.json` is gone the moment the merge lands. Put it in the learning document,
  in the pull request body's `Not proven / risk`, or in a comment in the code it concerns.
- **Say in the body which branch you took**, so the second merger knows which half of the plan is
  live without reading a deleted file.

## Evidence

sjawhar/legion#2620 added a ServiceAccount term to `relaunch`'s `replace` expression in
`packages/daemon/internal/runtime/sandbox/relaunch.go`. sjawhar/legion#2538 (dispatch://INFRA-484)
rewrites that whole expression and touches `relaunch_test.go`, `one_container_test.go`,
`shared_pod_test.go`, `busy_test.go` and `watch_test.go`, so a term added to today's expression
could disappear in the merge with nothing failing.

The plan's decision D5 answered it structurally: the launch-side account tests went into a new
file, `packages/daemon/internal/runtime/sandbox/account_drift_test.go`, never into
`relaunch_test.go`. The implementer recorded the deciding observation — `gh pr view 2538 -R
sjawhar/legion --json state,mergedAt,mergeCommit` answering `OPEN` with a null `mergedAt`, and no
`drift.go` in the tree — and posted one comment on #2538 naming that file as what fails if the
term is dropped.

The instruction for the other branch is the part that nearly went missing. If #2538 merges first,
the account must join the round's compared settings by reading the Sandbox's own
`spec.podTemplate.spec.serviceAccountName` **directly**, outside the `legion.dev/pod-shape`
annotation, normalized `""` → `default` on both sides: a Sandbox stored before that annotation
existed carries none, and the round then falls back to comparing the image alone, which is exactly
the fleet a ServiceAccount migration has to move. The controller's pod is not round-covered, so
the term must also stay in `relaunch`'s own decision, outside the plugin-install guard. That was
written in `.legion/INFRA-934/plan.json`, which retro removes before READY, which is why it is
restated here.
