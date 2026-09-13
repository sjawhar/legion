---
title: "A package no workflow names has no CI — and a package a job lints, typechecks, and packs but never tests has none either: diff the packages with tests against the workflows' test steps, not their working-directory lines, and expect the first run to fail on environment"
category: testing
tags:
  - ci
  - github-actions
  - pr-and-main
  - working-directory
  - coverage-gap
  - jj-identity
  - first-run
  - missing-test-step
  - envoy-plugin
date: 2026-09-13
status: active
module: .github/workflows
related_issues:
  - "sjawhar/legion#1023"
  - "LEGION-96"
  - "sjawhar/legion#1086"
---

# A package no workflow names has no CI — and a package a job lints, typechecks, and packs but never tests has none either

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

## The second shape: named by a job, never tested by it (LEGION-96)

The check below as first written would have passed `packages/envoy-plugin` — it *is* named:
`.github/workflows/envoy-and-contracts.yaml`'s `envoy-plugin` job steps into it three times, for
`Lint envoy plugin` (`bun run lint`), `Typecheck envoy plugin` (`bun run typecheck`), and
`Pack envoy plugin` (`bun pm pack`). Every sibling job in the same file (`contracts`,
`envoy-client`, `pi-envoy`, `dispatch`, `claude-envoy-bridge`) also has a `Test <name>` step;
this one never did, and its seven test files / 61 tests ran nowhere. So when
`@legion/contracts` gained two Dispatch tools, the plugin's `dispatch-tools.test.ts` went red on
`main` (`Received + 2`) and stayed red until the LEGION-76 tester happened to run `bun test` from
the repository root. Lint and typecheck are both green on a stale assertion — a hand-typed list
that disagrees with runtime is well-formed, well-typed TypeScript. The fix (#1086) is the step the
siblings already had, in the same position:

```yaml
      - name: Test envoy plugin
        run: bun run test
        working-directory: packages/envoy-plugin
```

placed after `Typecheck envoy plugin` and before `Pack envoy plugin`, and proven on the pull
request's own run (the workflow's `on.pull_request.paths` already matched `packages/envoy-plugin/**`,
so the new step appeared on that PR — run 34823844127, `Test envoy plugin` success, `61 pass`).

## The check

Two lists, one difference — and the second list is the *test* steps, not every step. A
`working-directory:` line proves a job visits the package; only a step whose `run:` is the
package's test command proves it tests it:

```sh
# packages that carry tests
for p in packages/*/; do
  n=$(find "$p" -name '*.test.ts' -not -path '*/node_modules/*' | wc -l)
  [ "$n" -gt 0 ] && echo "$p"
done | sort > /tmp/with-tests
# packages some workflow step TESTS: a `run:` that is `bun test` or `bun run test`,
# followed by the step's own working-directory line
awk '
  /run: *(bun test|bun run test)( |$)/ { armed = 1; next }
  armed && /working-directory: packages\// { sub(/.*working-directory: /, ""); print $0 "/"; armed = 0; next }
  /^ *- name:/ { armed = 0 }
' .github/workflows/*.y*ml | sort -u > /tmp/tested-in-workflows
comm -23 /tmp/with-tests /tmp/tested-in-workflows   # packages whose tests CI never runs
```

At `main` before #1086 this printed `packages/envoy-plugin/`; the old `working-directory`-only
form printed nothing. Read any survivor's job by eye before believing either list — a `run:` on the
line after `name:` and a `working-directory:` two lines later is the shape every job here uses, and
the `awk` assumes it (a job that puts `working-directory:` at job level, or runs its tests through a
script, needs a human read). Run it when adding a package **and when adding a job for one**, and
read the named workflow to confirm the step is *test*, not just *build*, *pack*, or *publish*
(`release.yaml` names `pi-envoy` and `envoy-plugin` for publishing; their tests live in
`envoy-and-contracts.yaml`).

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
