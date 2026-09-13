---
title: "A package no workflow names has no CI: diff the packages with tests against the workflows' working-directory lines, and expect the first run to fail on environment"
category: testing
tags:
  - ci
  - github-actions
  - pr-and-main
  - working-directory
  - coverage-gap
  - jj-identity
  - first-run
date: 2026-09-13
status: active
module: .github/workflows
related_issues:
  - "sjawhar/legion#1023"
---

# A package no workflow names has no CI: diff the packages with tests against the workflows' working-directory lines, and expect the first run to fail on environment

GitHub Actions runs what a workflow's steps name and nothing else. Every job in this repository
selects its package with `working-directory: packages/<name>`; a package with no such line in any
workflow has no lint, no typecheck, and no tests in CI, however many `*.test.ts` files it carries
and however green they are on a developer's box.

## The instance

`packages/workspace/src/workspace.test.ts` existed from `feat(legion): rebuild Legion on
oh-my-pi (#753)` on 2026-08-26. The `Tests` workflow (`.github/workflows/pr-and-main.yaml`) ran
`bun test`, `biome check`, and `tsc --noEmit` with `working-directory: packages/daemon` only, and
`bun test workspace.test` from `packages/daemon` reports `filters did not match any test files`.
Eighteen days and twelve tests later, #1023 was the first time CI ran the file — because the
planner checked (plan fact F8) rather than assumed, and the architect kept the fix in scope. Every
acceptance check the spec named would otherwise have guarded nothing after merge.

## The check

Two lists, one difference:

```sh
# packages that carry tests
for p in packages/*/; do
  n=$(find "$p" -name '*.test.ts' -not -path '*/node_modules/*' | wc -l)
  [ "$n" -gt 0 ] && echo "$p"
done | sort > /tmp/with-tests
# packages any workflow steps into
grep -hoE 'working-directory: packages/[a-z-]+' .github/workflows/*.y*ml \
  | sed 's#working-directory: ##; s#$#/#' | sort -u > /tmp/in-workflows
comm -23 /tmp/with-tests /tmp/in-workflows      # tests that CI never runs
```

At `main` after #1023 the difference is empty; `packages/workspace` was the only entry before.
Run it when adding a package, and read the named workflow to confirm the step is *test*, not
just *build* or *publish* (`release.yaml` names `pi-envoy` and `envoy-plugin` for publishing;
their tests live in `envoy-and-contracts.yaml`).

## What the first run costs

The first run of a suite that never had CI fails for **environment**, not code. `packages/
workspace`'s real-jj tests push to a scratch remote; the runner has no jj user config, and
`jj git push` refuses `Won't push commit … since it has no author and/or committer set`. The fix
was in the test rig (`JJ_USER`/`JJ_EMAIL` on every jj invocation), reproduced locally first by
running with an empty `JJ_CONFIG`. Budget one red run for a suite CI has never seen, and reproduce
the runner's environment — no user config, no ambient credentials, the pinned tool versions the
job installs — before the push rather than after.

## The wiring

Three steps, one per job, beside the daemon's:

```yaml
      - name: Lint workspace
        run: bunx biome check --error-on-warnings src/
        working-directory: packages/workspace
      - name: Typecheck workspace
        run: bunx tsc --noEmit
        working-directory: packages/workspace
      - name: Test workspace
        run: bun test
        working-directory: packages/workspace
```

The `test` job already installs `github:jj-vcs/jj@0.44.0` through mise and puts its shims on
PATH, so a suite that runs `mise x github:jj-vcs/jj@0.44.0 -- jj` (`STOCK_JJ` in the workspace
tests) needs nothing more. `bun install --frozen-lockfile` at the repository root is what gives
`bunx tsc` its `bun` types; a package's `tsc --noEmit` fails with
`Cannot find type definition file for 'bun'` without it.

Related: `docs/solutions/legion/template-names-a-ci-check-the-repository-never-produces.md` (the
converse — a PR body naming a check that no workflow produces).
