---
title: "Only main saves an Actions cache: restore and save steps gated on the ref, because cache-mode takes no expression"
category: github
tags:
  - ci
  - actions-cache
  - cache-mode
  - setup-go
  - setup-bun
  - buildkit
date: 2026-10-27
status: active
module: .github/workflows
related_issues:
  - "INFRA-584"
  - "sjawhar/legion#2328"
---

# Only main saves an Actions cache: restore and save steps gated on the ref, because cache-mode takes no expression

An Actions cache entry written under `refs/pull/<n>/merge` can be read by no other pull request, and it pushes main's entries out of the repository's cache. Write the rule "every run restores, only main saves" this way:

1. Never put a `${{ }}` expression in a workflow-level or job-level `cache-mode`. GitHub refuses the whole file as an invalid workflow (`Unrecognized named-value: 'github'`; `needs` and `vars` are refused too). Use `cache-mode` only for a literal `none` or `read`.
2. Split each cache into `actions/cache/restore` (with an `id`) before the work and `actions/cache/save` after it. The save takes `key: ${{ steps.<id>.outputs.cache-primary-key }}`, the identical `path` block, and `if: github.ref == 'refs/heads/main' && steps.<id>.outputs.cache-hit != 'true'`.
3. Test the ref, not the event. A merge-queue run (`refs/heads/gh-readonly-queue/…`) and a `workflow_dispatch` on another branch also write entries that no other ref can read.
4. Set `restore-keys` only off main: `restore-keys: ${{ github.ref != 'refs/heads/main' && format('<family>-{0}-', runner.os) || '' }}`. A main run seeded by prefix saves a module cache that grows with every dependency bump, because `~/go/pkg/mod` is never pruned. A run on main restores by the exact key, so the entry it saves is clean.
5. For `actions/setup-go`, set `cache: false` and use the pair in rule 2. setup-go has no restore-only input. If you move its folders aside instead, its post step prints the warning `There are no cache folders on the disk` on every pull-request job. New key names mean pull requests restore nothing until main's first run writes them.
6. `oven-sh/setup-bun`'s only cache input is `no-cache`, and it also stops the restore. Passing `no-cache: ${{ github.ref != 'refs/heads/main' }}` makes a run off main download Bun (about 34 MB) instead of restoring it.
7. For `docker/build-push-action`, give each image its own scope and leave `cache-to` empty off main: `cache-from: type=gha,scope=<image>` and `cache-to: ${{ github.ref == 'refs/heads/main' && 'type=gha,mode=max,scope=<image>' || '' }}`. An empty input passes no `--cache-to`. Two images on the default scope replace each other's cache index.
8. Prove it with the caches API, not the logs alone: `gh api "repos/<owner>/<repo>/actions/caches?ref=refs/pull/<n>/merge" --jq .total_count` prints `0` after every workflow of the pull request has finished. A pull request without the change, run in the same hours, is the negative control.

## Evidence

- Probe runs on the throwaway branch `probe/infra-584-cache-mode`: 40445439022 (workflow-level `github.ref` expression), 40445440160 (job-level), 40445585659 (`needs`) and 40445586575 (`vars`). Each was refused as an invalid workflow file. Their annotations are on the run page's `annotations_partial`; `gh run view` only says `This run likely failed because of a workflow file issue`.
- sjawhar/legion#2328 at d317a9a5: after Tests 40463123255, Docs 40463123212, Envoy 40463123196 and Worker Image had finished, `refs/pull/2328/merge` held 0 entries. Each `actions/cache/save` step was skipped, including those in the jobs `envoy-infra-go` (job 122080969483) and `envoy-e2e-local` (job 122080969328). In the same hour, `refs/pull/2322/merge` and `refs/pull/2303/merge`, which lack the change, each wrote a 2.2 GB `go-notices-Linux-…` entry.
- `actions/setup-go` `src/cache-save.ts` at b7ad1dad31e06c5925ef5d2fc7ad053ef454303e: when every cache path is missing it calls `core.warning('There are no cache folders on the disk')`.
- The worker image build in run 40448807776 ran with `--cache-from type=gha,scope=worker-image` and no `--cache-to`. `Build and push` took 4.28 min, against a 12.90 min median over the 30 pull-request runs before the change. Before the change, a main build at 22:17Z on 2026-10-26 imported the shared default scope's newest index after the Envoy listener's build and got 0 `CACHED` steps.
- The restore-keys growth on main was found by the simplify pass at 9e8a9241, and fixed in d0c01b1b.
