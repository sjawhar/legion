---
title: "A real-binary TUI case gives startup its own deadline and waits for the subagent's own request, not the task-label request"
category: testing
tags:
  - oh-my-pi
  - real-binary
  - pseudo-terminal
  - flaky-tests
  - subagent
date: 2026-10-28
status: active
module: packages/pi-legion
---

# A real-binary TUI case gives startup its own deadline and waits for the subagent's own request, not the task-label request

Extends docs/solutions/testing/the-next-real-binary-legion-case-is-one-runlegionpane-call-answer-runs-before-the-gateway-one-transcript-per-pane-and-a-subagents-side-turns-LEGION-634.md, and corrects one detail of it: on Oh My Pi 18.12.2 the task-label request does carry a system prompt.

- Drive an interactive Oh My Pi through a pseudo-terminal and read the screen from a headless
  xterm of the same size (`spawnInteractive` in `packages/pi-shared/test/omp-harness.ts`). Type
  text, wait until the composer shows it, then write `\r`.
- Give the host's startup its own budget: each wait's deadline is
  `max(now + step, spawnedAt + startup)`, so the first wait absorbs a slow startup and every later
  wait stays tight. Set the case timeout above the sum.
- Before you write a matcher for a subagent's request, log every held request with the start of
  its `system` prompt. The task-label request carries the delegated task text and the system
  prompt `# Task\nLabel the delegated work …`, and it is sent at spawn, before the subagent's
  session is attached. The subagent's own request carries the agent prompt.
- Focus a subagent view from the Agent Hub only after the subagent's own first request arrived:
  Enter before that reaches a running entry with no session ("cannot be revived").

## Evidence

- `9613f45a`: one 30 s deadline per wait timed out every case once startup took about 30 s in the
  pod; the separate startup deadline (`controller-start-omp.test.ts:91-97`) fixed it.
- `5fd0f13e`: Enter before the subagent's first request failed 2 of 5 runs. Its wait matched the
  label request and still failed 1 of 6; `6d51373b` excludes the label prompt, and a diagnostic run
  showed the matched request's system prompt is the agent prompt (10 of 10 passes after).
