---
title: "A go test step is protected only by the -timeout it names itself: CI's test job gives packages/daemon 20 minutes in one step and go test's 600 s default in the next"
category: testing
tags:
  - ci
  - go-test
  - timeout
  - race-detector
  - packages-daemon
date: 2026-11-03
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2674"
---

# A go test step is protected only by the -timeout it names itself

Extends docs/solutions/testing/the-pod-only-skip-list-is-re-derived-against-a-main-extract-and-the-daemon-package-needs-a-longer-timeout-INFRA-225.md.

That document says to give `internal/daemon` `-timeout 25m` and measures it at 459–853 s, but it
is about a local run in a pod.
On CI the same package runs **twice** in one job, and only the first run carries a deadline.

## The rule

A `go test` step is protected only by the `-timeout` it names itself.
The step above it in the same job protects nothing, however generous its own flag.
In `.github/workflows/ci.yaml`'s `test` job, `packages/daemon` gets `-timeout 20m` at
"Test the Go daemon" (`:1769`) and `go test`'s bare 600 s default at
"Test the concurrent packages under the race detector" (`:1779`), where the race detector makes the
same packages slower, not faster.

So a red `test` job whose panic reads `test timed out after 10m0s` and names a `packages/daemon`
package is the **race** step, not the one with the flag: a 20-minute deadline panics at `20m0s`.
Read the job's step list before you read its log, because the log's `running tests:` line names only
the test that happened to be executing, as INFRA-391 says:

```bash
gh api repos/cybermonklabs/legion/actions/jobs/<job id> \
  --jq '.steps[] | [.number, .name, .conclusion, .started_at, .completed_at] | @tsv'
```

For the round, the remedy is the ordinary one and no code: one re-run after the run settles, push
nothing (see
docs/solutions/legion/an-out-of-paths-red-is-triaged-by-whether-it-is-deterministic-at-the-merge-commit-a-flake-gets-one-re-run-and-a-round-reopened-above-retro-pushes-no-code-pr2619.md).
The fix is a `-timeout` on that step and belongs to whoever owns the workflow, not to a branch that
touches no file under `packages/daemon/`.

Whoever adds the flag raises the job's limit with it, or sizes the flag to fit.
The job is `timeout-minutes: 45` (`ci.yaml:1560`), so 5 minutes of setup plus 20 plus 25 would let
the runner cancel the job before Go prints the goroutine dump a hang needs — the budget rule in
docs/solutions/testing/a-race-lane-names-the-packages-that-hold-the-shared-state-and-its-job-limit-outlasts-every-steps-test-timeout-INFRA-141.md.

## Evidence

sjawhar/legion#2674, CI run 41562871172 at head `6e63b9aa`, a pull request that changes no file
under `packages/daemon/`.

Job `test` 125688799071, attempt 1, `failure`:

| step | name | conclusion | duration |
| --- | --- | --- | --- |
| 47 | Test the Go daemon | success | 03:59:58Z → 04:13:59Z (841 s) |
| 48 | Test the concurrent packages under the race detector | **failure** | 04:13:59Z → 04:25:41Z (702 s) |

The failure printed `panic: test timed out after 10m0s` with
`FAIL github.com/sjawhar/legion/daemon/internal/daemon 600.113s`, naming
`TestThePromptReferenceProbeOnTheRealOhMyPi` under `running tests:` — which had run for seconds.
Step 47, with its 20 minutes, passed on the same runner at 841 s.

Job `test` 125740406154, attempt 2 of the same commit with nothing changed, `success`:
step 47 489 s, step 48 448 s.
So step 48 normally finishes four packages and the race compile in less time than the one package
was given, and attempt 1's runner was about 1.7× slower on step 47 alone.
A missing flag is what turned a slow runner into a red `test` and a red required `ci-ok`.
