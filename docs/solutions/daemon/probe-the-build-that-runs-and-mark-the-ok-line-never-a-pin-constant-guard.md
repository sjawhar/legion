---
title: "Probe the build that runs and mark the OK line for consumers; never guard a capability with a pin-constant comparison"
category: daemon
tags:
  - boot-probes
  - probe-image
  - omp-fork-pin
  - worker-image
  - session-storage
  - launch-prefix
date: 2026-09-14
status: active
module: packages/daemon/src/daemon/boot-probes.ts, packages/daemon/src/cli/index.ts (probe-image), packages/daemon/docker/worker.Dockerfile
related_issues:
  - "LEGION-80"
  - "sjawhar/legion#1081"
---

# Probe the build that runs, and mark the OK line

## The question

A new fork setting (`session.storage: sql`) only exists from one fork release on. A Legion
deployment that sets `OMP_SESSION_STORAGE=sql` on an older build gets exactly the silent failure
the setting exists to prevent: the variable is ignored and sessions stay on files. How does the
daemon know the OMP that will actually run carries the setting?

## The wrong answer

Compare `OMP_FORK_PIN` against the release that introduced the setting. It is a tautology after
the bump — the same pull request moves the constant — and it says nothing about the OMP inside a
digest-pinned worker image, which was built from *some* pin at *some* commit. A guard that reads
a constant the daemon owns proves what the daemon believes, not what runs.

## The pattern

**Probe the binary.** `verifySessionStorageSetting` (`boot-probes.ts`) starts the build with
`OMP_SESSION_STORAGE=legion-launch-probe` — a value no build of any age accepts — and reads the
answer from the build itself:

- exit non-zero with the variable named on stderr → the build carries the setting (pass);
- exit 0 → the build started on a nonsense value, so it never read the variable: definitive,
  "this build predates the session.storage setting and would silently keep sessions on files";
- exit non-zero without the variable → the launch prefix or OMP died before the resolver:
  transient, retried per policy.

Three things made this workable and are reusable:

1. **Find a provider-free, network-free, TTY-free path that reaches the decision point.** Most
   flags exit before the resolver (`--version`, `--export`), never reach the root command
   (`omp models`, `omp gc`), or need a model (`-p`, `--mode rpc` → "No models available", exit 1
   without a key). `PI_TIMING=x` makes an interactive start print its timing tree and exit 0 just
   before the TUI opens; stdin on `/dev/null` keeps the start interactive (an empty non-TTY stdin
   is not a piped prompt); `--no-session --no-extensions --no-skills --no-rules --no-lsp --no-tools`
   keeps the profile out of it. The resolver runs right after settings load, ahead of that exit.
   Document the ordering you rely on — a refactor of the fork can move the exit point without
   breaking anything visible except this probe.
2. **A probe whose negative answer is an exit code has no marker.** The shared `killedOutcome`
   helper treated a printed negative marker as "already answered, definitive even if killed".
   For this probe a killed attempt has no exit code to read, so every budget kill is transient:
   the helper's `negativeMarker` became optional rather than passing a string that could match
   by accident.
3. **Print a token on the success line for the next consumer.** `legion probe-image` prints
   `probe-image: OK (<omp path>) session-storage=probed` (`SESSION_STORAGE_PROBE_MARK`, one
   exported constant). An image built before the probe existed prints a bare `probe-image: OK`
   having checked nothing, and the two are otherwise indistinguishable. The daemon that accepts
   an image for a `sql` session store requires the token in the probe pod's output — importing
   the constant, never retyping it — instead of probing a host OMP.

## Where it runs, and where it must not

The image build runs it unconditionally (`legion probe-image`, third), so no worker image
publishes on an older build. The daemon's own boot does **not** run it, for two reasons that are
worth stating inline so nobody wires it in "for completeness": a tmux deployment on an older
build must stay untouched; and this probe classifies a launch-prefix failure as transient (its
negative answer is an exit code, so a `secrets` denial is indistinguishable from OMP dying
early), which the daemon's **unbounded** boot retry policy would retry forever. The image gate's
policy is bounded (six attempts), so the same classification is safe there.

## Keep the prose in sync

Adding a probe with a different runtime scope than its siblings put "the daemon's two probes" in
five places (module header, Dockerfile twice, daemon AGENTS.md, docs/kubernetes.md). The first
pass updated three and the review found the header saying the daemon should run it. When a
shared module gains a member with a narrower scope, grep every sentence that counts the members.
