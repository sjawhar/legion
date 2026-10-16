---
title: "A throwaway sandbox repository probes GitHub Actions semantics with a control job beside the probe, every step that first executes in production rides the same run, and a reimplementation is differentialled against its oracle before anything is built on it"
category: testing
tags:
  - legion-smoke
  - github-actions
  - cache-mode
  - differential-testing
  - oracle
  - negative-control
  - bare-origin
  - worker-pod-tools
date: 2026-10-15
status: active
module: .github/workflows
related_issues:
  - "LEGION-748"
  - "sjawhar/legion#1969"
---

# A throwaway sandbox repository probes GitHub Actions semantics with a control job beside the probe, every step that first executes in production rides the same run, and a reimplementation is differentialled against its oracle before anything is built on it

## The rule

1. **Probe a workflow key or runner fact in a sandbox repository before editing the real
   workflow.** Push one throwaway branch (`on: push: branches: [<that branch>]`; a
   `workflow_dispatch` would need the file on the default branch) with one workflow holding the
   probe job and, in the same file, a control job identical but for the thing under test. Read
   the run with `gh run view <id> --log`, record the exact lines, delete the branch; the run
   outlives it and is the proof's link. Two pushes give the control a saved cache to restore.
2. **Put every step that first executes in production on that run.** Once the sandbox exists,
   list the steps of the change that no pre-merge surface reaches — a registry write, a binary
   the runner alone can execute, an action dropped on a measurement of its sibling — and probe
   each one, or write it into the pull request's `Not proven / risk` with the run that will
   prove it. A probe built for one unknown and not reused for the others is the gap the review
   records.
3. **A reimplementation of something an oracle already decides is proven by a differential
   first.** Extract the old side mechanically (the step bodies from the old workflow YAML with a
   YAML parser, never retyped), run old and new at several real commits — the ones a release was
   actually cut at, the one before it, the current head — and compare outputs pairwise; for a
   gate over a language, the oracle is the language's interpreter on the same bytes with a
   logging stub on `PATH`. Do it before building on the new side, and record the disagreement
   class in the component's header; do not wait for the oracle's edge cases to arrive as review
   findings.
4. **Every proof line carries a negative control on the same surface.** The control job that
   restores the cache; the same fixture green at the previous head; the exact-bump commit (A′)
   landing beside the doctored one (A) refused on the same base, so a refusal is shown to be
   about content, not shape; an empty hook log as the proof that nothing was pushed; a planted
   marker value and `grep -c MARKER` over all output as the proof that nothing was printed.
5. **The worker pod's image is not the CI runner.** It had no `jq`, `ssh`, `ssh-keygen`,
   `shellcheck` or PyYAML; install them under `$HOME/.local` (a `uv` target for PyYAML, the
   Debian packages extracted for OpenSSH, release binaries for jq and shellcheck) and tell every
   subagent to prepend `$HOME/.local/bin` to `PATH`, since a subagent's shell starts without
   it. A harness that needs them fails before its first case otherwise.

## Evidence

- sjawhar/legion-smoke runs 38758454703 (first push) and 38758671814 (second): the `probe` job
  with `cache-mode: none` printed `Set up job: Cache mode: none`, `Cache restore skipped: the
  effective cache-mode 'none' does not permit reads.`, `Cache save skipped: … does not permit
  writes.`; the `control` job printed `Cache mode: write`, saved
  `setup-go-Linux-x64-ubuntu24-go-1.26.8-…` on the first run and `Cache hit for: …`, `Cache
  restored successfully` on the second. The same run recorded the clean runner's credential
  baseline for the tripwire (no git config match, `~/.ssh` absent, five variables unset) and
  `docker buildx imagetools inspect --raw` with no builder action. The branch was deleted; the
  runs stayed.
- The gap rule 2 names: `docker buildx imagetools create`, `agent-secrets --version` on the
  linux/amd64 binary and `go version -m` on six cross-built binaries first run on the
  merge-triggered listener run; all three sit in sjawhar/legion#1969's `Not proven / risk`.
- Rule 3, both timings: the version consolidation (six inline computations into
  `release-push.sh version`) was differentialled at round 1 — 18 comparisons at three checkouts
  by the implementer, 43 of 43 (job, commit) pairs at eight checkouts by the tester — and drew
  no finding in six rounds; the checker's shell scanner was differentialled against bash only at
  round 6, after four rounds of lexing findings, and the review after it found nothing.
- Rule 4: `release-push.test.sh` case 2d (the exact bump lands) beside 2c (the same commit with a
  `scripts.postinstall` refused), the recording pre-receive hook's `seen` file empty on every
  refusal, `MARKER`/`example.invalid`/`postinstall` absent from every refusal's output.
