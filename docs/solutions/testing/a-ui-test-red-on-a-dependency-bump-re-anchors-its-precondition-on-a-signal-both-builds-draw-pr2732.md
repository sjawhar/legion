---
title: "A real-binary UI test red on a dependency bump: re-anchor its precondition on a signal both builds draw"
category: testing
tags:
  - dependency-bump
  - tui
  - terminal-tests
  - preconditions
  - evidence
date: 2026-11-03
status: active
module: packages/pi-legion
related_issues:
  - "sjawhar/legion#2732"
---

# A real-binary UI test red on a dependency bump: re-anchor its precondition on a signal both builds draw

A test that drives the real binary through a terminal goes red on a bump for two different reasons, and they take opposite actions: the product changed, which goes back as a defect, or the test watched chrome the release moved, which the test follows. Separate them from the screen before you touch either.

- **A string present in the binary is not a code path that runs.** Count the **call sites** that post it, not the occurrences of the text. Then read the screen the harness printed at the timeout, which says what the view actually reached.
- **Read the release's own changelog for the sentence that explains it**, and quote it in the comment you leave. A deliberate removal names its replacement; an accidental one does not.
- **Move only the precondition.** The assertion and every deadline stay. A precondition is how the case reaches the state it tests, and it is the only part a chrome change may move.
- **Anchor on a signal both builds draw, and prove it** by running the changed case against the old build too. A probe that only the new build satisfies tests the release rather than the behaviour, and it will red again on the next bump for the same reason.
- **Pair the wait with an absence control in the same case**, asserting the signal is missing before the action. Then passing is a change of state rather than a string the view always carries. A probe without it can pass on a view that never changed.
- **Parse a generated token off the surface rather than hard-coding it or widening to a bare marker**, and keep the parse fail-closed: a token the parse gets wrong must leave the wait timing out, never passing. Anchor the parse on stable structure and never on an animated glyph.
- Write in the comment which release moved the chrome and why, so the next reader re-reads the screen instead of widening the probe.

## Evidence

The Oh My Pi pin bump in sjawhar/legion#2732 reddened one case of `packages/pi-legion/extensions/controller-start-omp.test.ts`, "the operator sees a refused re-claim after /new from a focused subagent view": it timed out after 150,712.88 ms on `omp/18.16.0` and passed on `omp/18.13.3`.

The first diagnosis read "the Agent Hub has closed, and `Viewing agent` is in both binaries (3 matches each), so this is a flow change". Both halves were wrong, and the screen said so. The screen printed at the timeout is of a **focused** view: its status line reads `👻 StuckCobra > ⬢ Stand-in > 📁 …`, its body holds the subagent's own conversation, and its panel reads `Subagents · 1 running`. So `alt+a` then Enter had attached the subagent's session exactly as before.

Counting call sites rather than occurrences settled the rest: `omp/18.13.3` carries one `showStatus` call for `Viewing agent …` and `omp/18.16.0` carries none, though the text survives in both. The release's own changelog says why: "Viewing a subagent or returning to the main session no longer posts a 'Viewing agent …' / 'Returned to main session' status (a popup in Tern); the status line and Tern's composer already show which agent is in view."

Commit `df90fcc1d749` therefore moved the precondition alone. The case reads the name Oh My Pi generated for the subagent off the Subagents panel's row (`  └─ ⟳ StuckCobra Reply with done.`) as the last token before the assignment, anchored on `└─` and the assignment and never on the spinner frame, which changes between frames; it accepts the token only when it begins with an ASCII letter, so a half-drawn row keeps the poll going rather than yielding a wrong name. It asserts `expect(session.terminal.screen()).not.toContain(focusedView)` before `alt+a`, and waits for that breadcrumb after Enter. The `alt+n`, the wait for the second registration and the three strings `New session started`, `re-claiming the controller for this session failed` and `capability revoked` did not move, and no deadline was raised.

The case then passed three times on `omp/18.16.0` (7.61 s, 7.44 s, 7.43 s), once on `omp/18.13.3` (7.38 s), and the whole file was 16 pass, 0 fail in 171.27 s.

Two things the next reader should know. The probe depends on the Subagents row keeping `└─` and on the ghost marker staying in the status line's focused-agent segment; a release that redraws either reds this case again, and the answer then is to read the screen again rather than to widen the parse. And a pull-request body that becomes the merge commit's message must not leave two live contradictory diagnoses of one failure: this one carries both, with the first marked superseded.
