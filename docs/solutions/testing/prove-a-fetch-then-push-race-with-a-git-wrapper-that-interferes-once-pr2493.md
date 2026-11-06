---
title: "Prove a fetch-then-push race with a git wrapper that interferes once"
category: testing
tags:
  - bash-harness
  - git
  - bare-origin
  - race
  - pre-receive
date: 2026-10-29
status: active
module: .github/scripts
---

# Prove a fetch-then-push race with a git wrapper that interferes once

Extends docs/solutions/testing/bash-test-against-a-real-bare-git-origin.md.

## The rule

1. To test "the remote moved between my fetch and my push", put a script named `git` first on
   `PATH`. It runs the real binary by its absolute path (resolve it with `command -v git` before
   the change to `PATH`). After the first `fetch` it forwards, it pushes one more commit to the
   bare origin with that real binary, and a marker file stops it from doing so again. Git itself
   stays under test, and the race happens in the same place every run.
2. Assert the effect the code must take: what origin's branch ends as, and the exact log of every
   external call (here exactly one `gh workflow run`, never two).
3. Install a recording pre-receive hook only after the fixture's own pushes. Otherwise the fixture's
   push fires the hook with its variables unset, and the suite prints errors that look like
   failures.

## Evidence

- sjawhar/legion#2493: the race cases in `.github/scripts/release-push.test.sh` (the wrapper calls
  `$REAL_GIT` at lines 1696-1700). After the interfering push, push-built lands nothing, and
  `land_on_main`'s one fresh run is the only one.
- Review thread PRRT_kwDORFy7ds6xe-nk, fixed in 2d7200de83ad: `record_hook` ran before `move_main`,
  and the suite printed `hooks/pre-receive: line 2: : No such file or directory`. The tester's count
  of that line at 854256b6 was 0.
