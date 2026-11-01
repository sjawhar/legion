---
title: "Deleting a published workspace package: the site list, the two meta-checks that prove it, and a census of the brand and the path"
category: github
tags:
  - deletion
  - ci-router
  - release
  - acceptance-criteria
  - bun-workspace
date: 2026-11-01
status: active
module: .github
related_issues:
  - "sjawhar/legion#2599"
  - "INFRA-429"
---

# Deleting a published workspace package: the site list, the two meta-checks that prove it, and a census of the brand and the path

The inverse of `docs/solutions/github/join-a-job-to-the-ci-router.md`: that document joins a job
to the router, this one takes a published package out of it. `skills/legion-worker/references/cleanup-deletion.md`
gives the deletion chain for a command; a published workspace package has more sites than that,
and two of them no check covers.

## Delete every site in one commit, then let the meta-checks fail if you missed one

- **The package tree**, and the operator scripts that only existed to install it. When you delete
  a wrapper script, read every statement it ran, not only the call you are removing: a wrapper is
  often the only place a prerequisite is done.
- **The root `package.json` `workspaces` entry**, `bun.lock` (regenerate with `bun install`, then
  prove `bun install --frozen-lockfile`), and **every Dockerfile `COPY` of its manifest**. These
  three move together or the image build breaks:
  `.github/scripts/check-bun-workspace-manifests.sh` requires every root workspace manifest to be
  copied before each frozen install, so it flags a *missing* `COPY`, and a surviving `COPY` of a
  deleted file fails `docker build` instead.
- **Five separate sites in `.github/workflows/ci.yaml`**: the filter's output in the `changes`
  job, the package's manifest inside the shared `*bun_install` anchor, the filter block itself,
  the job, and *two* entries in `ci-ok` — its `needs` list and its `SELECTED_BY` map.
- **Two separate sites in `.github/scripts/ci_job_reads.py`**: the `WORKSPACES` entry and the
  `gate(...)` block.
- **The release**: the build job, its `*_VERSION_ARGS` `env:` block, every publish step, the
  `publish` job's `needs` and any `needs.<job>…` reference inside a step's `if:` (a surviving
  reference to an absent `needs` context fails the expression), and the `on.push.paths` entries in
  every workflow that triggered on the package.
- **The changelog fold's package list**, and **the architecture component file**
  (`.dispatch/architecture/<id>.md`; components are declared by the file's presence and retired by
  its absence, so there is no index to edit).

**Then run the two meta-checks, because they are the proof that the list is closed, not your
memory:** `.github/scripts/check-ci-filters.sh` and `.github/scripts/ci-ok.test.sh`, beside
`check-bun-workspace-manifests.sh`, `check-image-trigger-paths.sh`, `check-zod-version.sh` and
`check-release-credentials.sh`.

## Two sites no check covers

- **Prose that counts.** A comment saying "the four build jobs" or "the seven packages" is false
  the moment you delete one, and nothing goes red. Grep the files you touched for the number words
  as well as the names.
- **A meta-test whose fixture *is* the thing you deleted.** Substitute a sibling that still
  exists, never an invented placeholder: a fixture naming a job the workflow does not have is a
  stale reference of its own, and a real neighbour keeps the test's premise true.

## The acceptance census greps the brand *and* the path, and is a table, not a count

A "no remaining references to X" criterion is proven by a **table of file → keep-category**,
diffed row for row against the grep's output, so "no row missing and none spare" is a comparison
rather than a judgement. A count is not that: the file set moves under you at every forward merge,
and a count cannot say which way.

**Run the census twice, once for the brand and once for the directory**, and give each its own
table. They do not find the same files: a package published as `@scope/<brand>-thing` can live at
`packages/<something-else>`, and a grep for the brand never sees the path. Scope both to the whole
tree; a path census narrowed to `.github` and the manifests is how recorded fixtures elsewhere go
unexamined.

## Evidence

sjawhar/legion#2599 deleted `packages/envoy-plugin`, published as
`@sjawhar/opencode-legion-envoy`. The nine coupled sites above are the ones the diff touched, and
`check-ci-filters.sh` (`every ci.yaml filter matches every file its job reads`) and
`ci-ok.test.sh` were green at the merge head.

- The wrapper: `scripts/sync-envoy-host.sh` ran
  `ssh "$host" 'mkdir -p ~/legion/default/packages/envoy'` before calling two sync scripts. The
  surviving `packages/envoy/deploy/scripts/sync-host.sh` only runs `rsync`, which creates the
  final path component alone, so the deletion moved that `mkdir` into it with the reason attached.
  The plan caught this; no check would have.
- The prose: `.github/scripts/check-release-credentials.sh` said "four build jobs" and
  `AGENTS.md` said "the seven packages"; both are counts the deletion falsified, and the reviewer
  found the first by reading, not by a failing check. `.github/scripts/check-zod-version.sh`'s
  comment named the deleted package's one external dependency as an example, and its first
  rewrite was still wrong in number agreement.
- The fixture: `.github/scripts/ci-ok.test.sh` used the deleted job as one of its four example
  jobs in sixteen places; the diff substitutes `envoy-client`, a job `ci.yaml` still has and
  `ci_job_reads.py` still gates.
- The two censuses: the criterion's grep was `git grep -l -i opencode` with six path exclusions,
  and the pull request body carried its keep-category table, re-diffed at each of four forward
  merges as the set moved 37 → 38 → 39 files. The *path* census was run only over `.github`, the
  root manifests and the Dockerfiles. Outside that scope
  `packages/daemon/internal/classify/settlement_replay_test.go` carries ten `{"envoy-plugin", …}`
  rows — a recorded replay of real CI runs, correctly kept, and matched by **no** `opencode` grep
  (`grep -ci opencode` on that file answers 0). Nothing in the change ever looked at them.
- The committed artefact: `packages/claude-envoy/dist` is rebuilt only by a workflow gated on
  `refs/heads/main`, so at every head of the pull request it still carried the deleted host. The
  source was proven instead by building the bundle at the head
  (`bun scripts/build.ts --out "$TMPDIR/…"`) and grepping the build, with the committed copy as a
  standing negative control, and the body named the workflow that clears it after the merge.
