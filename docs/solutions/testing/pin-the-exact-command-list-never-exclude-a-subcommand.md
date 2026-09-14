---
title: "Pin the exact command list, never exclude a subcommand: how a rebase-time test narrowing let an identity write through, and the mutation check that caught it"
category: testing
tags:
  - bun-test
  - mutation-testing
  - fake-command-runner
  - argv-pins
  - rebase
  - code-review
  - jj-config
date: 2026-09-14
status: active
module: packages/workspace
related_issues:
  - "LEGION-84"
  - "sjawhar/legion#1080"
  - "LEGION-44"
---

# Pin the exact command list, never exclude a subcommand

A fake-command-runner test proves a sequence by pinning the exact list of commands the code ran.
Two features that emit into the same command stream — here two `jj config` writers on one shared
per-repo file — collide in those pins the moment one branch rebases over the other. The tempting
repair is to *narrow the filter* so the newcomer's commands fall out of the list. That repair
silently deletes the lock the test existed for. Add the newcomer's commands to the list instead.

## What happened

`main` had landed LEGION-44: `removeRepoScopedIdentity` probes and unsets a repository-scoped
`user.name`/`user.email`, and two tests pinned the *complete* list of `jj config` commands:

```ts
expect(calls.filter((cmd) => cmd[0] === "jj" && cmd[1] === "config")).toEqual([
  identityProbe(repoCloneDir, "user.name"),
  ["jj", "config", "unset", "--repo", "-R", repoCloneDir, "user.name"],
  …
]);
```

That closed enumeration is the property: *no* `jj config` command other than these runs, so any
stray identity write in provisioning fails the test.

LEGION-84's branch added `jj config get git.abandon-unreachable-commits` and, on the fake runner
(empty stdout to the read), `jj config set --repo git.abandon-unreachable-commits false` before
every fetch. On rebase the two tests failed, and the first fix narrowed the filter:

```ts
// WRONG: drops every `config set` — including an identity write — from what the test checks.
calls.filter((cmd) => cmd[0] === "jj" && cmd[1] === "config" && cmd[2] !== "get" && cmd[2] !== "set")
```

Green, and wrong. The reviewer mutation-tested it: with
`await run(deps, ["jj", "config", "set", "--repo", key, '"x"', "-R", repoCloneDir])` inserted after
each successful `unset` in the removal branch, all four identity/clone tests still passed — the
exact bug LEGION-44's tests were written to catch. The clone test's new `user.name`/`user.email`
guards did not cover it either: its fake probes report no identity, so the removal branch never
runs there.

## The fix

Pin the newcomer's exact commands in their position in the full list and keep the broad filter:

```ts
expect(calls.filter((cmd) => cmd[0] === "jj" && cmd[1] === "config")).toEqual([
  readKeepUnreachableCommitsCommand(repoCloneDir),
  writeKeepUnreachableCommitsCommand(repoCloneDir),
  identityProbe(repoCloneDir, "user.name"),
  ["jj", "config", "unset", "--repo", "-R", repoCloneDir, "user.name"],
  …
]);
```

Re-run the mutant: it fails `removes a repository-scoped jj identity from the shared clone,
logging each key` (the one identity test whose fake lets the unset succeed) and the real code
passes. Then restore the production file with `jj restore <path>` — never leave a mutant in the
working copy, and never fold one into a commit.

## The rules

- **A pinned list is a closed enumeration. Keep it closed.** When a rebase brings new commands
  into a pinned stream, the fix is more entries, never a narrower predicate. Excluding a
  subcommand (`cmd[2] !== "set"`) or a key excludes the very shape the test defends against.
- **Argv helpers make the entries cheap.** One `xxxCommand(repoCloneDir)` per production command,
  beside the existing helpers (`resolveBookmarkCommand`, `identityProbe`), so every pinned list
  spells the same argv and a change to the production argv is a one-helper edit.
- **Mutation-test any assertion you weakened to make a rebase green.** Insert the plausible wrong
  behaviour the test was written for — here, a `config set` of an identity key — and confirm the
  test fails; if it still passes, the narrowing removed the lock. This is the same method as
  `docs/solutions/testing/mutation-proof-probe-tests.md`, applied at rebase time by the author,
  not only by the reviewer.
- **Say what the pin proves in the rebase comment, precisely.** "Still proves LEGION-44's property
  while tolerating the guarded read/write" was wrong for the narrowed filter; the accurate claim
  for the fixed pin is "proves LEGION-44's property with LEGION-84's commands pinned". A reviewer
  who reads the claim against the diff will notice the gap either way — better it is true.

## Where the pattern recurs in Legion

Any two features that both talk to the shared clone's jj config (identity scrubbing, a `git.*`
setting, a future `snapshot.*` or `git.fetch` key) share one `cmd[1] === "config"` stream and one
per-repo file (`docs/solutions/daemon/a-per-repo-jj-setting-is-one-shared-file-rewritten-in-place-read-first-write-only-on-mismatch.md`).
Expect their tests to collide on every rebase between them, and resolve the collision by adding
entries.
