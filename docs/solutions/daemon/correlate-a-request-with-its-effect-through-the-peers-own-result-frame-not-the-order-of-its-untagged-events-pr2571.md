---
title: "Correlate a request with its effect through the peer's own result frame, never through the order of its untagged lifecycle events"
category: daemon
tags:
  - rpc
  - correlation
  - background-work
  - shimwire
date: 2026-10-31
status: active
module: packages/daemon/internal/supervise
related_issues:
  - "sjawhar/legion#2571"
---

# Correlate a request with its effect through the peer's own result frame, never through the order of its untagged lifecycle events

Extends docs/solutions/daemon/a-prompt-is-delivered-when-the-turn-starts-not-when-the-shim-acknowledges.md.
That document records attribution-by-order as accepted debt, and names the frame that would pay
it. The debt is paid; this is the rule that replaces it.

Attributing a peer's work to your request by order — "the first lifecycle event after my request is
mine" — is correct only while the peer runs your work and nothing else. The moment the peer can
start work of its own (a background job finishing, a timer, a push from a third party), order
credits someone else's turn to your request, and the mistake is invisible: your request is marked
served while its real work has not started, and the turn that does run it answers for nothing.

So before you enable any background or push-initiated work on a peer, convert the correlation:

- Find the frame the peer writes that **names your request's own id**. A lifecycle start and end
  that carry no id cannot do it, however reliable their order.
- Make that frame, not the lifecycle end, the end of the request's life, and hold the request open
  while the frame is outstanding.
- Keep the outstanding-request id in memory only if a reconnect has a cheaper way to recover — ask
  the peer rather than guess — and say so where the field is declared.
- Every adapter that stands in for the peer must emit the same frame. A second backend behind the
  same protocol that does not write it leaves the request open for ever, which is the same stall
  reached from the other side.

## Evidence

- `packages/daemon/internal/shimwire/frames.go`'s `PromptResult` is the only frame of Oh My Pi's
  RPC that names the prompt a turn belonged to; `AgentStart` and `AgentEnd` carry no prompt
  identity at all.
- `packages/daemon/internal/supervise/machine.go:431` `awaitingResult` holds the pending delivery
  whose prompt is still outstanding, and `supervise/delivery.go`'s settle keeps the turn open while
  it is set, so a turn a background result started retires nothing.
- `packages/daemon/internal/claudebridge/bridge.go` writes the same frame behind the `agent_end` of
  the turn its prompt ran, because the Claude Code backend speaks the same worker stream; without
  it the daemon would wait on a result that never comes.
- The failure this replaced: with background work on, a role's own finished job wakes a turn the
  daemon never prompted. The daemon confirmed its task on that turn's start and retired it as
  served at its end, so the role was never prompted for the task again.
