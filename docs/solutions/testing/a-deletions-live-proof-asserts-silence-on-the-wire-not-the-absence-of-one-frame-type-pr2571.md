---
title: "A deletion's live proof asserts silence on the wire, not the absence of the one frame type it deleted"
category: testing
tags:
  - live-proof
  - mechanism-removal
  - negative-assertion
date: 2026-10-31
status: active
module: packages/daemon/internal/daemon
related_issues:
  - "sjawhar/legion#2571"
---

# A deletion's live proof asserts silence on the wire, not the absence of the one frame type it deleted

Extends docs/solutions/testing/evidence-can-be-absent-rather-than-wrong-and-absence-reads-exactly-like-success-INFRA-31.md
with the shape for a removed mechanism.

"We no longer send X" cannot be asserted by looking for X once X's type is gone: the assertion
compiles away with the code it guarded, and a reader of the test later cannot tell a guarantee
from a leftover. Assert the stronger, type-free fact instead:

- **Read the peer's own connection for a window and require that nothing at all arrives** — not
  one frame of any kind. That holds whatever the removed mechanism is later replaced by.
- Put a **positive fact beside it in the same step**: the path that now acts did act, while the
  peer's turn was still running. Silence alone is also what a daemon that did nothing produces.
- Read the peer's state as well: its claim still working on the delivery it confirmed, and the
  runtime asked for no suspension. That rules out the other ways a turn could have been ended
  without a frame.

Say in the proof's own header why it holds the stronger fact, so the next reader does not
"restore" a frame-type assertion.

## Evidence

- `packages/daemon/internal/daemon/ci_red_no_takeover_live_test.go:26-29` states it: the RPC
  carries no abort frame and the daemon has no quiesce of a claim, so the proof holds that the
  role's connection is sent nothing at all while the start acts, rather than the absence of one
  frame type.
- `:356` `silent(within)` reads the role's own connection with a deadline and fails on any byte.
- Step 2 of the proof drives it for three seconds while asserting the implementer was prompted its
  fix task at once, the tester's claim is still `working` on the delivery it confirmed before the
  red, and the fake runtime was asked for no suspension: `ok
  step2-the-red-starts-the-implementer-while-the-testers-turn-runs-on`.
