---
title: "An equal fingerprint after a forward merge does not cover main's change to the code the real-surface test drives"
category: legion
tags:
  - fingerprint
  - forward-merge
  - e2e
  - rebase-re-check
date: 2026-10-27
status: active
module: skills/legion-worker
related_issues:
  - "INFRA-530"
  - "sjawhar/legion#2307"
---

# An equal fingerprint after a forward merge does not cover main's change to the code the real-surface test drives

Extends docs/solutions/legion/a-plan-that-expects-an-equal-fingerprint-runs-the-sibling-census-too-and-fetches-main-again-before-its-push-INFRA-391.md.

An equal fingerprint proves only that the branch's own lines did not change. It says nothing about
the code from main that runs under them. After a forward merge whose fingerprint is unchanged:

- **List what main changed between the two fork points** (from the old head's fork point to the
  new head's):
  `jj diff --from 'fork_point("main"@origin | <old head>)' --to 'fork_point("main"@origin | <new head>)' --name-only`.
- **Compare that list with the files your real-surface test drives.** These are the view, the
  editor and the server route the test loads, not only the files the pull request changes.
- **Where they overlap, re-run the real-surface test at the new head.** Record the overlap and the
  run in the handoff's `deviations`. Run it locally, because in sjawhar/legion the browser suite
  runs only in the `dispatch` job (`.github/workflows/envoy-and-contracts.yaml`,
  `bunx playwright test`). That job is not in main's required set, and READY does not wait for
  it. Where they do not overlap, run the bare gates only, as
  `skills/legion-worker/references/pr-body.md` says.

## Evidence

INFRA-530's fourth implementing round merged main 1b8d5c13 into #2307 in 412048cb. The
fingerprint stayed `10032dfc…`. Between the two fork points, main had changed proof-editor's
read-only directives and `packages/dispatch/web/src/features/doc/ProofDocument.tsx`. That code
is the past-version view that `email-links.e2e.ts` clicks. The implementer re-ran the browser test
at 412048cb (2 passed), and the tester re-ran it at c139dc07. At 12:04Z, after the reviewer had
approved, the `dispatch` job at c139dc07 was still `queued` (run 40488518222), so those two local
runs were the only evidence that the merged tree still opens a mail link.
