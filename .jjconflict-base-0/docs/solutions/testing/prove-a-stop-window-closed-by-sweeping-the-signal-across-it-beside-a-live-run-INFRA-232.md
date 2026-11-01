---
title: "Prove a stop window closed by sweeping the signal across it beside a live run"
category: testing
tags:
  - signals
  - trap
  - race
  - shell
  - kubernetes
date: 2026-10-25
status: active
module: docs/kubernetes.md
related_issues:
  - "INFRA-232"
  - "sjawhar/legion#2195"
---

# Prove a stop window closed by sweeping the signal across it beside a live run

When a change claims that a script's clean-up handles a stop at any moment, one stop at one
moment proves almost nothing: the window can be only a few tens of milliseconds wide.

1. **Make the window deterministic first.** Put a `kill -TERM -- -$$` fixture line exactly where
   the window is (for example, right after the call whose answer the next line reads). This proves
   the branch the clean-up takes there.
2. **Then sweep the real signal across the window.** Start the script detached (`setsid -f`),
   wait for its first line, and send the signal at a series of offsets that spans the window
   (here 0.10 s to 0.45 s, in 0.01 s steps near the edges). Record for each offset which branch
   ran: trap not yet installed, nothing to clean, cleaned by the fallback, cleaned by the normal
   path. Count the hits inside the window, and refine the sweep around them until there are some.
3. **Run the sweep while a second live run is going.** A clean-up that deletes by a selector
   must leave the other run alone. List what is left after each stop, and let the live run finish
   with its own success line.
4. **Run the same fixture on the code before the fix as the negative control.** It must leave
   behind what the fix removes.
5. **Treat the offsets as relative.** Changing the script moves the window. Round 3 added a
   `python3` call before the create, and the window moved from about 0.23 s (round 2) to about
   0.33 s, so sweep again after every change.

## Evidence

sjawhar/legion#2195, round 3, with run A going the whole time:

- The fixture stop gave `deleting by label`, and the stopped run's Job was deleted. Run A's Job
  stayed `Running` and ended `sequence: exit 0`.
- Of 26 timed stops: at 0.10 s nothing was printed (before the trap); from 0.15 s to 0.32 s, and
  at 0.34 s, the label found nothing; at 0.33 s and 0.35 s the label deleted a Job (inside the
  create); later offsets deleted by name.
- The same fixture on round 2's block left its Job running.
- The tester repeated the sweep on its own: 0.316 s found nothing, 0.318 s deleted a Job.

Related: docs/solutions/testing/widen-the-contender-count-before-calling-a-race-unreproducible.md
covers widening the number of contenders. This document covers moving the signal across a window.
