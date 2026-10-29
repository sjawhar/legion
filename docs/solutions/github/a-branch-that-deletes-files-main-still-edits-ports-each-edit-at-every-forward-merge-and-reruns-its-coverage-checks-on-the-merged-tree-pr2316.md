---
title: "A branch that deletes files main still edits ports each edit at every forward merge, and reruns its coverage checks on the merged tree"
category: github
tags:
  - forward-merge
  - conflicts
  - ci
  - workflow-migration
date: 2026-10-29
status: active
module: .github/workflows
related_issues:
  - "sjawhar/legion#2316"
---

# A branch that deletes files main still edits ports each edit at every forward merge, and reruns its coverage checks on the merged tree

- **Read the modify/delete conflict as an inventory, not a resolution.** Keeping the deletion
  resolves the conflict and loses main's change. Before you keep it, read main's diff of the
  deleted file since the fork point (`jj diff --from 'fork_point("main"@origin | <head>)' --to
  '"main"@origin' -- <file>`). Port every hunk into the file that replaced it: new jobs with their
  filter, gate and final-check entries, removed jobs with all of theirs, steps, service images,
  env and comments.
- **Map each hunk to its new home before you edit.** A filter or job can sit under another name or
  anchor in the replacement. A path main added to a filter may already be there through a shared
  anchor; say so instead of adding it twice. A value main left stale in its copy (an old image
  digest) takes the replacement's current one; say so on the pull request.
- **Rerun every coverage check on the merged tree, not only the merge's own files.** A
  conflict-free part of main can add an import or a package that a filter must now name. The
  merge reports no conflict there, and only the check that recomputes what each job reads finds
  it.
- **Merge the newest main when you push, and push as soon as the checks pass.** On a busy base,
  the window between a merge and the next conflict is shorter than a full CI run. Tell whoever
  sequences merges the new head at once.

## Evidence

sjawhar/legion#2316 deleted three workflows and merged main forward nine times (fbf47eef,
62380e30, f343a775, 44544200, bb1a700a, af395f91, 5e6cac9b, 0dc57a01, ae8b66d6).

- Modify/delete hunks ported, each named by its main pull request:
  - #2364: three Postgres 16 jobs added. #2476 later removed them.
  - #2331: the `envoy-session-stall` job.
  - #2323: the `dispatch-browser-multiprocess` job. Main's copy still named the Postgres 16 image.
  - #2278: pi-envoy's NATS service.
  - #2449: `-timeout 20m` on a step the check pins whole, so its pinned text changed too.
  - #2186: the worker image's new inputs.
- Without a conflict, `check-ci-filters.sh` named seven packages main had added and the filters
  lacked, among them `internal/dispatch/processes`, `internal/logtest`,
  `internal/dispatch/docs/crafted`, `internal/claudelaunch` and `internal/bus/bustest`.
  `check-image-trigger-paths.sh` named `.bun-version` and `packages/claude-envoy` (#2186).
