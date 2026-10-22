---
title: "A fixture built by arithmetic on the old limit fails loudly at it and silently past it"
category: legion
tags:
  - limits
  - tests
  - fixtures
  - sweep
date: 2026-10-22
status: active
module: packages/daemon/internal/workflow, packages/envoy/internal/dispatch/api, packages/dispatch
related_issues:
  - "INFRA-45"
  - "sjawhar/legion#2097"
---

# A fixture built by arithmetic on the old limit fails loudly at it and silently past it

Extends docs/solutions/legion/a-contract-bumps-sweep-includes-test-expectations-spelling-the-bare-number-replace-them-with-the-packages-derived-constant-LEGION-636.md.

That document sweeps a bare number a test *states*. This is the delta for a number a test
*computes with*, which no grep for the digits finds.

- A fixture built **at** the limit (`strings.Repeat("a", contracts.DispatchBodyMax)`) fails loudly
  when the limit moves and is safe: derive it from the constant and move on.
- A fixture built to **exceed** the limit by a fixed count stops exceeding it, and the path it
  existed to reach goes untested with every test still green. Build it from the limit instead, with
  relative headroom, so it survives the next move too.
- Fix such a fixture at the fixture, never at the assertion. An assertion loosened to accommodate a
  fixture proves nothing.
- A test whose premise *cites* the limit as a justification is a third kind: grep the tests for the
  limit used in an argument, not only as a value, because a raise can revive a premise the test was
  written to dismiss.
- An expectation that pins **wording** stays a literal. Derive the size from the constant; never
  compute the expected sentence with the formatter under test, and never convert an expectation for
  a limit the change does not move.

## Evidence (INFRA-45, sjawhar/legion#2097)

Raising Dispatch's body cap moved the Legion daemon's derived `record.MessagePostLimit` from 1,956
to 31,956, and three fixtures behaved three different ways.

`packages/daemon/internal/workflow/ready_test.go`'s
`TestAWithdrawalOfAMergeTheQueueFailedFitsOneDispatchMessage` built 40 failing merge-queue runs so
the withdrawal reason would overrun one Dispatch message and be cut to fit. Forty runs made about
3,931 characters: past 1,956, nowhere near 31,956. The cut-to-fit path stopped being exercised and
the test failed only because it asserts the body *says* it was cut — CI caught it at `76c1eb9c`
(run 39655628970). The fixture now appends run lines until the reason is a tenth past
`record.MessagePostLimit`, so it follows the constant whatever it becomes.

`packages/dispatch/web/src/features/refs/Unfurl.test.tsx` was the loud kind: its bodies were
`scheme.repeat(Math.floor(1998 / scheme.length))`, sized to the old cap, so the catastrophic-
backtracking guard was still measuring 2,000-character bodies. Resized to the constant, it measured
4.380 ms against its unchanged 2,000 ms budget, so `isBareReferenceBody` is linear at sixteen times
the old cap and no production change was needed. Its millisecond budget was deliberately **not**
swept: `2000` there is milliseconds, and raising it would have disabled the thing the test exists
for.

`packages/envoy/internal/dispatch/api/search_pathological_test.go` was the premise kind. It had
argued it needed no comment ladder *because* a comment was capped at 2,000 characters, too short to
tell the slow code from the fast; its removed timings measured a 2,000-character `a_` comment at
51 ms. At 32,000 that premise is gone, so the file now ladders comments at `DispatchBodyMax/32`,
`/4` and the cap, held by its existing `growsLinearly` bound.

The wording rule came from a review finding: the sweep had replaced two **800**-character literal
expectations in `packages/envoy-client/src/__tests__/dispatch-execute.test.ts` and
`dispatch-images.test.ts` with `overCapMessage(821, 800)` and `overCapMessage(801, 800)` — the
formatter under test, for a cap this change does not move — which makes the assertion pass through
any wording change it exists to pin. Both are literal text again.
