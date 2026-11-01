---
title: "An empty check set is a conflict until proven otherwise: read mergeable before polling runs"
category: github
tags:
  - github-actions
  - pull_request
  - mergeable
  - ci-watcher
  - jj
  - merge-conflicts
date: 2026-10-20
status: active
module: .github/workflows, legion workers
related_issues:
  - "INFRA-42"
  - "sjawhar/legion#2086"
---

# An empty check set is a conflict until proven otherwise: read mergeable before polling runs

Extends `docs/solutions/github/conflicting-pr-gets-no-pull-request-ci.md`, which establishes that
GitHub queues no `pull_request`-triggered workflow for a `CONFLICTING` pull request. This is the
operational delta for anyone waiting on checks:

- **Zero runs is a diagnosis, not a delay.** A failing check is visible; a conflicting head is
  silent, and a poll loop over `actions/runs?head_sha=…` returns an empty list that looks exactly
  like a slow queue. Before waiting on anything, and immediately whenever a poll comes back empty
  twice:

  ```bash
  gh pr view <n> --json mergeable,headRefOid --jq '"head=\(.headRefOid) mergeable=\(.mergeable)"'
  ```

  `CONFLICTING` means no run will ever arrive for that head. The cure is the forward merge in
  `skill://legion-worker/references/conflicts-and-rewrites.md`, never a rebase.
- **A stale head reads the same way.** The same command tells you whether the SHA you are polling
  is still the pull request's head; a push by another role moves it, and runs accrue there instead.
- **Poll the SHA you pushed, read from jj rather than retyped.** `jj log -r 'legion/<KEY>' -T
  commit_id` gives the full SHA; a short id expanded by hand matches no run and polls forever.
- After the forward merge, expect the unchanged-diff fingerprint to move whenever the conflicted
  file is inside its fileset — see
  `docs/solutions/best-practices/a-word-level-merge-of-prose-is-verified-by-reading-the-paragraph-INFRA-42.md`.

## Evidence (INFRA-42, sjawhar/legion#2086)

Twice on one pull request. Both times a push produced no workflow run at all and the branch sat
waiting: `dd18d0145239` (the first head, which had never run CI, so the pull request was opened
with no checks) and `256f6fd7dc1f`, where a poll loop ran for nearly an hour over an empty run set
before `mergeable` was read and answered `CONFLICTING`. Each was cured by one forward merge of
`main` (`6af018be9bde`, `e270a523ecaa`), after which the full set ran green. The second conflict
was in documentation alone, which still moved the fingerprint (`7cbe0076…` → `a73e7599…`).
