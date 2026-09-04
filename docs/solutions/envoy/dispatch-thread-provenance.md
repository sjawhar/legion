---
title: "Dispatch threads carry session provenance from the shim, not the server"
category: envoy
tags:
  - dispatch
  - pi-envoy
  - envoy-client
  - provenance
  - tmux
date: 2026-09-04
status: active
module: envoy-client
problem_type: architecture_pattern
applies_when:
  - "A stateless server needs per-session context (cwd, host app, terminal location) that only the calling session knows"
  - "Adding a field to the dispatch tool's origin block"
  - "A dashboard needs to send a human back to the agent session that asked a question"
---

## Context

The dispatch MCP tool is served by a stateless Go server shared by every agent on the
machine. It authenticates per request and has no session context at all: not the caller's
working directory, not which coding-agent host is running, not where that process lives in
the human's terminal. Yet the whole point of the Dispatch dashboard is answering a question
without hunting for the session that asked it, which needs exactly that context.

## Guidance

Put session-derived defaults in the **shim**, not the server or the model.

The stdio shim (`packages/envoy-client/src/dispatch-mcp-shim.ts`) is the one component that
runs inside the session's own process and working directory, and all three hosts (OMP,
OpenCode, Claude) spawn it. On a `tools/call` for `dispatch` it injects what the model
cannot know and should not be trusted to state:

- `repo` from the cwd's GitHub remote (`jj git remote list`, then `git remote get-url origin`)
- `origin: { host, machine, cwd, tmux, pane }` from the environment

The server validates and stores; the model supplies only intent (`subject`, `context`,
`question`). Injection happens only when the field is absent, so an explicit `repo` still
wins, and a qualified `parent` (`owner/name#n`) suppresses repo injection entirely.

### Record a stable identifier next to the readable one

`tmux display-message -p '#S:#I.#P'` yields `main:3.0`, which reads well and is the obvious
choice. It is also unreliable as a target: when several sessions form a tmux **session
group** they share windows, so the same pane reports a different session name between two
calls (`dev3:4.5`, then `dev4:4.5`), and a `select-window` against one of those names does
not move the human's attached client anyway.

Capture both in one call — `'#S:#I.#P #{pane_id}'` — and use each for what it is good at:
the `session:window.pane` string for display, the pane id (`%840`) for the action
(`tmux switch-client -t %840`), which lands the human's own client on the right session,
window, and pane from wherever they are attached.

### Validate marker-sourced values narrowly before building a command

The origin block lives in the issue body, so anyone with write access to the repo can edit
it. Any value the dashboard turns into a copyable command must be validated against the
narrowest pattern that describes it — a pane id is `^%\d+$`, full stop — rather than a
general "looks safe" character filter. Values that fail validation are still displayed as
escaped text; they just do not get the one-click action.

## Why This Matters

Provenance in the shim survives everything above it. The model cannot forget it, an agent on
a different host gets the same fields, and the server stays a stateless request handler that
any client can call. Deriving it server-side is impossible (no session), and asking the model
for it produces confident guesses.

## When to Apply

Any tool where the useful default is a property of the calling process rather than of the
request: working directory, terminal location, host application, machine identity. Put it in
the local shim, mark it "injected by the shim; leave unset" in the tool schema, and let the
server treat it as data it stores rather than data it derives.
