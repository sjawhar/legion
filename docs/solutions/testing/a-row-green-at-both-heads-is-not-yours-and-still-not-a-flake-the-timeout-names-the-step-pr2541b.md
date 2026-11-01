---
title: "A row green at both heads is not your branch's, and still not a flake: the timeout names the step that never finished, and the teardown error is only the teardown"
category: testing
tags:
  - playwright
  - flake
  - ci
  - findings
  - issue-pod
date: 2026-10-31
status: active
module: packages/dispatch/e2e
related_issues:
  - "sjawhar/legion#2541"
---

# A row green at both heads is not your branch's, and still not a flake: the timeout names the step that never finished, and the teardown error is only the teardown

Two runs settle whether a red belongs to your change. They settle nothing about what it is, and
the step between those two conclusions is where this round was wrong.

- **Run the exact row CI printed, repeatedly, at your head and at `main`.** Take the row's name
  from CI's own line and select it with `-g`, so a selector that matches nothing reports
  `0 passed` rather than a green run that tested nothing, and repeat it: `bunx playwright test
  <spec> --project=<the project CI named> -g '<the printed name>' --repeat-each=5`. Clone `main`
  beside the workspace rather than touching it — `git clone --shared --branch main
  "$LEGION_WORKSPACE/.git" "$TMPDIR/mainrun"` — and record `diff -q` of the spec in both, which
  says the two runs differ only in the code your branch changed.
- **Green at both heads means "not this branch's". It does not mean "the environment".** Hand it
  over with that wording (`skill://legion-worker/references/findings.md`) and let whoever owns the
  code read the trace: a row that passes 30 times in a quiet pod can still lose a real race under
  a loaded runner, and calling it a flake in your handoff is a claim your runs did not make.
- **Read the timeout, not the error after it.** A Playwright row that ends `Test timeout of
  30000ms exceeded.` and then `browserContext.close: Protocol error
  (Target.disposeBrowserContext)` at the spec's own `finally` is one fault, not two: the context
  went with the timed-out test, and the teardown line is where the runner gave up. The fault is
  the step that never finished, which the trace names and the log does not.

## Evidence

sjawhar/legion#2541 (INFRA-264). CI job 124592486461 at `61e45835` failed one row of
`packages/dispatch/e2e/overlapping-marks.e2e.ts:311` with exactly that pair of lines, 1433 rows of
the same suite green beside it, in a directory the branch does not touch. The row then passed
15/15 at the branch head and 15/15 in a `--shared` clone of `main` at `290625b7` (which contains
`b4dc9f96`, the `main` the branch had merged), with the spec byte-identical in both, and the
re-run of the failed jobs settled green — so the round reported a flake of the harness.

It was not one. INFRA-317's architect had traced the same row at 18:57Z: the test checked only the
server's record before making the next quote, while the margin refuses a new quote while a
composer's send is still in flight, so the next composer never opened and the row ran out its 30
seconds. The fix waits for the composer to close, and passed 10 of 10 with a send held and 40 of
40 repeated. The two-head runs were right about whose red it was and wrong about what it was; the
hand-over is what reached the role that already knew.

A pod note from the same round: a port answering is not the harness you remember. Each pod
replacement takes the issue volume's harness with it, and the role that rebuilds it may `initdb`
under its own name, so `psql -p 5433 -U postgres` answered `FATAL: role "postgres" does not exist`
on a server that was healthy for its owner. Probe it (`select current_user`) before assuming whose
it is, and start your own on another port with your own database rather than stopping the holder.
