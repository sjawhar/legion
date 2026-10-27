---
title: Claude Code plugin
parent: legion
depends_on: [envoy-client, contracts]
paths: [packages/claude-envoy]
---
The Claude Code channel plugin that delivers Envoy and Dispatch traffic into Claude Code sessions. Its session hooks add context at every session start, resume, compaction and subagent start: the session's open asks, the `dispatch-first` skill, and what people wrote to the session in the last 72 hours, read from Dispatch's `GET /api/v1/agents/{session_id}/answers` through `@legion/envoy-client/session-answers` within 800 ms of the hook's start, a subagent reading its parent's. A test runs the Claude Code release `.claude-code-pin` pins against a stand-in model and Dispatch.
