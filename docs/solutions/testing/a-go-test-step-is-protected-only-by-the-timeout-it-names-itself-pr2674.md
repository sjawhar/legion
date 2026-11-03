---
title: "A go test step is protected only by the -timeout it names itself: CI's test job names one on each of its two Go steps, 15 minutes for the plain step and 30 for the race step"
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
  - "INFRA-1052"
---

# A go test step is protected only by the -timeout it names itself

Extends docs/solutions/testing/the-pod-only-skip-list-is-re-derived-against-a-main-extract-and-the-daemon-package-needs-a-longer-timeout-INFRA-225.md.

That document says to give `internal/daemon` `-timeout 25m` and measures it at 459–853 s, but it
is about a local run in a pod.
On CI `internal/daemon` runs once per `test` job, in the race step, and that step carries
`-timeout 30m`.

## The rule

A `go test` step is protected only by the `-timeout` it names itself.
The step above it in the same job protects nothing, however generous its own flag.
In `.github/workflows/ci.yaml`'s `test` job both Go steps now name an explicit `-timeout`.
"Test the Go daemon" runs every package of the module except the four concurrent ones under
`-timeout 15m`, and "Test the concurrent packages under the race detector" runs
`./internal/daemon/`, `./internal/record/`, `./internal/sessionproxy/` and `./internal/launcher/`
under `-timeout 30m`, where the race detector makes those packages slower, not faster.

So a red `test` job whose panic names a `packages/daemon` package tells you which step it came from
by the deadline it prints: a 15-minute deadline panics at `15m0s`, a 30-minute one at `30m0s`.
Read the job's step list before you read its log, because the log's `running tests:` line names only
the test that happened to be executing, as INFRA-391 says:

```bash
gh api repos/sjawhar/legion/actions/jobs/<job id> \
  --jq '.steps[] | [.number, .name, .conclusion, .started_at, .completed_at] | @tsv'
```

For the round, the remedy is the ordinary one and no code: one re-run after the run settles, push
nothing (see
docs/solutions/legion/an-out-of-paths-red-is-triaged-by-whether-it-is-deterministic-at-the-merge-commit-a-flake-gets-one-re-run-and-a-round-reopened-above-retro-pushes-no-code-pr2619.md).
The fix is a `-timeout` on that step and belongs to whoever owns the workflow, not to a branch that
touches no file under `packages/daemon/`.

Whoever adds the flag raises the job's limit with it, or sizes the flag to fit.
The job's limit must stay above the sum of the largest setup time and, for each `go test` step, its
largest measured green duration plus its own `-timeout`.
The `test` job's `timeout-minutes` is 80 for a measured worst case of 77, so a flag raised without
the limit can let the runner cancel the job before Go prints the goroutine dump a hang needs — the
budget rule in
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
