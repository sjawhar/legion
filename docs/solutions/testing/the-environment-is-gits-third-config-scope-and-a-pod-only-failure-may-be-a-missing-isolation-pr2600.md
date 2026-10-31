---
title: "The environment is git's third configuration scope, and a pod-only failure may be a missing isolation rather than a test the pod cannot run"
category: testing
tags:
  - git-config
  - test-isolation
  - legion-pod
  - GIT_CONFIG_COUNT
date: 2026-10-31
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2600"
---

# The environment is git's third configuration scope, and a pod-only failure may be a missing isolation

Extends docs/solutions/testing/a-test-that-reads-repository-git-config-must-pin-its-global-and-system-scopes.md.

That page's rule — a test that reads a repository's configuration with real git pins
`GIT_CONFIG_GLOBAL` and `GIT_CONFIG_NOSYSTEM` — shuts out two scopes. There is a third.

- **`git config --get-all` lists the environment's own scope too**: the
  `GIT_CONFIG_COUNT` / `GIT_CONFIG_KEY_<n>` / `GIT_CONFIG_VALUE_<n>` block. A Legion issue pod sets
  it for every process. Pin it with `GIT_CONFIG_COUNT=0`, which makes git read none of the pairs,
  in the test's process and in every child of it.

- **Strip the whole block, never the count alone**, wherever production code removes it from a
  child's environment. Either half left behind misleads a step that appends pins after the
  inherited count: a count with no keys has git read pairs that are not there, and keys with no
  count are pairs the appended ones get numbered over.

- **Before you add a name to the list of tests a pod cannot run, ask whether the pod exposed a real
  gap in the test.** That list is for tests the environment cannot support, not for tests the
  environment caught. A base-commit control proves the failure pre-exists your change; it does not
  prove the failure is not yours to fix.

## Evidence (sjawhar/legion#2600, INFRA-623)

- A Legion issue pod sets `GIT_CONFIG_COUNT=5`, and two of those five pairs are
  `credential.https://github.com.helper` — once empty, once `!legion git-credential`.

- `packages/daemon/internal/daemon/clonecredential_test.go` pinned `GIT_CONFIG_GLOBAL` and
  `GIT_CONFIG_SYSTEM`, and its own comment said why. Its
  `git config --get-all credential.https://github.com.helper` then read **four** values in a pod
  where the clone's file holds two, and `TestTmuxBootRewritesEveryOlderClonesCredentialHelper`
  failed. Restoring the base commit's copy of that file in a scratch tree reproduced the same
  failure, so it pre-dated this change — and it was the test's own gap, fixed here with
  `t.Setenv("GIT_CONFIG_COUNT", "0")` rather than parked. It is not in
  `docs/solutions/testing/go-test-in-a-worker-pod-leaves-gowork-to-go-work-and-names-the-packages-the-pod-cannot-run-LEGION-630.md`'s
  list, and it should not be.

- The production half of the same fact: `config.go`'s `withoutGitConfigPairs` strips
  `GIT_CONFIG_COUNT` and every `GIT_CONFIG_KEY_<n>`/`GIT_CONFIG_VALUE_<n>` from a provisioning
  child's base environment before the command's own pairs are merged in, so a pod's role
  credential helper reaches no clone or fetch. Its comment carries the "whole block, never the
  count alone" reason. The fixtures' half is `workspace_test.go`'s `fixtureEnvironment`, which
  calls the same helper rather than restating which entries make up the block.
