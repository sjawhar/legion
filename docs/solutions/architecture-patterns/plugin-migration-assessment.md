---
title: "oh-my-opencode → opencode-legion Migration Assessment"
date: 2026-02-15
category: architecture-patterns
tags: [plugin, migration, oh-my-opencode, opencode-legion]
---

# oh-my-opencode → opencode-legion Migration Assessment

## Current State

`opencode-legion` (packages/opencode-plugin/) already has:
- 12 agents with model overlays
- Delegation system (background tasks, category routing)
- 11 hooks (compaction, todos, stop-continuation, session recovery, thinking validation, etc.)
- Tools (session, task, delegation)
- Config system
- Model overlays (Claude/GPT/Gemini)

## What's Needed Before the Switch

### Critical Path: Delegation Hardening

10-task plan at `.sisyphus/plans/delegation-hardening.md` covering:
- File-based task persistence (survive crashes)
- Push-based parent notification (not polling)
- Concurrency limiting (5/model, 15 global)
- Stale timeouts (30 min)
- Retry with model fallback
- Startup reconciliation

Oracle/Ultrabrain review found 5 HIGH race conditions that must be fixed in the plan before implementation. Feedback provided at `~/.agent-mail/delegation-hardening-feedback.md`.

### Needed: Skill-MCP Manager Port

oh-my-opencode gates MCP tools behind skills — tools only load into context when the skill is invoked. OpenCode natively loads ALL MCP tools into every LLM call with no conditional loading. Without this, 10 configured MCPs would bloat every context window.

The port is ~500 lines of real logic: connection management (stdio + HTTP), per-skill tool scoping, idle cleanup. The key architectural decision: tools are keyed by `(sessionID, skillName, serverName)` and only surfaced to the LLM when the skill is active.

### Needed: Tool Metadata Store

~100-line shim that preserves custom metadata (especially `sessionId`) through OpenCode's `fromPlugin()` wrapper which overwrites all metadata. Critical for TUI task progress tracking.

Alternatively, this could be fixed in OpenCode itself — the `fromPlugin()` metadata overwrite is arguably a bug.

## Not Needed

### Skill-MCP Manager (Connection Layer)

OpenCode's native MCP support is a superset (stdio + HTTP + SSE, full OAuth, CLI management). The connection/lifecycle management doesn't need porting. Only the skill-scoping layer does.

### Builtin Commands

8 commands in oh-my-opencode. Most are prompt templates that can be ported incrementally or kept via superpowers. Not blocking the switch.

### Boulder State

Single-user, single-plan work continuity tracker. The current `todo-continuation-enforcer` hook partially covers this. A team task system (closer to Claude Code Teams) is the long-term replacement, but not blocking the switch since interactive development still works with the existing hook.

## Separate Plugins (Unchanged)

- **superpowers** — skills like TDD, systematic-debugging, brainstorming. Stays.
- **compound-engineering** — frontend-design, agent-browser, etc. Stays.

These are independent of the oh-my-opencode → opencode-legion migration.
