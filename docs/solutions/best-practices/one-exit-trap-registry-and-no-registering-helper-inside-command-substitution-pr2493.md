---
title: "One EXIT trap registry, and no registering helper inside command substitution"
category: best-practices
tags:
  - shell
  - trap
  - cleanup
  - credentials
date: 2026-10-29
status: active
module: .github/scripts
---

# One EXIT trap registry, and no registering helper inside command substitution

Narrows rule 1 of docs/solutions/best-practices/a-shared-bash-helper-sets-no-exit-trap-INFRA-629.md:
`on_exit_remove` is the one function that runs `trap … EXIT`, over every directory registered with
it. Every other helper, shared or not, registers through it and never calls `trap` itself.

## The rule

1. When one command needs several directories removed at exit, keep one array and one function
   that appends to it and sets the one trap over the whole array (`on_exit_remove <dir>`). Never set
   a second trap.
2. Make a setup helper that creates a credential directory return at once on a second call, so two
   callers share one directory and nothing leaks.
3. Never call a function that registers a directory inside `$(…)`. The subshell starts with the
   inherited array and no trap. Registering arms a new trap in the subshell over the whole array,
   and when the substitution ends, that trap removes the parent's directories too, the deploy key's
   among them. Call the function directly, and return its result in a named variable.
4. Lock rule 3 with a test that sees the credential files on disk while the push runs, for example
   a pre-receive hook that records the files in the key directory.

## Evidence

- sjawhar/legion#2493: `on_exit_remove` (`.github/scripts/release-push.sh:446`) holds the key
  directory and the temporary index's directory. `deploy_key_env` returns early on a second call,
  since push-built fetches before `land_on_main` fetches again. `commit_build_on` (`:630`) sets the
  global `commit`, and its header says why it is not called as `commit=$(…)` (review thread
  PRRT_kwDORFy7ds6xe-nc).
- Checked on bash 5.2.37: a helper calling `on_exit_remove` inside `x=$(helper)` removed the
  parent's registered directory once the substitution ended.
- `.github/scripts/release-push.test.sh` (the moved-by-an-empty-commit case, `record_hook` at line
  1478) asserts that the key directory holds exactly `key` and `known_hosts` while the push runs.
