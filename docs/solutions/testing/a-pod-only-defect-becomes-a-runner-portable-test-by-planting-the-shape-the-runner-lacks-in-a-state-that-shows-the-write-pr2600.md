---
title: "A pod-only defect becomes a runner-portable test by planting the shape the runner lacks, in the state that makes the foreign write visible"
category: testing
tags:
  - legion-pod
  - fixture
  - regression-test
  - decoy
  - measurement
date: 2026-10-31
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2600"
---

# A pod-only defect becomes a runner-portable test by planting the shape the runner lacks, in the state that makes the foreign write visible

A defect that reproduces in a Legion issue pod and not on a GitHub runner is not an environment
problem to be left to the environment. Name the one difference and plant it as a fixture; then CI
holds the defect for every later change.

1. **Name what the runner cannot reproduce, and plant exactly that — no more.** Write the sentence
   first ("a repository above the test process"), then build the smallest fixture that is that
   sentence, and put the test where the pod's process stands.

2. **The fixture must be in the state that makes the foreign write visible.** A tool that merely
   *opens* something it does not own often leaves no trace. Find the state in which it writes, and
   plant that state, or the test passes against the broken code.

3. **Never build a control on the shared resource.** A one-shot effect — a migration, a first
   write, an upgrade — burns itself the first time you trigger it, and the control stops
   reproducing for every later round. Build it on a fixture you can plant again.

4. **When the observation uses the same mechanism as the effect, measure out of band.** Reading the
   fixture with the tool under test is itself the write you are looking for. Read the files
   instead, and record what each non-regular entry *is*, not only what it holds, so a kind change
   shows up too.

5. **Keep the pod proof as well as the runner test.** The runner test holds the defect; the pod run
   is what shows the real resource untouched. Splitting them was deliberate here, and both lines
   are in the pull request body.

## Evidence (sjawhar/legion#2600, INFRA-623)

- The one difference: in a Legion issue pod the Go test binary stands inside `$LEGION_WORKSPACE`, a
  jj workspace of the pod's real shared clone; a GitHub runner's checkout is git-only with no `.jj`
  anywhere above it. `TestProvisionLeavesTheEnclosingRepositoryUntouched` builds a decoy colocated
  jj repository and `t.Chdir`s into it — "the decoy below stands in for the pod's clone and
  `t.Chdir` puts the test where the pod's test process stands, which is the whole of what a GitHub
  runner cannot reproduce".

- Rule 2 was found by measurement, not guessed: a `jj git clone` inside a *pristine* decoy opens the
  enclosing repository and writes nothing into it — refs, operation head and the whole `.jj`
  fingerprint were identical. The write only becomes visible when the enclosing repository still
  holds the legacy `.jj/repo/config.toml` that jj migrates, so the decoy plants that file. Without
  it, only the half of the test whose state directory sits *inside* the decoy would have separated
  the fixed code from the broken.

- Rule 3 bit this change: its own earlier runs migrated `/legion/repos/github.com/sjawhar/legion`,
  which now holds `config-id` and no legacy `config.toml`, so the first round's base control no
  longer reproduces a write on that repository. Every later control used a decoy.

- Rule 4 is `enclosingState`: it runs no jj in the decoy, because opening it with jj *is* the write
  under test — `jj op log` would migrate the configuration and, without `--ignore-working-copy`,
  snapshot the working copy, so "the measurement would destroy what it measures". It reads the
  operation head as a filename under `.jj/repo/op_heads/heads` and takes a SHA-256 of every file
  under `.jj`, recording an entry's type where it is not a regular file.
  `assertEnclosingRepositoryUnchanged` then names the individual file written, lost or rewritten,
  so a failure says what provisioning did rather than that something changed.

- The split: the plan separated the runner-portable regression (which must fail at the commit
  before the fix and pass at the head) from the pod run under a read-only mount, because "the real
  clone" does not exist on a runner and a literal reading of the acceptance criterion would have
  left CI guarding nothing.
