---
title: "A session_switch handler's outcome goes in an extension widget, because /new resets the transcript after the handlers return"
category: legion
tags:
  - oh-my-pi
  - session-switch
  - widget
  - notify
date: 2026-10-28
status: active
module: packages/pi-legion
---

# A session_switch handler's outcome goes in an extension widget, because /new resets the transcript after the handlers return

- Show what a `session_switch` handler decided with `ui.setWidget(key, lines)`, not
  `ui.notify`. Oh My Pi's `/new` awaits every handler and only then resets the transcript and the
  scrollback, so a notice shown from the handler is wiped before anyone reads it.
- Do not defer the notice to a timer instead: with a subagent view focused, `/new` first returns
  to the main session, and that replay can yield to file writes and `setImmediate` before the
  reset, so a timer's notice can still land before it. Widgets survive the reset and the return.
- Record which session a widget describes, and take it down on that session's next run, on a
  later success, and when a switch lands on a session it does not describe (a `/resume` back to the
  session that is still registered included). A widget left up describes the wrong session.

## Evidence

- `d42147b4` deferred the notices to a timer; the tester's focused-subagent case still lost them.
  `9037d3c9` moved them to the widget `legion-controller-claim`.
- `bbc7410b` fixed review round 4's regression: a refusal widget shown for a new session stayed up
  after `/resume` back to the registered session, saying wakes and `set_status` fail there.
  `packages/pi-legion/src/controller-session.ts` `reclaimAfterSessionChange` now clears it before
  its early return, and `handleAgentStart` clears it at the described session's next run.
