---
title: "A required check runs main's post-merge command, and that command writes nothing in the checkout"
category: best-practices
tags:
  - ci
  - required-checks
  - jj
  - release-push
  - shell
date: 2026-10-28
status: active
module: .github/workflows
related_issues:
  - "sjawhar/legion#2423"
---

# A required check runs main's post-merge command, and that command writes nothing in the checkout

Extends docs/solutions/legion/a-red-lane-outside-the-required-set-merges-unread-read-every-lane-that-builds-the-changed-package-INFRA-379.md.

## The rule

1. When a job on main (a push, rebuild or release job) refuses some input after the merge, run
   the same command in a required job (`lint`, `typecheck`, `test` or `pr-title`). A check in a
   non-required job gates nothing: the pull request merges, and every run on main then fails or
   drops a file.
2. Move the credentialed job's input checks into a subcommand that holds no credential, and call
   it from both jobs, so the two lists cannot drift. Keep `bun` (a package script) in command
   position of the step, so `check-release-credentials.sh` still classifies the job as running
   dependency code.
3. That shared command, and any command a worker runs to reproduce a red check, writes nothing in
   the checkout: it builds into `mktemp -d` outside the repository and refuses an output path
   inside it. jj snapshots every write into `@`, the commit the worker pushes, and modified
   tracked files make an operator's next `git pull` refuse to fast-forward. Only a disposable CI
   checkout extracts the output into the tree, with one named subcommand.
4. Find the repository root from the script's own path, not `git rev-parse --show-toplevel`,
   which fails in a jj workspace with no colocated git.

## Evidence

- sjawhar/legion#2423, review round 2 (`pr-and-main.yaml`): the required `test` job ran three of
  the rebuild's checks, so an executable skill file passed every required check and would have
  failed every rebuild on main. 7e41852e added `release-push.sh check-built` (push-built's member
  checks) and one `bun run verify-build <tar>` step for the rebuild's build job, the required
  `test` job and the `claude-envoy` job.
- Review round 3: that command built in place and left rebuilt files in `@`; 79c191d1 builds in a
  scratch directory, refuses a tar inside the repository, and leaves extraction to
  `release-push.sh extract-built` in the CI jobs alone (4360b7a2).
- `packages/claude-envoy/scripts/build.ts` `outRefusal` compares real paths against the package's
  own `../..`.
