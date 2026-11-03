---
title: "An Oh My Pi extension's own first turn runs from a command-line slash word, sent before any inbound lane opens"
category: legion
tags:
  - oh-my-pi
  - session-start
  - startup
  - extension-command
  - envoy
date: 2026-10-28
status: active
module: packages/pi-legion
---

# An Oh My Pi extension's own first turn runs from a command-line slash word, sent before any inbound lane opens

- Never start a turn from a `session_start` handler that a person must watch or interrupt. Oh My
  Pi holds every send such a handler makes and runs them at the end of its startup, so the turn
  runs before the TUI draws it or reads input: no busy row, Enter takes no text, Esc does nothing.
- To run an extension's work after startup, register an extension command and have the launcher
  end Oh My Pi's command line with it as a positional word (`/your-command`). Oh My Pi runs that
  word as a prompt once its TUI listens, and the command's handler does the work there.
- A launcher that passes such a word refuses a stdin that is not a terminal, before any side
  effect: with a positional word and no terminal, Oh My Pi runs headless print mode instead.
- Send the session's own first message before the call that opens an inbound lane (an Envoy role
  claim, a notice subscription). The far side can deliver on the lane before that call returns,
  so a send after it resolves can land behind a card.
- While the command runs, Oh My Pi still reads the terminal, so the person can run it again.
  Chain every step that re-registers with a server or rotates a secret it holds through one
  promise, and say once that a queued run is waiting.

## Evidence

- `packages/daemon/internal/daemon/controller.go:28` (`ControllerStartCommand`, the word) and
  `packages/daemon/cmd/legion/controller.go:40` (`stdinIsTerminal`, the refusal before the probe
  and mint).
- `packages/pi-legion/src/controller-session.ts`: the start message goes out after the
  registration and before `claimEnvoyRole`; `serialised` (line 101) chains the claims.
- Before this change the operator's controller claimed and sent at `session_start`; on the real
  binary its whole start turn ran inside the startup wait. Sending after the role claim instead
  failed the "card published as soon as the role is claimed" case 4 of 30 runs. Two claims typed
  during one another overlapped and the second secret was refused (`invalid controller secret`).
