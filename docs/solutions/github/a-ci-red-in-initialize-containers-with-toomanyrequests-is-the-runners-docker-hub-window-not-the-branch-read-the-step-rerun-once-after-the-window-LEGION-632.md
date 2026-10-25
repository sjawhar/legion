---
title: "A CI red in Initialize containers with toomanyrequests is the runners' Docker Hub window, not the branch: read the step, re-run once after the window"
category: github
tags:
  - github-actions
  - docker-hub
  - rate-limit
  - testcontainers
  - mirror-gcr-io
  - ci-triage
date: 2026-10-10
status: active
module: .github/workflows
related_issues:
  - "LEGION-632"
  - "sjawhar/legion#1842"
---

# A CI red in Initialize containers with toomanyrequests is the runners' Docker Hub window, not the branch: read the step, re-run once after the window

- Before re-running a red job, read which step failed. A job red in `Initialize containers`
  (a `services:` container), in `docker/setup-buildx-action` (the `moby/buildkit` manifest), in a
  "Pull … image" step or in testcontainers' first pull, with `toomanyrequests: You have reached
  your unauthenticated pull rate limit`, never reached a lane: it is Docker Hub's anonymous limit on
  the GitHub-hosted runners' shared egress address, and the same red is on every other pull
  request's runs of that hour. Read `legion gh -- run view <id> --json jobs --jq '.jobs[] | …steps[]
  | select(.conclusion=="failure") | .name'` and `--log-failed | grep toomanyrequests` and name the
  cause in the PR body's `CI` line, so the reviewer and merger read the red for what it is.

  **Superseded 2026-10-23 (dispatch://INFRA-83):** only the clause that names the cause in the PR
  body's `CI` line is gone: the body carries no `CI:` line
  (`skills/legion-worker/references/pr-body.md`), and no role records a check result for a later
  step to trust — the tester and the reviewer each read CI on GitHub at the head they are judging
  and record none of it (the reviewer's own four GitHub reads are in
  `skills/legion-worker/references/merge-gate.md`), so the reviewer and the merger read the red for
  what it is by reading the failed step and its `toomanyrequests` line themselves at that head.
  Everything else stands: read which step failed before re-running, the two `legion gh` reads
  above, the one-re-run-per-interval cadence owned by one role below, and the mirror as the fix.
- Do not re-run on a loop. The limit is a rolling window per address and every failed pull counts
  against it, so each re-run inside the window extends the hold for everyone; the cadence that
  worked was one `legion gh -- run rerun <id> --failed` no earlier than an hour after the first
  429, then hourly, owned by one role so it happens once per interval. A role whose phase has ended
  holds no grant (`GRANT_EXPIRED`) and cannot re-run at all; say so rather than retrying.
- The fix is the mirror, not patience: `mirror.gcr.io/library/<name>` serves Docker Hub's official
  images at the same tags and digests with no anonymous limit (`AGENTS.md`, Tech Stack; main's
  #1871 moved every workflow, Dockerfile and test pull to it). A branch forked before that lands
  stays red until it forward-merges main — which GitHub requires anyway once the head conflicts —
  so a forward merge that brings the mirror is also what makes the branch's CI green.

## Evidence

#1842 (LEGION-632), 2026-10-09 21:01–21:33Z: the retro's docs commit cd8f3d20, the `.legion/`
removal 6cc0c109 and the harness fix bb12c138 each turned `Tests` (pgvector's pull in `Initialize
containers`), `Worker Image` (`debian:bookworm-slim` HEAD in buildx, then `moby/buildkit` in
`setup-buildx-action`), `Docs` and seven container jobs of `Legion Envoy and Contracts` red with
`toomanyrequests`; `lint`, `typecheck`, `pr-title` and the plugin test jobs passed each time, since
they pull nothing. Attempt 2 of `Tests` at 21:07Z failed the same way, and the architect stopped
further reruns (one at 22:30Z, then hourly). Main's #1871 landed at 22:48Z; the forward merge
3366e977 brought it, and `Tests` 38008011356 and `Docs` 38008011222 at the next head were green
through the mirror on the first run.
