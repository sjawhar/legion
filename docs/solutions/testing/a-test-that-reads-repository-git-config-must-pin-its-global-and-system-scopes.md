---
title: "A test that reads a repository's git config with real git must pin GIT_CONFIG_GLOBAL and GIT_CONFIG_NOSYSTEM, or the developer's own credential.helper lands in the assertion"
category: testing
tags:
  - git-config
  - test-isolation
  - real-git
  - credential-helper
  - review-finding
date: 2026-09-15
status: active
module: packages/workspace/src/workspace.test.ts
related_issues:
  - "LEGION-178"
  - "sjawhar/legion#1121"
symptoms:
  - "a `git config --get-all credential.helper` assertion that expects `\\n!<pane helper>\\n` receives `store\\n\\n!<pane helper>\\n` (or any other extra first line) on one developer's box and passes on CI"
  - "a test over a bare fixture repository fails before the behaviour under test runs"
---

# A test that reads a repository's git config with real git must pin its global and system scopes

## The finding

LEGION-178's regression test provisions a bare fixture clone with the production config
(pane helper on `credential.helper` and `credential.https://github.com.helper`,
`credential.interactive=false`), then asserts what was persisted with real git:

```ts
const persisted = async (...args: string[]) =>
  (await runCommand([SYSTEM_GIT, `--git-dir=${gitDir}`, "config", ...args])).stdout;
expect(await persisted("--get-all", "credential.helper")).toBe(`\n!${paneHelper}\n`);
```

The `git credential fill` calls in the same test were isolated (`GIT_CONFIG_GLOBAL=/dev/null`,
`GIT_CONFIG_NOSYSTEM=1`, a fixture `HOME`); the `git config` reads were not — `runCommand` with no
options spawns with the process's own environment, so git read the box's `~/.gitconfig` and
`/etc/gitconfig` too. `--get-all` concatenates every scope in read order. The reviewer reproduced
it with a global `credential.helper = store` on git 2.43: the assertion received
`store\n\n!<pane>\n`, and the test failed at its first `expect` — before the credential contract
it exists to defend ever ran. The implementer's box has an empty global config, so the test was
green there and on the CI runner; that is what made the gap invisible until review.

## The rule

Every real-git invocation in a test — reads included, not only the pushes and credential fills
the identity-proof learning already covers — runs under one isolation object:

```ts
const isolation = {
  GIT_CONFIG_GLOBAL: "/dev/null",   // no ~/.gitconfig: no credential.helper, no includes
  GIT_CONFIG_NOSYSTEM: "1",         // no /etc/gitconfig
  HOME: fixtureDir,                 // anything git resolves through $HOME lands in the fixture
  XDG_CONFIG_HOME: fixtureDir,      // ~/.config/git too
};
```

and the reads pass it: `runCommand([...], { env: isolation })`. `git config --local` is the
other correct spelling for a read that must see only the repository file; the reviewer verified
both. Pinning the env is preferred here because the same object already governs the fills, so one
definition keeps the reads and the fills looking at the same git.

## How to prove the fix (and reproduce the finding) without touching your own config

Never edit the box's real global config for this. Point `GIT_CONFIG_GLOBAL` at a scratch file for
the **test runner's** process, which the unisolated child inherits and the isolated one overrides:

```sh
mkdir -p /tmp/scratch-global
printf '[credential]\n\thelper = store\n' > /tmp/scratch-global/gitconfig
GIT_CONFIG_GLOBAL=/tmp/scratch-global/gitconfig bun test src/workspace.test.ts -t '<the test>'
# before the fix: (fail) … Received: "store\n\n!<pane>\n"
# after the fix:  1 pass — and the whole file passes with and without the scratch global
```

That red→green under the same scratch global is the evidence the PR body and the thread reply
carry; a green run on a box with an empty global config proves nothing about this class of bug.

## Related

- `isolate-git-from-the-pane-credential-helper-for-identity-proofs.md` — the same two variables,
  for a push or credential fill that must not redeem the pane's own grant.
- `../daemon/a-repository-git-config-serves-one-caller-another-overrides-it-through-git-config-env-pairs.md`
  — the fix this test defends.
