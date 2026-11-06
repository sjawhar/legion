---
title: "An invariant between two values is chosen in one place, and what stays by hand is named as risk and measured over one real run"
category: architecture-patterns
tags:
  - invariant
  - helper
  - subprocess
  - path-shim
  - census
date: 2026-10-31
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2600"
---

# An invariant between two values is chosen in one place, and what stays by hand is named as risk and measured over one real run

Two values that must agree — a flag and a directory, a key and its count, an id and the row it
names — drift at every site that sets them separately.

- **Route every site through the one helper where both are derived from a single variable**, and
  state the invariant *there*, where the choice is made. A helper that takes one argument and spends
  it twice cannot disagree with itself.

- **State it once.** A list of call sites in a doc comment and a second list in a guide both go
  stale the first time a site is added, and neither fails anything when it does.

- **Where you cannot make it total, say so in the risk section with the failure a new site causes**,
  not "every site does this today". The reader who adds the next site is the audience.

- **Then measure the invariant over one real run instead of asserting it in prose.** Put a shim
  first on `PATH` that logs each child's own resolved working directory and argv before exec'ing
  the real binary, drive the production command, and count: how many children, how many in the
  wrong directory, how many whose flag and directory disagree. Run the same trace against the base
  commit; the two counts are the claim.

- **A plan's list of call sites is a lower bound.** Re-derive it with the mechanism — a trace, an
  AST scan, a grep for both the command's name and its package's — before you believe it is
  complete.

## Evidence (sjawhar/legion#2600, INFRA-623)

- The invariant: a `jj -R <x>` command is safe only because it runs with `Dir` = `<x>`. `-R` is no
  fence — jj 0.45.1 loads the repository its working directory sits in before it applies `-R`.
  `runCheckedOnClone` keeps it by construction: one `cloneDir` reaches both the `-R` and
  `Command.Dir`. Three sites kept it by hand (`undescribedMove` and `removeRepositoryIdentity`'s
  two probes) until a review round routed them through that helper.

- It had been written down twice, in `jjWorkspaceRoot`'s comment and in `docs/testing.md`'s list of
  call sites, and the review asked for one statement where the directory is chosen; both copies were
  deleted and the guide now points at that comment.

- What stays by hand is in the pull request's `Not proven / risk`, with the consequence spelled out:
  "`RunCheckedIn`, `createWorkspace`'s `jj workspace add`, `readBookmark`, the identity `unset` and
  the fetch each pair the two by hand. Every site does today, and a new call site that breaks it
  writes the repository its directory sits in." One of those, the fetch, cannot be routed at all:
  the helper passes a nil environment and the fetch needs the one-shot credential's.

- The measurement that stands in for enforcement: a `PATH` shim for `git` and `jj` logging each
  child's `pwd -P`, its argv and its `GIT_CONFIG_*`, over a real
  `legion workspace-init provision` — **23 children, 0** in the process's working directory, and of
  the 12 jj children, 11 naming a `-R` and each running in exactly that directory with no mismatch,
  the 12th the rootless create under the guard. The base-commit control: **23 of 23** in the wrong
  directory, each carrying the pod's inherited configuration block.

- The lower-bound rule, learned the expensive way: the plan named two bad call sites. The trace and
  the sweep that followed found **nineteen sites in ten files**, two of them (in
  `internal/shim/shim_test.go`) the production defect's own shape — a rootless
  `jj git init --colocate` with no directory of its own.
